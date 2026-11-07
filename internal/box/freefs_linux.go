//go:build linux

package box

import "syscall"

// FreeBytes is the space available to an unprivileged writer on the filesystem
// that holds dir.
//
// A bake's clone needs a copy of the box's disk, so this is what decides whether
// the node can hold one: the answer is measured on the filesystem the Lima home
// is on, which is where `limactl clone` writes the copy.
func FreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// TotalBytes is the size of the filesystem that holds dir: what the node's disk
// reserve is a share of, since a share of nothing is nothing.
func TotalBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Blocks) * int64(st.Bsize), nil
}
