package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackUnpackFile(t *testing.T) {
	src := t.TempDir()
	file := filepath.Join(src, "hello.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello box"), 0o640))

	var buf bytes.Buffer
	require.NoError(t, PackTar(&buf, file, "hello.txt"))

	dst := t.TempDir()
	names, err := UnpackTar(bytes.NewReader(buf.Bytes()), dst)
	require.NoError(t, err)
	require.Equal(t, []string{"hello.txt"}, names)
	data, err := os.ReadFile(filepath.Join(dst, "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello box", string(data))
	info, err := os.Stat(filepath.Join(dst, "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestPackUnpackDirectory(t *testing.T) {
	src := t.TempDir()
	root := filepath.Join(src, "tree")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "deep.txt"), []byte("deep"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "top.txt"), []byte("top"), 0o644))
	require.NoError(t, os.Symlink("top.txt", filepath.Join(root, "link.txt")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o755))

	var buf bytes.Buffer
	require.NoError(t, PackTar(&buf, root, "tree"))

	dst := t.TempDir()
	names, err := UnpackTar(&buf, dst)
	require.NoError(t, err)
	require.Equal(t, []string{"tree"}, names)
	deep, err := os.ReadFile(filepath.Join(dst, "tree", "a", "b", "deep.txt"))
	require.NoError(t, err)
	require.Equal(t, "deep", string(deep))
	target, err := os.Readlink(filepath.Join(dst, "tree", "link.txt"))
	require.NoError(t, err)
	require.Equal(t, "top.txt", target)
	info, err := os.Stat(filepath.Join(dst, "tree", "empty"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestPackTarMissingPath(t *testing.T) {
	err := PackTar(io.Discard, filepath.Join(t.TempDir(), "absent"), "absent")
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))
}

func TestUnpackRefusesEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len("x")), Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	parent := t.TempDir()
	dst := filepath.Join(parent, "inside")
	_, err = UnpackTar(&buf, dst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside")
	_, statErr := os.Stat(filepath.Join(parent, "evil.txt"))
	require.True(t, os.IsNotExist(statErr), "nothing may be written outside the destination")
}

func TestUnpackTreatsAbsoluteNamesAsRelative(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "/etc/passwd", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	dst := t.TempDir()
	names, err := UnpackTar(&buf, dst)
	require.NoError(t, err)
	require.Equal(t, []string{"etc"}, names)
	_, err = os.Stat(filepath.Join(dst, "etc", "passwd"))
	require.NoError(t, err)
}

func TestUnpackRefusesSpecialFiles(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "fifo", Mode: 0o644, Typeflag: tar.TypeFifo}))
	require.NoError(t, tw.Close())
	_, err := UnpackTar(&buf, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot be copied")
}

func TestTreeBytes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("12345"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("123"), 0o644))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, int64(8), treeBytes(dir, info))

	fileInfo, err := os.Stat(filepath.Join(dir, "a"))
	require.NoError(t, err)
	require.Equal(t, int64(5), treeBytes(filepath.Join(dir, "a"), fileInfo))
}
