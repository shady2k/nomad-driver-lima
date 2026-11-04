package box

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// The files Nomad renders for a box task -- the job's local/ files and the
// executor credentials in secrets/ -- reach the guest as two read-only mounts
// of a directory on the host, not as a copy into the guest. The driver puts the
// files there before the VM boots, so they are in the guest from its first
// second, a guest that reboots by itself has them again, and a re-render is
// visible at once. The directory is on the host's tmpfs: no credential is on a
// disk, neither the host's nor the box's.
//
//	<FilesRoot>/<box>/local    -> <FilesDirInGuest>/local   (default /etc/box/local)
//	<FilesRoot>/<box>/secrets  -> <SecretsDirInGuest>       (default /run/box/secrets)

// DefaultFilesRoot is where the task files of every box are on the host: /run
// is a tmpfs on Linux. macOS has none; its temporary directory is the nearest.
func DefaultFilesRoot() string {
	if runtime.GOOS == "linux" {
		return "/run/box-files"
	}
	return filepath.Join(os.TempDir(), "box-files")
}

// defaultFilesDirInGuest is the root the local/ files are mounted below.
const defaultFilesDirInGuest = "/etc/box"

// FilesDir is the host directory of one box's task files.
func (m *Manager) FilesDir(boxID string) string { return filepath.Join(m.opts.FilesRoot, boxID) }

// guestMounts are the two mounts of a box: host directory -> guest path.
func (m *Manager) guestMounts(spec Spec) []limaMount {
	files, secrets := spec.FilesDirInGuest, spec.SecretsDirInGuest
	if files == "" {
		files = defaultFilesDirInGuest
	}
	if secrets == "" {
		secrets = DefaultSecretsDirInGuest
	}
	dir := m.FilesDir(spec.BoxID)
	return []limaMount{
		{Location: filepath.Join(dir, boxLocalDir), MountPoint: filepath.Join(files, boxLocalDir)},
		{Location: filepath.Join(dir, boxSecretsDir), MountPoint: secrets},
	}
}

// StageTaskFiles puts the task's local/ and secrets/ files into the box's host
// directory, which the guest mounts, and returns how many there are. Files
// that are gone from the task are removed; each file is replaced whole, so the
// guest never reads half of one.
func (b *Box) StageTaskFiles(localDir, secretsDir string) (int, error) {
	root := b.m.opts.FilesRoot
	// The root is the driver's; only the runtime user, which runs qemu, reads
	// a box's directory.
	if err := os.MkdirAll(root, 0o711); err != nil {
		return 0, fmt.Errorf("create %s: %w", root, err)
	}
	if err := os.Chmod(root, 0o711); err != nil {
		return 0, fmt.Errorf("set the mode of %s: %w", root, err)
	}
	dir := b.m.FilesDir(b.spec.BoxID)
	if err := b.m.opts.Identity.EnsureDir(dir, 0o700); err != nil {
		return 0, err
	}
	count := 0
	for _, part := range []struct{ name, src string }{{boxLocalDir, localDir}, {boxSecretsDir, secretsDir}} {
		files, err := collectTaskFiles(part.src, part.name)
		if err != nil {
			return count, fmt.Errorf("read the task's %s directory %s: %w", part.name, part.src, err)
		}
		n, err := b.syncFiles(filepath.Join(dir, part.name), files)
		count += n
		if err != nil {
			return count, err
		}
	}
	return count, nil
}

// syncFiles makes dst hold exactly files.
func (b *Box) syncFiles(dst string, files []taskFile) (int, error) {
	id := b.m.opts.Identity
	if err := id.EnsureDir(dst, 0o700); err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for _, f := range files {
		target := filepath.Join(dst, f.rel)
		want[target] = true
		if err := id.EnsureDir(filepath.Dir(target), 0o700); err != nil {
			return 0, err
		}
		data, err := os.ReadFile(f.host)
		if err != nil {
			return 0, fmt.Errorf("read task file %s: %w", f.log, err)
		}
		tmp := target + ".box-new"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return 0, fmt.Errorf("write task file %s: %w", f.log, err)
		}
		if err := id.Chown(tmp); err != nil {
			os.Remove(tmp)
			return 0, err
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return 0, fmt.Errorf("put task file %s in place: %w", f.log, err)
		}
	}
	// What the task no longer has goes, the directories it leaves empty too.
	var dirs []string
	_ = filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dst {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !want[path] {
			_ = os.Remove(path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only an empty one goes
	}
	return len(files), nil
}

// RemoveTaskFiles removes the box's host directory: a stopped box keeps no
// credential on the host.
func (b *Box) RemoveTaskFiles() error {
	return os.RemoveAll(b.m.FilesDir(b.spec.BoxID))
}
