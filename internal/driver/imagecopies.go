package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// The cleanup of the uncompressed copies of images.
//
// EnsureImageDisk makes one uncompressed copy of an image and every box made from
// that image reads it, so a box disk starts with plain clusters (see
// box.EnsureImageDisk). The copy occupies as much as the image itself, and it is
// worth nothing once no disk on the node is built on it: a node whose filesystem is
// full is a node whose boxes freeze in qemu. So an old copy is deleted, and a copy
// becomes old sooner on a node that is below its disk reserve.
//
// Nothing else in the image cache is touched: the verified downloads, the partial
// downloads, the baked records and the platform bundles are the cache.
const (
	// imageCopyMaxAge is how long a copy nothing uses is kept.
	imageCopyMaxAge = 7 * 24 * time.Hour
	// imageCopyLowDiskMaxAge is how long it is kept while the node is below its
	// disk reserve: the node needs the room now, and the next box that needs the
	// copy makes it again.
	imageCopyLowDiskMaxAge = time.Hour
	// imageCopyCleanupPeriod is how often the cleanup runs, and
	// imageCopyStartupDelay is how long after the plugin starts its first pass
	// runs.
	imageCopyCleanupPeriod = time.Hour
	imageCopyStartupDelay  = time.Minute
	// imageCopyBackingTimeout bounds the qemu-img call that reads what a box disk
	// is built on.
	imageCopyBackingTimeout = 20 * time.Second
	// qemuImgBinary is the qemu-img a node has on its PATH when its configuration
	// names none, the same default the image cache uses.
	qemuImgBinary = "qemu-img"
)

// diskBackingFile returns the file a box disk names as its backing file, or "" for
// a disk that has none: a box disk is one flat qcow2 (measured), so this is empty
// for every box this driver makes.
//
// It is a seam: a test cannot run qemu-img, and the answer is about a real file.
var diskBackingFile = qemuImgBackingFile

// qemuImgBackingFile reads a disk's backing file with qemu-img.
func qemuImgBackingFile(qemuImg, diskPath string) (string, error) {
	if qemuImg == "" {
		qemuImg = qemuImgBinary
	}
	ctx, cancel := context.WithTimeout(context.Background(), imageCopyBackingTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, qemuImg, "info", "--output=json", diskPath).Output()
	if err != nil {
		return "", fmt.Errorf("qemu-img info %s: %w", diskPath, err)
	}
	var info struct {
		BackingFile     string `json:"backing-filename"`
		FullBackingFile string `json:"full-backing-filename"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("read qemu-img info of %s: %w", diskPath, err)
	}
	if info.FullBackingFile != "" {
		return info.FullBackingFile, nil
	}
	return info.BackingFile, nil
}

// imageCopyExpired reports whether an uncompressed copy of this age may be
// deleted, given whether the node is below its disk reserve.
func imageCopyExpired(age time.Duration, low bool) bool {
	if low {
		return age > imageCopyLowDiskMaxAge
	}
	return age > imageCopyMaxAge
}

// watchImageCopies runs the cleanup for the life of the plugin. The first pass
// waits a minute: a plugin that has just started is a plugin whose boxes are still
// being adopted, and the room a copy takes is not a minute's worry.
func (d *Driver) watchImageCopies() {
	first := time.NewTimer(imageCopyStartupDelay)
	defer first.Stop()
	select {
	case <-d.ctx.Done():
		return
	case <-first.C:
		d.pruneImageCopies(d.nodeLowOnDisk())
	}
	ticker := time.NewTicker(imageCopyCleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.pruneImageCopies(d.nodeLowOnDisk())
		}
	}
}

// nodeLowOnDisk reports whether the filesystem the boxes write to is below the
// reserve this node keeps for itself. It is the same rule the CLI refuses a box
// on, measured here so the cleanup can act on it.
func (d *Driver) nodeLowOnDisk() bool {
	free, err := box.FreeBytes(d.config.LimaHome)
	if err != nil {
		return false
	}
	total, err := box.TotalBytes(d.config.LimaHome)
	if err != nil || total <= 0 {
		return false
	}
	reserve := diskReserve(total, d.config.DiskReserveGiB, d.config.DiskReservePercent)
	return reserve > 0 && free < reserve
}

// pruneImageCopies deletes the uncompressed image copies that are old enough and
// that no box disk is built on, and returns the names it deleted.
//
// A failure is a log line and never a task error: the room a copy takes is the
// reason to delete it, not a reason to fail anything. What cannot be read is kept:
// an instance whose disk cannot be inspected may be built on any of the copies.
func (d *Driver) pruneImageCopies(low bool) []string {
	if d.boxm == nil {
		return nil
	}
	cache := d.boxm.Options().ImageCacheDir
	entries, err := os.ReadDir(cache)
	if err != nil {
		if !os.IsNotExist(err) {
			d.logger.Warn("cannot read the image cache", "dir", cache, "error", err)
		}
		return nil
	}
	now := time.Now()
	type candidate struct {
		path string
		name string
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), box.UncompressedSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() || !imageCopyExpired(now.Sub(info.ModTime()), low) {
			continue
		}
		candidates = append(candidates, candidate{path: filepath.Join(cache, entry.Name()), name: entry.Name()})
	}
	if len(candidates) == 0 {
		return nil
	}

	// What the boxes on this node are built on. A copy that is a backing file of
	// one of their disks cannot go: every read of the disk would come from it.
	inUse, ok := d.imageCopiesInUse()
	if !ok {
		return nil
	}
	var removed []string
	for _, c := range candidates {
		if inUse[c.path] {
			d.logger.Debug("keeping the uncompressed image a box disk is built on", "image", c.name)
			continue
		}
		if err := os.Remove(c.path); err != nil {
			d.logger.Warn("cannot delete the uncompressed image copy", "image", c.name, "error", err)
			continue
		}
		removed = append(removed, c.name)
	}
	if len(removed) > 0 {
		d.logger.Info("deleted the uncompressed image copies nothing uses any more", "images", removed,
			"low_disk", low, "cache", cache)
	}
	return removed
}

// imageCopiesInUse returns the paths of the uncompressed copies that a disk on
// this node is built on. ok is false when some disk could not be read: the copies
// are then all kept, because any of them may be the one it needs.
func (d *Driver) imageCopiesInUse() (map[string]bool, bool) {
	ctx, cancel := context.WithTimeout(d.ctx, 4*imageCopyBackingTimeout)
	defer cancel()
	instances, err := d.boxm.List(ctx)
	if err != nil {
		d.logger.Debug("cannot list the instances, so no uncompressed image is deleted", "error", err)
		return nil, false
	}
	inUse := map[string]bool{}
	for _, inst := range instances {
		if inst == nil || inst.Dir == "" {
			continue
		}
		disk := filepath.Join(inst.Dir, "disk")
		if _, err := os.Stat(disk); err != nil {
			continue // no disk: nothing of it is built on an image
		}
		backing, err := diskBackingFile(d.qemuImgPath(), disk)
		if err != nil {
			d.logger.Debug("cannot read the image a box disk is built on, so no uncompressed image is deleted",
				"box", inst.Name, "error", err)
			return nil, false
		}
		if backing == "" {
			continue
		}
		if !filepath.IsAbs(backing) {
			backing = filepath.Join(filepath.Dir(disk), backing)
		}
		inUse[filepath.Clean(backing)] = true
	}
	return inUse, true
}

// qemuImgPath is the qemu-img binary this node uses.
func (d *Driver) qemuImgPath() string {
	if d.config.QEMUImg != "" {
		return d.config.QEMUImg
	}
	return qemuImgBinary
}
