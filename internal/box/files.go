package box

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
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
// is a tmpfs on Linux. macOS has none (see stageInTaskDir); its temporary
// directory holds only a link per box there.
func DefaultFilesRoot() string {
	if runtime.GOOS == "linux" {
		return "/run/box-files"
	}
	return filepath.Join(os.TempDir(), "box-files")
}

// stageInTaskDir: on a host without a tmpfs (macOS) the files go into the
// task's own private directory, which Nomad keeps on that disk anyway with its
// own copy of the secrets and removes with the allocation, and the box's
// directory is a link to it. A copy anywhere else would be one more place a
// credential stays on the disk after a crash.
var stageInTaskDir = runtime.GOOS != "linux"

// defaultFilesDirInGuest is the root the local/ files are mounted below.
const defaultFilesDirInGuest = "/etc/box"

// FilesDir is the host directory of one box's task files.
func (m *Manager) FilesDir(boxID string) string { return filepath.Join(m.opts.FilesRoot, boxID) }

// guestMounts are the mounts of a box: host directory -> guest path.
//
// platformDir is the unpacked platform bundle, or "" when the box runs the boxd
// its image carries. The bundle is mounted with a 9p cache: it is one immutable
// directory per bundle version, so caching it costs nothing and saves every
// guest read of boxd and of the executors.
func (m *Manager) guestMounts(spec Spec, platformDir string) []limaMount {
	files, secrets := spec.FilesDirInGuest, spec.SecretsDirInGuest
	if files == "" {
		files = defaultFilesDirInGuest
	}
	if secrets == "" {
		secrets = DefaultSecretsDirInGuest
	}
	dir := m.FilesDir(spec.BoxID)
	mounts := []limaMount{
		{Location: filepath.Join(dir, boxLocalDir), MountPoint: filepath.Join(files, boxLocalDir)},
		{Location: filepath.Join(dir, boxSecretsDir), MountPoint: secrets},
	}
	if platformDir != "" {
		mounts = append(mounts, limaMount{
			Location:   platformDir,
			MountPoint: PlatformMountPoint,
			Cache:      CacheFscache,
		})
	}
	return mounts
}

// StageTaskFiles puts the task's local/ and secrets/ files into the box's host
// directory, which the guest mounts, and returns how many there are. Files
// that are gone from the task are removed; each file is replaced whole, so the
// guest never reads half of one.
func (b *Box) StageTaskFiles(localDir, secretsDir, privateDir string) (int, error) {
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
	if stageInTaskDir && privateDir != "" {
		real := filepath.Join(privateDir, "box-files")
		if err := b.m.opts.Identity.EnsureDir(real, 0o700); err != nil {
			return 0, err
		}
		target, err := os.Readlink(dir)
		switch {
		case err == nil && target == real:
			dir = real
		case b.mountInUse(dir):
			// A running guest has this directory mounted, and a share does not
			// follow a replaced directory: it stops reading. The files stay where
			// the guest reads them until the VM stops.
			b.event("files", fmt.Sprintf("Box %s is running with its task files in %s: they stay there until its next start", b.spec.BoxID, dir))
		default:
			_ = os.RemoveAll(dir)
			if err := os.Symlink(real, dir); err != nil {
				return 0, fmt.Errorf("link %s to the task's directory: %w", dir, err)
			}
			_ = b.m.opts.Identity.Chown(dir)
			dir = real
		}
	} else if err := b.m.opts.Identity.EnsureDir(dir, 0o700); err != nil {
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

// mountInUse reports whether dir exists and the box's VM may have it mounted. An
// answer that cannot be had counts as in use: leaving the files where they are
// costs one extra stage, and replacing a mounted directory breaks the box.
func (b *Box) mountInUse(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inst, err := b.Status(ctx)
	if err != nil {
		return true
	}
	return b.m.Alive(inst)
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
func (b *Box) RemoveTaskFiles() error { return b.m.removeFilesDir(b.spec.BoxID) }

// removeFilesDir removes a box's files, and what its link points at.
func (m *Manager) removeFilesDir(boxID string) error {
	dir := m.FilesDir(boxID)
	if target, err := os.Readlink(dir); err == nil {
		_ = os.RemoveAll(target)
	}
	return os.RemoveAll(dir)
}

// PruneTaskFiles removes the files of every box that has no running VM: after
// a crash of the driver nothing else would, and they hold credentials.
func (m *Manager) PruneTaskFiles(running map[string]bool) []string {
	ents, err := os.ReadDir(m.opts.FilesRoot)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range ents {
		if running[e.Name()] {
			continue
		}
		if m.removeFilesDir(e.Name()) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}
