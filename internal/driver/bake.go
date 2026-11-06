package driver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// A bake makes an image out of a box without emptying the box: the box's disk is
// copied into a Lima clone on the node that holds it, the clone is started with
// the platform bundle and with empty stand-ins for the mounts boxd waits for,
// sealed from inside, stopped and compressed. The box keeps its work; only the
// copy becomes the image.
//
// The task is a task of this driver because the disk, the clone and the image
// cache are on the node, and because a task is what Nomad schedules, reports and
// stops. The CLI stops the box first (a clone of a running instance copies a disk
// that is being written) and starts it again afterwards.

const (
	// bakeBoxdTimeout is how long a bake waits for boxd in the clone after Lima
	// reports the VM up. boxd's own unit waits up to 300s for its mounts.
	bakeBoxdTimeout = 6 * time.Minute
	// bakeSealTimeout bounds the seal of a clone: it deletes, proves, trims and
	// records, and a box with a large home takes minutes.
	bakeSealTimeout = 30 * time.Minute
	// bakeCloneStopTimeout is how long a clone may take to shut down.
	bakeCloneStopTimeout = 3 * time.Minute
)

// bakeCloneGrowth is what a booted clone writes on top of the copy of the box's
// disk before the seal trims it: systemd starts, boxd writes its state, and the
// seal records what it removed. The CLI holds the same number (bakeCloneGrowthBytes),
// because it refuses the same bake before the box is stopped.
const bakeCloneGrowth = int64(1) << 30

// The room a bake's copy of a disk and its image need is measured with these. A
// test replaces them: the answers are about the real filesystems, and a test
// cannot fill one.
var (
	bakeFreeBytes           = box.FreeBytes
	bakeImageCacheFreeBytes = box.FreeBytes
	bakeSameFilesystem      = box.SameFilesystem
)

// bakeRoom is what a bake of one box needs of a node, and what the node has.
//
// The clone is a copy of the box's disk written in the Lima home, and it is booted
// once, so it writes a little more before the seal trims it. The image is another
// disk's worth in the image cache: `qemu-img convert -c` reads the clone's disk and
// writes a compressed qcow2 of at most about that size. When both directories are
// on one filesystem the three numbers come out of one pool; otherwise each
// directory must hold its own.
type bakeRoom struct {
	// Used is what the box's disk occupies: the size of the clone's copy.
	Used int64
	// LimaHome is where the clone's disk is written, ImageCache where the image is.
	LimaHome   string
	ImageCache string
	// LimaFree is the room on the filesystem of LimaHome, ImageCacheFree the room
	// on the image cache's, which is measured only when it has one of its own.
	LimaFree       int64
	ImageCacheFree int64
	// SameFilesystem says both directories are on one filesystem, so both needs
	// come out of LimaFree.
	SameFilesystem bool
}

// copyNeeds is the room the clone takes: the copy of the disk plus what a booted
// clone writes before the seal trims it.
func (r bakeRoom) copyNeeds() int64 { return r.Used + bakeCloneGrowth }

// imageNeeds is the room the image may take: the compressed disk is at most about
// the size of the disk it comes from.
func (r bakeRoom) imageNeeds() int64 { return r.Used }

// problem returns why the bake does not fit on this node, in a sentence, or "" when
// it does. It is the whole decision, so that a test needs to fill no filesystem.
func (r bakeRoom) problem(id string) string {
	const advice = "free space on the node, or bake a box with a smaller disk"
	if r.SameFilesystem {
		if r.LimaFree >= r.copyNeeds()+r.imageNeeds() {
			return ""
		}
		return fmt.Sprintf("this node has %s free on the filesystem of its Lima home %s, where a bake of box %s "+
			"writes both the copy of its disk and the image in %s, and it needs %s for the copy and %s for the "+
			"image: %s", humanSize(r.LimaFree), r.LimaHome, id, r.ImageCache, humanSize(r.copyNeeds()),
			humanSize(r.imageNeeds()), advice)
	}
	if r.LimaFree >= r.copyNeeds() && r.ImageCacheFree >= r.imageNeeds() {
		return ""
	}
	return fmt.Sprintf("this node has %s free on the filesystem of its Lima home %s and %s free on the one of its "+
		"image cache %s, and a bake of box %s needs %s for the copy of its disk and %s for the image: %s",
		humanSize(r.LimaFree), r.LimaHome, humanSize(r.ImageCacheFree), r.ImageCache, id,
		humanSize(r.copyNeeds()), humanSize(r.imageNeeds()), advice)
}

