package driver

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// Disk usage attributes.
//
// Nomad knows the capacity of the dynamic host volume a box disk lives on, but
// not how much of the host the box really occupies, and a qcow2 grows on demand:
// an 8 GiB disk that has done nothing takes a few hundred MiB. The fingerprint
// therefore publishes the real numbers, per box and for the whole node, from a
// stat of the disk file (cheap enough for the fingerprint period; no qemu-img).
const (
	// diskAttributePrefix names the per-box attributes:
	// driver.box.disk.<box-id>.used_bytes and driver.box.disk.<box-id>.virtual_bytes.
	diskAttributePrefix = "driver.box.disk."
	// diskTotalUsed and diskTotalVirtual are the sums over every box disk of the
	// node, including stopped boxes and orphans: their disks occupy the host all
	// the same.
	diskTotalUsed    = diskAttributePrefix + "used_bytes"
	diskTotalVirtual = diskAttributePrefix + "virtual_bytes"
	// diskFreeBytes is the space free on the filesystem the node writes its box
	// disks to (its Lima home). A bake copies a whole disk there before it seals
	// it, so `box bake` reads this to refuse a copy that does not fit -- with a
	// sentence, before it stops the box.
	diskFreeBytes = diskAttributePrefix + "free_bytes"
	// diskImageCacheFreeBytes is the space free on the filesystem of the image
	// cache, and it is published only when that is a filesystem of its own: a bake
	// writes the image there too, and an absent attribute means the cache is on
	// the filesystem of the Lima home, out of the same room.
	diskImageCacheFreeBytes = diskAttributePrefix + "image_cache_free_bytes"
	// diskTotalBytes is the size of the filesystem the node writes its box disks
	// to. diskReserveBytes is the part of it the node keeps for itself: the room a
	// box, a bake or a cleanup may not take.
	//
	// They are node totals and never name a box, exactly like the two sums above:
	// a reader that looks for per-box disks by prefix must not see them as one.
	diskTotalBytes   = diskAttributePrefix + "total_bytes"
	diskReserveBytes = diskAttributePrefix + "reserve_bytes"
)

// Defaults of the disk reserve: a tenth of the node's filesystem, and never less
// than 20 GiB. A node that sets both to 0 keeps no reserve at all.
const (
	defaultDiskReserveGiB     = 20
	defaultDiskReservePercent = 10
)

// DiskUsage is the host footprint of one box disk.
type DiskUsage struct {
	// BoxID is the box id, which is also the lima instance name.
	BoxID string
	// UsedBytes is the space the disk file really occupies (st_blocks * 512).
	UsedBytes int64
	// VirtualBytes is its apparent size: how big the disk may grow.
	VirtualBytes int64
}

// diskUsageOf stats one disk file.
//
// Used is what the file occupies on the host (st_blocks), virtual is the size the
// disk has: a qcow2 file grows as the guest writes, so its apparent size says
// nothing about the disk, and the header is what knows. Any other format (a raw
// image, or a file whose header cannot be read) falls back to the apparent size.
//
// A missing disk is not an error: a box that never finished creating, or whose
// volume is gone, simply has no usage to report.
func diskUsageOf(path string) (used, virtual int64, ok bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, 0, false
	}
	used = info.Size()
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
		used = int64(st.Blocks) * 512
	}
	virtual = info.Size()
	if size, ok := qcow2VirtualSize(path); ok {
		virtual = size
	}
	return used, virtual, true
}

// qcow2VirtualSize reads the virtual disk size out of a qcow2 header. The
// reading itself is box.Qcow2VirtualSize, which a bake uses as well.
func qcow2VirtualSize(path string) (int64, bool) { return box.Qcow2VirtualSize(path) }

// boxDiskUsages returns the usage of every box disk lima knows about, sorted by
// box id.
//
// Every instance counts, whatever its state: a stopped box keeps its disk on its
// volume, and an instance without a task (an orphan) occupies the host just the
// same. The disk of an instance is <instance dir>/disk, a symlink into the
// volume, so one stat is all this needs.
func boxDiskUsages(instances []*limactl.Instance) []DiskUsage {
	out := make([]DiskUsage, 0, len(instances))
	for _, inst := range instances {
		if inst == nil || inst.Name == "" || inst.Dir == "" {
			continue
		}
		used, virtual, ok := diskUsageOf(filepath.Join(inst.Dir, "disk"))
		if !ok {
			continue
		}
		out = append(out, DiskUsage{BoxID: inst.Name, UsedBytes: used, VirtualBytes: virtual})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BoxID < out[j].BoxID })
	return out
}

// setDiskFree publishes the space free where this node writes its box disks, and
// where it writes their images when that is another filesystem.
//
// It is at most one stat, one comparison and two statfs per fingerprint. A cache on
// the filesystem of the Lima home is not published: a reader then takes the two out
// of one number, which is what the node does.
func setDiskFree(fp *drivers.Fingerprint, limaHome, imageCacheDir string, reserveGiB, reservePercent int64) {
	free, err := box.FreeBytes(limaHome)
	if err != nil {
		return
	}
	fp.Attributes[diskFreeBytes] = structs.NewStringAttribute(strconv.FormatInt(free, 10))
	// The size of the filesystem and the part of it this node keeps for itself,
	// so `box capacity` can say what "low" means instead of guessing it. A
	// filesystem whose size cannot be read still says how much is free: the
	// reserve is then unknown, and an unknown reserve never refuses a box.
	if total, err := box.TotalBytes(limaHome); err == nil && total > 0 {
		fp.Attributes[diskTotalBytes] = structs.NewStringAttribute(strconv.FormatInt(total, 10))
		fp.Attributes[diskReserveBytes] = structs.NewStringAttribute(strconv.FormatInt(diskReserve(total, reserveGiB, reservePercent), 10))
	}
	same, err := box.SameFilesystem(limaHome, imageCacheDir)
	if err != nil || same {
		return
	}
	cacheFree, err := box.FreeBytes(imageCacheDir)
	if err != nil {
		return
	}
	fp.Attributes[diskImageCacheFreeBytes] = structs.NewStringAttribute(strconv.FormatInt(cacheFree, 10))
}

// diskReserve is the room a node keeps free on the filesystem it writes its box
// disks to: the larger of the GiB figure and the share of the filesystem.
//
// A node that sets both to 0 keeps no reserve, and a node whose filesystem size
// is unknown is given the GiB figure only: a share of nothing would be nothing,
// and a node that cannot be measured is not a node with no reserve.
func diskReserve(total, reserveGiB, reservePercent int64) int64 {
	byPercent := int64(0)
	if total > 0 && reservePercent > 0 {
		byPercent = total / 100 * reservePercent
	}
	byGiB := int64(0)
	if reserveGiB > 0 {
		byGiB = reserveGiB << 30
	}
	if byPercent > byGiB {
		return byPercent
	}
	return byGiB
}
