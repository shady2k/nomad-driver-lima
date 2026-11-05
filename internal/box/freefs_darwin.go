//go:build darwin

package box

import "syscall"

// FreeBytes is the space available to an unprivileged writer on the filesystem
// that holds dir (see the linux build for why a bake needs it).
func FreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
