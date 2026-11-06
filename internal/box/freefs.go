package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// SameFilesystem reports whether one filesystem holds both directories.
//
// A bake writes two things on a node -- the clone's copy of the disk in the Lima
// home, and the compressed image in the image cache -- and when both are on one
// filesystem they come out of one free-space number, not of two. The device of
// the filesystem a directory is on says which case it is.
//
// A directory that does not exist yet is measured at its nearest existing parent:
// the image cache may not exist before the first image, and the answer is about
// the filesystem the directory is made on.
func SameFilesystem(a, b string) (bool, error) {
	da, err := deviceOf(a)
	if err != nil {
		return false, err
	}
	db, err := deviceOf(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// deviceOf is the device number of the filesystem that holds dir, measured at the
// nearest directory that exists.
func deviceOf(dir string) (uint64, error) {
	existing, err := nearestExistingDir(dir)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(existing)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot read the device of %s", existing)
	}
	// Dev is uint64 on Linux and int32 on macOS; a device number is small and
	// positive on both, so the comparison is the same on either host.
	return uint64(st.Dev), nil
}

// nearestExistingDir walks up from dir to the first path that exists and is a
// directory, and returns it.
func nearestExistingDir(dir string) (string, error) {
	dir = filepath.Clean(dir)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if info.IsDir() {
				return dir, nil
			}
			// A file is on the filesystem of the directory that holds it.
			dir = filepath.Dir(dir)
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// The root of a volume that is not mounted: nothing to measure.
			return "", err
		}
		dir = parent
	}
}