// bakeRoomOf measures what this node has for a bake of one box.
func (d *Driver) bakeRoomOf(used int64) (bakeRoom, error) {
	opts := d.boxm.Options()
	room := bakeRoom{Used: used, LimaHome: opts.LimaHome, ImageCache: opts.ImageCacheDir}
	free, err := bakeFreeBytes(room.LimaHome)
	if err != nil {
		return room, err
	}
	room.LimaFree = free
	same, err := bakeSameFilesystem(room.LimaHome, room.ImageCache)
	if err != nil {
		return room, err
	}
	room.SameFilesystem = same
	if same {
		return room, nil
	}
	if room.ImageCacheFree, err = bakeImageCacheFreeBytes(room.ImageCache); err != nil {
		return room, err
	}
	return room, nil
}

// bakeImage runs a bake: clone, start, seal, stop, compress, and delete the clone.
func (d *Driver) bakeImage(ctx context.Context, h *taskHandle) *drivers.ExitResult {
	b := h.cfg.Bake
	if d.boxm == nil {
		return imageWorkFailure(h, d, "the box driver is not configured on this node")
	}
	// The steps go to the task's own log as well as to its events: Nomad keeps
	// only the last ten task events, and a seal prints more than that, so the log
	// is what `nomad alloc logs` and `box bake` read back.
	log := h.workLog()
	defer log.close()
	step := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		log.line(line)
		d.emit(h.tc, "bake", line)
	}
	fail := func(format string, args ...any) *drivers.ExitResult {
		return imageWorkFailure(h, d, fmt.Sprintf(format, args...))
	}
	id := h.cfg.BoxID

	// The box must be a stopped instance of this node's Lima home. The CLI stops
	// it before it registers this job; a box that is running here is a bake that
	// would copy a disk while the guest writes it.
	src, err := d.boxm.Lima().Get(ctx, id)
	if err != nil {
		return fail("cannot look for box %s on this node: %v", id, err)
	}
	if src == nil {
		return fail("this node has no Lima instance named %s: a bake runs on the node that holds the box's disk, "+
			"and the box %s is not there", id, id)
	}
	if src.IsRunning() {
		return fail("box %s is running on this node, and a bake copies a stopped box: `box stop %s`, then bake it "+
			"again (`box bake` stops it itself and starts it again afterwards)", id, id)
	}

	// The clone runs the node's platform bundle: boxd and boxctl are in it, and the
	// seal proves the image with the bundle's own guest/no-secrets.sh.
	p := h.cfg.Platform
	if p == nil || p.URL == "" || p.SHA256 == "" {
		return fail("the bake of box %s carries no platform block: the clone runs the bundle's boxd and boxctl, and "+
			"the seal runs the bundle's guest/no-secrets.sh, so a bake needs the bundle its box mounts", id)
	}
	platformDir, err := d.boxm.EnsurePlatform(ctx,
		box.PlatformSpec{Version: p.Version, URL: p.URL, SHA256: p.SHA256},
		func(kind, message string) { d.emit(h.tc, kind, message) })
	if err != nil {
		return fail("cannot put the platform bundle on this node: %v", err)
	}

	// A clone a killed bake left behind goes first: it holds a copy of this box's
	// disk, and nothing will ever seal it. Its room would make the check below
	// refuse a bake that fits.
	left, err := d.boxm.BakeClones(ctx, id)
	if err != nil {
		return fail("cannot look for clones of box %s on this node: %v", id, err)
	}
	for _, name := range left {
		stale := d.boxm.BakeCloneOf(name)
		if err := stale.Delete(context.WithoutCancel(ctx)); err != nil {
			return fail("a previous bake of box %s left the clone %s behind, and it cannot be removed: %v. "+
				"Remove it on the node with `limactl delete %s` as the driver's runtime user", id, name, err, name)
		}
		step("removed the clone %s that a previous bake of box %s left behind", name, id)
	}
	// Both the copy and the image need room: a clone is a full copy of the disk
	// file, and the compression writes a compressed disk of its own.
	if used, ok := box.DiskUsed(h.diskPath); ok {
		room, err := d.bakeRoomOf(used)
		if err != nil {
			step("cannot measure the free space on this node (%v); cloning and compressing anyway", err)
		} else if problem := room.problem(id); problem != "" {
			return fail("%s", problem)
		}
	}
	name := box.NextBakeCloneName(id, left)

	clone, err := d.boxm.CloneForBake(ctx, id, name, platformDir)
	if err != nil {
		return fail("%v", err)
	}
	// Whatever happens next -- a failed step, a killed task -- the clone goes: it
	// is a copy of the box's disk, and it is the machine the seal empties.
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		// It goes to the task's log, not to its events: the last event of a failed
		// bake must be the failure, or `box bake` reports the cleanup instead.
		if err := clone.Delete(cleanCtx); err != nil {
			line := fmt.Sprintf("the clone %s could not be deleted: %v; remove it on the node with `limactl delete %s` "+
				"as the driver's runtime user", name, err, name)
			log.line(line)
			d.emit(h.tc, "exit", line)
			return
		}
		log.line(fmt.Sprintf("deleted the clone %s: the copy of the box's disk is gone, and the box itself was never touched", name))
	}()
	step("cloned box %s into %s: a copy of its stopped disk, with the platform bundle at %s and empty stand-ins for "+
		"the task files and the credentials", id, name, box.PlatformMountPoint)

	if err := clone.Start(ctx); err != nil {
		return fail("%v. The box %s is untouched: it starts again with its work (`box start %s`)", err, id, id)
	}
	step("started the clone %s", name)
	waitCtx, cancelWait := context.WithTimeout(ctx, bakeBoxdTimeout)
	err = clone.WaitBoxd(waitCtx, bakeBoxdTimeout)
	cancelWait()
	if err != nil {
		return fail("the clone %s did not come up: %v. The box %s is untouched: it starts again with its work "+
			"(`box start %s`)", name, err, id, id)
	}
	step("boxd answers in the clone: sealing it into the image %s", b.Version)

	sealCtx, cancelSeal := context.WithTimeout(ctx, bakeSealTimeout)
	err = clone.Seal(sealCtx, box.CloneSeal{
		Name:    b.Name,
		Version: b.Version,
		Parent:  b.Parent,
		Arch:    b.Arch,
		Recipe:  recipeScript(b.Recipe),
	}, func(line string) {
		log.line("  " + line)
		d.emit(h.tc, "seal", line)
	})
	cancelSeal()
	if err != nil {
		return fail("the seal of box %s failed in the clone: %v. The clone is deleted, and the box %s is untouched: "+
			"it starts again with its work (`box start %s`)", id, err, id, id)
	}

	if err := clone.Stop(ctx, bakeCloneStopTimeout); err != nil {
		return fail("%v. The box %s is untouched: it starts again with its work (`box start %s`)", err, id, id)
	}
	step("stopped the clone %s; compressing its disk into the image cache", name)

	rec := box.BakedRecord{Version: b.Version, Arch: b.Arch, Parent: b.Parent, Name: b.Name, Recipe: b.Recipe}
	out, err := d.boxm.Bake(ctx, clone.Disk(), rec, func(_ int, note string) {
		d.emit(h.tc, "bake", note)
	})
	if err != nil {
		var missing *box.QEMUImgMissing
		if errors.As(err, &missing) {
			return fail("qemu-img is not installed on this node (%v), and a bake needs it to compress the disk of "+
				"the clone into an image. Install qemu-img on the node and bake again", missing.Err())
		}
		if ctx.Err() != nil {
			return fail("the bake of %s was stopped; its temporary file is gone", b.Version)
		}
		return fail("the bake of %s failed: %v. The box %s is untouched: it starts again with its work (`box start %s`)",
			b.Version, err, id, id)
	}
	d.emitData(h.tc, "bake", fmt.Sprintf("Baked the image %s %s: sha256 %s, %s compressed, the disk inside it %s; "+
		"a box on this node creates from it without a download",
		out.Version, out.Arch, out.SHA256, humanSize(out.Size), humanSize(out.VirtualBytes)),
		map[string]string{
			"version":       out.Version,
			"arch":          out.Arch,
			"sha256":        out.SHA256,
			"size":          fmt.Sprintf("%d", out.Size),
			"virtual_bytes": fmt.Sprintf("%d", out.VirtualBytes),
			"name":          out.Name,
			"parent":        out.Parent,
		})
	return &drivers.ExitResult{}
}

// recipeScript is the recipe's script out of the base64 the job carries, or nil
// when the image has none. A recipe that does not decode is not passed on: the
// seal only needs it to record what makes the environment.
func recipeScript(encoded string) []byte {
	if encoded == "" {
		return nil
	}
	script, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return script
}
