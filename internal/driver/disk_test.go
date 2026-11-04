package driver

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// writeDisk makes a sparse disk file of virtualBytes, of which written bytes
// really occupy the host, and returns it as an instance.
func writeDisk(t *testing.T, home, boxID string, virtualBytes int64, written int) *limactl.Instance {
	t.Helper()
	dir := filepath.Join(home, boxID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "disk")
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(virtualBytes))
	if written > 0 {
		_, err = file.Write(make([]byte, written))
		require.NoError(t, err)
	}
	require.NoError(t, file.Close())
	return &limactl.Instance{Name: boxID, Dir: dir, Status: limactl.StatusStopped}
}

func TestDiskUsageOf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	require.NoError(t, os.WriteFile(path, make([]byte, 32*1024), 0o644))

	used, virtual, ok := diskUsageOf(path)
	require.True(t, ok)
	require.Equal(t, int64(32*1024), virtual, "the apparent size is what the disk may grow to")
	require.Greater(t, used, int64(0), "a written file occupies blocks")
	require.LessOrEqual(t, used, virtual, "used never exceeds the apparent size")

	// A missing or non-regular disk is no usage, not an error.
	_, _, ok = diskUsageOf(filepath.Join(dir, "absent"))
	require.False(t, ok)
	_, _, ok = diskUsageOf(dir)
	require.False(t, ok)
}

// writeQcow2 makes a file with a qcow2 header whose virtual size is virtual.
func writeQcow2(t *testing.T, path string, virtual int64, clusters int) {
	t.Helper()
	header := make([]byte, 32)
	copy(header[0:4], "QFI\xfb")
	binary.BigEndian.PutUint32(header[4:8], 3) // format version 3
	binary.BigEndian.PutUint64(header[24:32], uint64(virtual))
	require.NoError(t, os.WriteFile(path, append(header, make([]byte, clusters*512)...), 0o644))
}

func TestDiskUsageOfQcow2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "box-box1.qcow2")
	// A disk that may grow to 8 GiB but occupies a handful of clusters today.
	writeQcow2(t, path, int64(8)<<30, 8)

	used, virtual, ok := diskUsageOf(path)
	require.True(t, ok)
	require.Equal(t, int64(8)<<30, virtual, "the header says how big the disk is")
	require.Less(t, used, int64(1<<20), "the file has barely grown")
	require.Less(t, used, virtual, "used is the blocks, not the disk size")

	// A file that is not a qcow2 falls back to its apparent size.
	raw := filepath.Join(dir, "raw.img")
	require.NoError(t, os.WriteFile(raw, make([]byte, 64*1024), 0o644))
	_, rawVirtual, ok := diskUsageOf(raw)
	require.True(t, ok)
	require.Equal(t, int64(64*1024), rawVirtual)
}

func TestBoxDiskUsages(t *testing.T) {
	home := t.TempDir()
	// A running box with data, a stopped box with its disk still on the volume,
	// and an instance whose disk is gone (a volume that was deleted).
	writeDisk(t, home, "box1", 8<<30, 256*1024)
	writeDisk(t, home, "box2", 4<<30, 64*1024)
	gone := &limactl.Instance{Name: "box3", Dir: filepath.Join(home, "box3")}
	require.NoError(t, os.MkdirAll(gone.Dir, 0o755))

	disks := boxDiskUsages([]*limactl.Instance{
		{Name: "box1", Dir: filepath.Join(home, "box1")},
		{Name: "box2", Dir: filepath.Join(home, "box2")},
		gone,
		nil,
		{Name: "", Dir: home},
	})
	require.Len(t, disks, 2, "only instances with a disk count")
	require.Equal(t, "box1", disks[0].BoxID)
	require.Equal(t, "box2", disks[1].BoxID)
	require.Equal(t, int64(8<<30), disks[0].VirtualBytes)
	require.Greater(t, disks[0].UsedBytes, int64(0))
	require.Less(t, disks[0].UsedBytes, disks[0].VirtualBytes,
		"a qcow2 that has done little is far smaller than the disk it may become")
}

