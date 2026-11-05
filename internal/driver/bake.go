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

// bakeFreeBytes measures the room a bake's copy of a disk needs on this node. A
// test replaces it: the answer is about the real filesystem, and a test cannot
// fill one.
var bakeFreeBytes = box.FreeBytes

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

	// The copy needs room: a clone is a full copy of the disk file.
	if used, ok := box.DiskUsed(h.diskPath); ok {
		free, err := bakeFreeBytes(d.boxm.Options().LimaHome)
		if err != nil {
			step("cannot measure the free space on this node (%v); cloning anyway", err)
		} else if free < used {
			return fail("this node has %s free on the filesystem of its Lima home and the copy of box %s's disk "+
				"needs %s: a bake clones the disk before it seals it. Free space on the node, or bake the box on a "+
				"node with room", humanSize(free), id, humanSize(used))
		}
	}

	// A clone a killed bake left behind is removed here: it holds a copy of this
	// box's disk, and nothing will ever seal it.
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
