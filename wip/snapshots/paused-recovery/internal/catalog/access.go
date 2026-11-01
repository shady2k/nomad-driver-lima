package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Files are checksum-pinned, secret-free boot artifacts (0444). The private
// runtime-owned Lima home and root-owned cache traversal are the access boundary.
// Never repair existing permissions silently; the operator reviews that delta.
func (c Cache) directoryPolicy() (int, os.FileMode, error) {
	gid := os.Getegid()
	mode := os.FileMode(0700)
	if c.Runtime != nil {
		if c.Runtime.UID < 0 || c.Runtime.GID < 0 {
			return 0, 0, fmt.Errorf("invalid cache runtime identity")
		}
		if c.Runtime.UID != os.Geteuid() {
			if os.Geteuid() != 0 {
				return 0, 0, fmt.Errorf("cache cannot grant another runtime identity")
			}
			gid = c.Runtime.GID
			mode = 0710
		}
	}
	return gid, mode, nil
}
func (c Cache) CheckDirectory() error {
	gid, mode, e := c.directoryPolicy()
	if e != nil {
		return e
	}
	st, e := os.Lstat(c.Dir)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(raw.Uid) != os.Geteuid() || int(raw.Gid) != gid || st.Mode().Perm() != mode {
		return fmt.Errorf("cache ownership/traversal repair required; expected driver owner, runtime group and mode %04o; no automatic chmod", mode)
	}
	return nil
}
func (c Cache) prepareDirectory() error {
	if !filepath.IsAbs(c.Dir) {
		return fmt.Errorf("absolute cache directory required")
	}
	gid, mode, e := c.directoryPolicy()
	if e != nil {
		return e
	}
	e = os.Mkdir(c.Dir, 0700)
	if os.IsExist(e) {
		return c.CheckDirectory()
	}
	if e != nil {
		return e
	}
	// This is a newly created leaf only, never recursive chown/chmod of Lima home.
	fd, e := syscall.Open(c.Dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	if os.Geteuid() == 0 {
		if e = syscall.Fchown(fd, os.Geteuid(), gid); e != nil {
			return e
		}
	}
	if e = syscall.Fchmod(fd, uint32(mode)); e != nil {
		return e
	}
	return c.CheckDirectory()
}
func (c Cache) checkFile(path string) error {
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || !ok || int(raw.Uid) != os.Geteuid() || st.Mode().Perm() != 0444 {
		return fmt.Errorf("cache artifact is symlink/mutable or wrong owner; operator inspection required")
	}
	return nil
}
