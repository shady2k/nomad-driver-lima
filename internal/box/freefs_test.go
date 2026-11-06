package box

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSameFilesystem: two directories on one filesystem answer true, and a
// directory that does not exist yet is measured at its nearest existing parent.
func TestSameFilesystem(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "lima"), 0o755))

	same, err := SameFilesystem(root, filepath.Join(root, "lima"))
	require.NoError(t, err)
	require.True(t, same, "two directories of one temporary directory are on one filesystem")

	// The image cache does not exist before the first image: it is on the
	// filesystem of the directory it is made in.
	same, err = SameFilesystem(filepath.Join(root, "lima"), filepath.Join(root, "lima", "_image-cache", "baked"))
	require.NoError(t, err)
	require.True(t, same, "an absent directory is measured at its nearest existing parent")

	// A path where a file stands is measured at the directory that holds it.
	file := filepath.Join(root, "disk")
	require.NoError(t, os.WriteFile(file, []byte("QFI\xfb"), 0o644))
	same, err = SameFilesystem(file, root)
	require.NoError(t, err)
	require.True(t, same, "a file is on the filesystem of its directory")
}

// TestSameFilesystemSeesAnotherFilesystem is the case a bake needs: the image
// cache on a filesystem of its own. /dev/shm is one on most Linux hosts, and on a
// host where it is not this test has nothing to compare.
func TestSameFilesystemSeesAnotherFilesystem(t *testing.T) {
	const other = "/dev/shm"
	info, err := os.Stat(other)
	if err != nil || !info.IsDir() {
		t.Skipf("%s is not a directory on this host", other)
	}
	same, err := SameFilesystem(t.TempDir(), other)
	require.NoError(t, err)
	if same {
		t.Skipf("%s is on the same filesystem as %s here", other, os.TempDir())
	}
	require.False(t, same, "%s and a temporary directory are on different filesystems", other)
}