// TestFingerprintReportsDiskUsage is the attribute the coordinator reads.
func TestFingerprintReportsDiskUsage(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)

	// Give the box disk some content, and add a stopped instance by hand: both
	// belong in the numbers.
	diskPath := filepath.Join(vol, "box-box1.qcow2")
	file, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	_, err = file.Write(make([]byte, 128*1024))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	// A stopped box is an ordinary instance in lima's table, and its disk is
	// still on its volume.
	stoppedDir := f.InstanceDir("box2")
	require.NoError(t, os.MkdirAll(stoppedDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stoppedDir, "disk"), make([]byte, 64*1024), 0o644))
	f.SetState(t, "box2", map[string]any{"status": limactl.StatusStopped, "dir": stoppedDir})

	ch, err := d.Fingerprint(context.Background())
	require.NoError(t, err)
	fp := <-ch

	usedBox1 := attrInt(t, fp, "driver.box.disk.box1.used_bytes")
	virtualBox1 := attrInt(t, fp, "driver.box.disk.box1.virtual_bytes")
	require.Greater(t, usedBox1, int64(128*1024), "the written bytes occupy the host")
	require.GreaterOrEqual(t, virtualBox1, int64(128*1024), "the apparent size includes what was written")
	// A tiny fully allocated file can occupy a block more than its size; the
	// interesting case (a sparse 8 GiB disk that occupies a few MiB) is covered
	// by TestBoxDiskUsages.
	require.Greater(t, virtualBox1, int64(0))

	usedBox2 := attrInt(t, fp, "driver.box.disk.box2.used_bytes")
	require.Greater(t, usedBox2, int64(0), "a stopped box still occupies its disk")

	require.Equal(t, usedBox1+usedBox2, attrInt(t, fp, "driver.box.disk.used_bytes"),
		"the node total is the sum over every box disk")
	require.Equal(t, virtualBox1+attrInt(t, fp, "driver.box.disk.box2.virtual_bytes"),
		attrInt(t, fp, "driver.box.disk.virtual_bytes"))

	// The totals are always published, even on a node without a single box, so
	// a reader never has to guess whether a missing attribute means zero.
	bare := &drivers.Fingerprint{Attributes: map[string]*structs.Attribute{}}
	setDiskUsage(bare, nil)
	require.Equal(t, "0", attrValue(t, bare, "driver.box.disk.used_bytes"))
	require.Equal(t, "0", attrValue(t, bare, "driver.box.disk.virtual_bytes"))
}

// attrInt reads a string fingerprint attribute as a number.
func attrInt(t *testing.T, fp *drivers.Fingerprint, key string) int64 {
	t.Helper()
	value := attrValue(t, fp, key)
	n, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err, "attribute %q = %q is not a number", key, value)
	return n
}

// TestFingerprintClearsTheKeysOfAGoneBox: Nomad keeps a node attribute the
// driver stops sending, so the fingerprint after a box is gone sends its keys
// empty once, which makes Nomad drop them, and the one after names them no more.
func TestFingerprintClearsTheKeysOfAGoneBox(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	dir := f.InstanceDir("gone")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk"), make([]byte, 4096), 0o644))
	f.SetState(t, "gone", map[string]any{"status": limactl.StatusStopped, "dir": dir})

	fp := d.buildFingerprint(context.Background())
	require.NotEmpty(t, attrValue(t, fp, "driver.box.disk.gone.used_bytes"))
	require.Equal(t, "Stopped", attrValue(t, fp, "driver.box.instance.gone.state"))

	f.RemoveInstance(t, "gone")
	fp = d.buildFingerprint(context.Background())
	for _, key := range []string{
		"driver.box.disk.gone.used_bytes", "driver.box.disk.gone.virtual_bytes",
		"driver.box.instance.gone.state", "driver.box.instance.gone.task",
		"driver.box.instance.gone.cpus", "driver.box.instance.gone.memory_mib",
		"driver.box.instance.gone.disk_gib",
	} {
		attr, ok := fp.Attributes[key]
		require.True(t, ok, "%s is sent to be cleared", key)
		v, _ := attr.GetString()
		require.Empty(t, v, key)
	}
	require.Equal(t, "0", attrValue(t, fp, "driver.box.disk.used_bytes"), "a total is never cleared")

	fp = d.buildFingerprint(context.Background())
	for key := range fp.Attributes {
		require.NotContains(t, key, ".gone.", "a cleared key is not sent again")
	}
}
