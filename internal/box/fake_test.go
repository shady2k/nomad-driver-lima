package box

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// newTestManager returns a Manager wired to a fake limactl, with the memory
// limit off (a unit test has no systemd user manager to make a scope with).
func newTestManager(t *testing.T, f *fakelima.Fake) *Manager {
	t.Helper()
	m, err := New(Options{
		Limactl:           f.Bin,
		LimaHome:          f.Home,
		ImageCacheDir:     filepath.Join(f.Home, "_image-cache"),
		MemoryOverheadMiB: 150,
		VMType:            VMTypeQEMU,
		MemoryLimitOff:    true,
	})
	require.NoError(t, err)
	return m
}

// imageServer serves a fixed image and returns its URL and sha256.
func imageServer(t *testing.T, payload string) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256([]byte(payload))
	return srv.URL + "/image.img", hex.EncodeToString(sum[:])
}

// imageURL serves payload and counts requests, for cache tests.
func imageURL(t *testing.T, payload string, hits *int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/image.img"
}

// testSpec returns a box spec for box1 in the given volume directory.
func testSpec(t *testing.T, volumeDir string) Spec {
	t.Helper()
	url, sha := imageServer(t, "FAKE-IMAGE")
	return Spec{
		BoxID:       "box1",
		VolumeDir:   volumeDir,
		ImageURL:    url,
		ImageSHA256: sha,
		CPUs:        2,
		MemoryMiB:   1024,
		DiskGiB:     8,
		VMType:      VMTypeQEMU,
	}
}

func volumeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

// writeDisk puts a disk file with recognizable content into the volume.
func writeDisk(t *testing.T, dir, boxID, content string) string {
	t.Helper()
	path := filepath.Join(dir, DiskFileName(boxID))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func symlinkTarget(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	require.NoError(t, err)
	return target
}
