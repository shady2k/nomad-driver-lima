package cli

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// PackTar writes path (a file, a directory or a symlink) to w as a tar stream.
//
// The archive holds exactly one top-level entry, named name, so the receiver
// can unpack it into a directory and get that entry back. Modes, modification
// times and symlinks are preserved; ownership is not, because a copy into a box
// runs as the box's user anyway.
func PackTar(w io.Writer, path, name string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if err := writeTarEntry(tw, path, name, info); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return nil
}

// writeTarEntry writes one path and, for a directory, everything below it.
func writeTarEntry(tw *tar.Writer, path, name string, info os.FileInfo) error {
	mode := info.Mode()
	switch {
	case mode.IsDir():
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name + "/"
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		for _, child := range names {
			childPath := filepath.Join(path, child)
			childInfo, err := os.Lstat(childPath)
			if err != nil {
				return err
			}
			if err := writeTarEntry(tw, childPath, name+"/"+child, childInfo); err != nil {
				return err
			}
		}
		return nil
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return err
		}
		header.Name = name
		return tw.WriteHeader(header)
	case mode.IsRegular():
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err = io.Copy(tw, file)
		return err
	default:
		return fmt.Errorf("%s is not a regular file, a directory or a symlink", path)
	}
}

// UnpackTar extracts a tar stream into dir and returns the top-level names it
// created, sorted.
//
// Absolute entry names and names that leave dir are refused: a tar stream from
// a guest is data, not a licence to write anywhere on this host.
func UnpackTar(r io.Reader, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tr := tar.NewReader(r)
	top := map[string]struct{}{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name, cleanName, err := safeArchivePath(dir, header.Name)
		if err != nil {
			return nil, err
		}
		if cleanName == "" {
			continue
		}
		if first := strings.SplitN(cleanName, "/", 2)[0]; first != "" && first != "." {
			top[first] = struct{}{}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, fs.FileMode(header.Mode)&0o7777|0o700); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return nil, err
			}
			file, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(header.Mode)&0o7777)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(file, tr); err != nil {
				file.Close()
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return nil, err
			}
			if err := os.RemoveAll(name); err != nil {
				return nil, err
			}
			if err := os.Symlink(header.Linkname, name); err != nil {
				return nil, err
			}
		case tar.TypeXGlobalHeader:
			continue
		default:
			// Hard links, devices and FIFOs are refused rather than skipped
			// silently: a copy that loses files is worse than a copy that says
			// it cannot do this.
			return nil, fmt.Errorf("the archive holds %q, which cannot be copied", header.Name)
		}
	}
	out := make([]string, 0, len(top))
	for name := range top {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// safeArchivePath joins an archive entry name onto dir. It returns the path to
// write and the entry name relative to dir.
//
// A ".." element is refused outright, and a leading "/" is dropped: an archive
// is data from a guest, not a licence to write anywhere on this host.
func safeArchivePath(dir, entry string) (fullPath, cleanName string, err error) {
	slash := strings.ReplaceAll(entry, "\\", "/")
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return "", "", fmt.Errorf("the archive tries to write outside %s", dir)
		}
	}
	cleanName = strings.TrimPrefix(path.Clean("/"+slash), "/")
	cleanName = strings.TrimPrefix(cleanName, "./")
	if cleanName == "" || cleanName == "." {
		return "", "", nil
	}
	fullPath = filepath.Join(dir, filepath.FromSlash(cleanName))
	rel, err := filepath.Rel(dir, fullPath)
	if err != nil {
		return "", "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("the archive tries to write outside %s", dir)
	}
	return fullPath, cleanName, nil
}
