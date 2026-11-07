package driver

import (
	"context"
	"path/filepath"
	"testing"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// newTestDriverWithReserve is a driver whose node keeps a disk reserve of its own.
func newTestDriverWithReserve(t *testing.T, f *fakelima.Fake, gib, percent int64) *Driver {
	t.Helper()
	d := NewDriver(context.Background(), hclog.NewNullLogger())
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	cfg := Config{
		LimactlPath:        f.Bin,
		LimaHome:           f.Home,
		FilesRoot:          filepath.Join(t.TempDir(), "box-files"),
		ImageCacheDir:      filepath.Join(f.Home, "_images"),
		MemoryOverheadMiB:  150,
		VMType:             "qemu",
		DiskReserveGiB:     gib,
		DiskReservePercent: percent,
	}
	var encoded []byte
	require.NoError(t, base.MsgPackEncode(&encoded, cfg))
	require.NoError(t, d.SetConfig(&base.Config{PluginConfig: encoded}))
	return d
}

func TestDiskReserve(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name      string
		total     int64
		reserveGi int64
		percent   int64
		want      int64
	}{
		{"the default of a small filesystem is the GiB figure", 100 * gib, 20, 10, 20 * gib},
		{"a large filesystem keeps its share", 400 * gib, 20, 10, 40 * gib},
		{"a tenth of a small filesystem is less than the floor", 50 * gib, 20, 10, 20 * gib},
		{"the larger of the two wins", 100 * gib, 1, 10, 10 * gib},
		{"both zero is no reserve at all", 400 * gib, 0, 0, 0},
		{"a percent with no GiB floor", 100 * gib, 0, 10, 10 * gib},
		{"a GiB floor with no percent", 400 * gib, 20, 0, 20 * gib},
		{"a filesystem that cannot be measured keeps the GiB figure", 0, 20, 10, 20 * gib},
		{"a filesystem that cannot be measured and no floor is nothing", 0, 0, 10, 0},
		{"the whole filesystem may be reserved", 50 * gib, 0, 100, 50 * gib},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, diskReserve(tc.total, tc.reserveGi, tc.percent))
		})
	}
}

// TestFingerprintReportsTheDiskRoom: the total and the reserve are what makes a
// node's "low" a number the CLI can check, and they are node totals: a reader that
// looks for per-box disks must not see them as one.
func TestFingerprintReportsTheDiskRoom(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriverWithReserve(t, f, 20, 10)
	fp := d.buildFingerprint(context.Background())

	total := attrInt(t, fp, diskTotalBytes)
	require.Greater(t, total, int64(0), "the filesystem of the Lima home has a size")
	require.Equal(t, diskReserve(total, 20, 10), attrInt(t, fp, diskReserveBytes))
	require.GreaterOrEqual(t, attrInt(t, fp, diskReserveBytes), int64(20)<<30)

	// A node that keeps no reserve publishes the zero: "unknown" and "off" are
	// different answers, and the fingerprint can say the second.
	off := newTestDriverWithReserve(t, fakelima.New(t), 0, 0)
	fp = off.buildFingerprint(context.Background())
	require.Equal(t, "0", attrValue(t, fp, diskReserveBytes))

	// Neither is a box: a per-box key is cleared when its box is gone, and a node
	// total never is.
	for _, key := range []string{diskTotalBytes, diskReserveBytes, diskFreeBytes, diskImageCacheFreeBytes, diskTotalUsed} {
		require.False(t, isPerBoxKey(key), "%s is a node total", key)
	}
	require.True(t, isPerBoxKey(diskAttributePrefix+"box1.used_bytes"))
	require.True(t, isPerBoxKey(instanceAttributePrefix+"box1"+instanceQEMUStateSuffix))
}

// TestDiskReserveIsAbsentWhenTheFilesystemCannotBeMeasured: a directory that does
// not exist says nothing at all, and an absent reserve is "unknown", never a
// healthy zero a reader could take for a node with room.
func TestDiskReserveIsAbsentWhenTheFilesystemCannotBeMeasured(t *testing.T) {
	fp := &drivers.Fingerprint{Attributes: map[string]*structs.Attribute{}}
	setDiskFree(fp, filepath.Join(t.TempDir(), "absent-dir"), filepath.Join(t.TempDir(), "absent-cache"), 20, 10)
	_, free := fp.Attributes[diskFreeBytes]
	require.False(t, free, "a directory that does not exist has no free space to report")
	_, reserve := fp.Attributes[diskReserveBytes]
	require.False(t, reserve)
}
