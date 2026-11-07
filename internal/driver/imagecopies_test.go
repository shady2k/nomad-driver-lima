package driver

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

func TestImageCopyExpired(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		low  bool
		want bool
	}{
		{"a fresh copy is kept", time.Hour, false, false},
		{"a copy older than a week goes", 8 * 24 * time.Hour, false, true},
		{"a copy of six days is kept", 6 * 24 * time.Hour, false, false},
		{"a low node keeps one for an hour", 30 * time.Minute, true, false},
		{"a low node deletes one older than an hour", 2 * time.Hour, true, true},
		{"a low node still deletes a week-old copy", 8 * 24 * time.Hour, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, imageCopyExpired(tc.age, tc.low))
		})
	}
}

// imageCacheFixture is a driver with an image cache holding one old and one recent
// uncompressed copy, the verified download they came from, a partial download and
// a platform bundle.
type imageCacheFixture struct {
	driver *Driver
	fake   *fakelima.Fake
	cache  string
	old    string
	recent string
}

func newImageCacheFixture(t *testing.T) *imageCacheFixture {
	t.Helper()
	f := fakelima.New(t)
	cache := filepath.Join(t.TempDir(), "cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	d := newTestDriverWithCache(t, f, filepath.Join(t.TempDir(), "box-files"), cache)

	write := func(name string, age time.Duration) string {
		path := filepath.Join(cache, name)
		require.NoError(t, os.WriteFile(path, make([]byte, 4096), 0o644))
		when := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(path, when, when))
		return path
	}
	fixture := &imageCacheFixture{driver: d, fake: f, cache: cache}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixture.old = write(digest+box.UncompressedSuffix, 8*24*time.Hour)
	fixture.recent = write("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"+box.UncompressedSuffix, time.Hour)
	write(digest+".img", 8*24*time.Hour)
	write(digest+".12345.part", 8*24*time.Hour)
	write(digest+".baked.json", 8*24*time.Hour)
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "platform-1.2.3-linux-amd64"), 0o755))
	return fixture
}

// noBackingFiles answers that no disk of the node is built on an image.
func noBackingFiles(qemuImg, diskPath string) (string, error) { return "", nil }

func TestPruneImageCopies(t *testing.T) {
	fixture := newImageCacheFixture(t)
	previous := diskBackingFile
	diskBackingFile = noBackingFiles
	t.Cleanup(func() { diskBackingFile = previous })

	removed := fixture.driver.pruneImageCopies(false)
	require.Equal(t, []string{filepath.Base(fixture.old)}, removed)
	require.NoFileExists(t, fixture.old)
	require.FileExists(t, fixture.recent, "a copy used within the week is kept")

	// Nothing else of the cache is the cleanup's business.
	entries, err := os.ReadDir(fixture.cache)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Len(t, names, 5, "the download, the partial download, the baked record, the platform bundle and the recent copy: %v", names)
}

// TestPruneImageCopiesOnALowNode: a node below its disk reserve needs the room now,
// so a copy an hour old goes; the same copy on a healthy node stays.
func TestPruneImageCopiesOnALowNode(t *testing.T) {
	fixture := newImageCacheFixture(t)
	previous := diskBackingFile
	diskBackingFile = noBackingFiles
	t.Cleanup(func() { diskBackingFile = previous })

	// On a healthy node only the week-old copy goes...
	removed := fixture.driver.pruneImageCopies(false)
	require.Equal(t, []string{filepath.Base(fixture.old)}, removed)
	require.FileExists(t, fixture.recent)
	// ...and on a node below its reserve the hour-old one goes too.
	removed = fixture.driver.pruneImageCopies(true)
	require.Equal(t, []string{filepath.Base(fixture.recent)}, removed)
	require.NoFileExists(t, fixture.recent)
}

// TestPruneImageCopiesKeepsWhatABoxDiskIsBuiltOn: deleting the file a box disk
// reads from would break a box this driver did not make, so a disk that names one
// keeps it.
func TestPruneImageCopiesKeepsWhatABoxDiskIsBuiltOn(t *testing.T) {
	fixture := newImageCacheFixture(t)
	// A box of the node, whose disk is built on the old copy.
	dir := fixture.fake.InstanceDir("box1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk"), make([]byte, 4096), 0o644))
	fixture.fake.SetState(t, "box1", map[string]any{"status": "Running", "dir": dir})

	previous := diskBackingFile
	diskBackingFile = func(qemuImg, diskPath string) (string, error) { return fixture.old, nil }
	t.Cleanup(func() { diskBackingFile = previous })

	removed := fixture.driver.pruneImageCopies(true)
	require.Equal(t, []string{filepath.Base(fixture.recent)}, removed,
		"the copy a box disk reads from is kept, the one nothing uses is not")
	require.FileExists(t, fixture.old)
}

// TestPruneImageCopiesKeepsEverythingWhenADiskCannotBeRead: a disk whose backing
// file cannot be read is proof of nothing, and any copy may be the one it needs.
func TestPruneImageCopiesKeepsEverythingWhenADiskCannotBeRead(t *testing.T) {
	fixture := newImageCacheFixture(t)
	dir := fixture.fake.InstanceDir("box1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk"), make([]byte, 4096), 0o644))
	fixture.fake.SetState(t, "box1", map[string]any{"status": "Running", "dir": dir})

	previous := diskBackingFile
	diskBackingFile = func(qemuImg, diskPath string) (string, error) { return "", os.ErrPermission }
	t.Cleanup(func() { diskBackingFile = previous })

	require.Empty(t, fixture.driver.pruneImageCopies(true))
	require.FileExists(t, fixture.old)
	require.FileExists(t, fixture.recent)
}

// TestPruneImageCopiesKeepsEverythingWhenAnInstanceCannotBeListed: without the
// list there is no way to know what a disk is built on.
func TestPruneImageCopiesKeepsEverythingWhenAnInstanceCannotBeListed(t *testing.T) {
	fixture := newImageCacheFixture(t)
	previous := diskBackingFile
	diskBackingFile = noBackingFiles
	t.Cleanup(func() { diskBackingFile = previous })

	// A node whose instances cannot be listed might have a disk built on any of
	// the copies, so nothing is deleted.
	state := filepath.Join(fixture.fake.Home, ".fake-state.json")
	require.NoError(t, os.WriteFile(state, []byte("{not json"), 0o644))
	require.Empty(t, fixture.driver.pruneImageCopies(true))
	require.FileExists(t, fixture.old)
	require.FileExists(t, fixture.recent)
}
