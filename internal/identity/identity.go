// Package identity resolves the unprivileged user every Lima child process of
// the box driver runs as.
//
// Lima refuses to run as root, and a Nomad client on a Linux node runs as root.
// The driver therefore starts limactl (and the systemd user scope around it) as
// an existing unprivileged user: dropped uid/gid, that user's supplementary
// groups (kvm is one of them), and an explicit environment, so nothing of the
// root client's environment reaches a VM.
package identity

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Identity is the unprivileged user that owns everything Lima touches.
//
// The zero value is not usable; use Resolve.
type Identity struct {
	// Name is the login name of the user.
	Name string
	// UID and GID are the user's own ids.
	UID int
	GID int
	// Home is the user's home directory. It becomes HOME for every child, so a
	// child never sees the home of the root client that started it.
	Home string
	// Groups are the user's group ids, including the primary one. They are only
	// used when the driver runs as root and has to drop privileges.
	Groups []uint32
	// Drop is true when the calling process runs as root: every child then runs
	// with this user's uid, gid and groups instead of the driver's.
	Drop bool
}

// Resolve returns the identity the driver runs its children as.
//
// name is the driver's runtime_user setting:
//
//   - empty: the current user, and only when the driver is not root (root has no
//     unprivileged identity to fall back to, and Lima refuses to run as root).
//   - a name: an existing local user that is not root. A driver that does not run
//     as root can only use its own user, because it cannot change user.
//
// The role does not create users and the driver does not change ownership of
// anything it did not create: an unusable identity is an error, never a repair.
func Resolve(name string) (*Identity, error) {
	name = strings.TrimSpace(name)
	root := os.Geteuid() == 0
	if name == "" {
		if root {
			return nil, fmt.Errorf("the driver runs as root and Lima refuses to run as root: set runtime_user in the plugin \"box\" block to an existing unprivileged user (the owner of the Lima home and the image cache)")
		}
		current, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("resolve the current user: %w", err)
		}
		return fromUser(current, false)
	}
	found, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("runtime_user %q is not a local user: %w", name, err)
	}
	id, err := fromUser(found, root)
	if err != nil {
		return nil, err
	}
	if id.UID == 0 {
		return nil, fmt.Errorf("runtime_user %q is root: Lima refuses to run as root, so runtime_user must be an existing unprivileged user", name)
	}
	if !root && id.UID != os.Geteuid() {
		return nil, fmt.Errorf("runtime_user %q is not the user the driver runs as (%s): only a driver that runs as root can change user, so leave runtime_user empty or set it to %s", name, currentName(), currentName())
	}
	return id, nil
}

// Current is the identity of the calling process, whatever it is. Unlike Resolve
// it does not refuse root: a component that may run as root and only needs the
// identity to give files away falls back to itself.
func Current() *Identity {
	uid, gid := os.Geteuid(), os.Getegid()
	id := &Identity{Name: "uid " + strconv.Itoa(uid), UID: uid, GID: gid, Home: os.Getenv("HOME")}
	if u, err := user.Current(); err == nil {
		id.Name = u.Username
		if u.HomeDir != "" {
			id.Home = u.HomeDir
		}
	}
	return id
}

// fromUser builds the identity of one passwd entry.
func fromUser(u *user.User, root bool) (*Identity, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("user %q has an unusable uid %q: %w", u.Username, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, fmt.Errorf("user %q has an unusable gid %q: %w", u.Username, u.Gid, err)
	}
	id := &Identity{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir, Drop: root}
	if !root {
		return id, nil
	}
	// The supplementary groups matter: /dev/kvm is opened through the kvm group
	// on many nodes, and a dropped uid that lost its groups cannot start a VM.
	groups, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("read the groups of %q: %w", u.Username, err)
	}
	ids := make([]uint32, 0, len(groups))
	for _, g := range groups {
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("user %q has an unusable group id %q: %w", u.Username, g, err)
		}
		ids = append(ids, uint32(n))
	}
	id.Groups = ids
	return id, nil
}

func currentName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "uid " + strconv.Itoa(os.Geteuid())
}

// String describes the identity for logs and fingerprint attributes.
func (i *Identity) String() string {
	if i == nil {
		return ""
	}
	return fmt.Sprintf("%s (uid %d, gid %d)", i.Name, i.UID, i.GID)
}

// Env is the complete environment of every child the driver starts as this
// user: an explicit HOME and LIMA_HOME, a PATH that finds lima and qemu, and on
// Linux the user's runtime directory, which systemd-run --user needs.
func (i *Identity) Env(limaHome string) []string {
	env := []string{
		"PATH=" + childPath(),
		"HOME=" + i.Home,
		"LIMA_HOME=" + limaHome,
	}
	if runtime.GOOS == "linux" {
		run := "/run/user/" + strconv.Itoa(i.UID)
		env = append(env,
			"XDG_RUNTIME_DIR="+run,
			"DBUS_SESSION_BUS_ADDRESS=unix:path="+run+"/bus",
		)
	}
	return env
}

// basePath is the PATH every child starts with.
const basePath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// childPath is the PATH of every child: the standard directories first, then
// the directories of the driver's own PATH. The second part is what lets lima
// find qemu on a host that keeps it outside /usr/bin (a nix store, /opt/lima).
func childPath() string {
	seen := make(map[string]bool)
	var parts []string
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" || !filepath.IsAbs(dir) || seen[dir] {
			return
		}
		seen[dir] = true
		parts = append(parts, dir)
	}
	for _, dir := range strings.Split(basePath, ":") {
		add(dir)
	}
	for _, dir := range strings.Split(os.Getenv("PATH"), ":") {
		add(dir)
	}
	return strings.Join(parts, ":")
}

// Chown gives path to the identity. It does nothing when the driver already runs
// as that user: a process cannot give a file away, and does not have to.
//
// Symlinks are changed, not their targets: the disk symlink of a box lives in the
// Lima home, the disk itself on the volume, and both belong to the user.
func (i *Identity) Chown(path string) error {
	if i == nil || !i.Drop {
		return nil
	}
	if err := os.Lchown(path, i.UID, i.GID); err != nil {
		return fmt.Errorf("give %s to %s: %w", path, i.Name, err)
	}
	return nil
}

// ChownTree is Chown for a directory and everything below it.
func (i *Identity) ChownTree(root string) error {
	if i == nil || !i.Drop {
		return nil
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return i.Chown(path)
	})
}

// Owns reports whether path already belongs to the identity.
func (i *Identity) Owns(path string) (bool, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	uid := statUID(st)
	if uid < 0 {
		return false, fmt.Errorf("cannot read the owner of %s on this platform", path)
	}
	return uid == i.UID, nil
}

// EnsureDir creates dir and its parents with mode and gives every level it
// creates to the identity. An existing directory keeps its content and gets the
// same owner and mode, so a Lima home or an image cache created before the
// driver was configured is usable as soon as the driver starts.
//
// Each new level belongs to the runtime user, not only the last one: a parent of
// a Lima home is a directory limactl walks as that user, and one left to the
// driver stops every limactl call. Measured with a lima_home of
// <dir>/lima whose parent did not exist yet: os.MkdirAll made the parent 0700
// and root's, and the fingerprint failed with "cannot evaluate symlinks in
// <dir>/lima: permission denied". A level that is already there keeps its owner:
// /var/lib is not the driver's to give away.
func (i *Identity) EnsureDir(dir string, mode os.FileMode) error {
	dir = filepath.Clean(dir)
	levels, err := missingDirs(dir)
	if err != nil {
		return err
	}
	// From the nearest existing level down, so every parent is in place and
	// belongs to the user the next level is for.
	for k := len(levels) - 1; k >= 0; k-- {
		if err := i.makeDir(levels[k], mode); err != nil {
			return err
		}
	}
	// The leaf is set even when it existed already. Mkdir applies the umask, so
	// the mode is set explicitly.
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}
	return i.Chown(dir)
}

// missingDirs returns the levels of dir that do not exist, deepest first, down to
// the nearest one that does.
func missingDirs(dir string) ([]string, error) {
	var levels []string
	for path := dir; ; {
		_, err := os.Lstat(path)
		if err == nil {
			return levels, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("look at %s: %w", path, err)
		}
		levels = append(levels, path)
		parent := filepath.Dir(path)
		if parent == path {
			return levels, nil
		}
		path = parent
	}
}

// makeDir creates one level, gives it its mode and hands it to the identity.
//
// A level that appeared between the Lstat of missingDirs and this call is one
// another part of the driver made, and it is given the same owner and mode as if
// this call had made it.
func (i *Identity) makeDir(dir string, mode os.FileMode) error {
	if err := os.Mkdir(dir, mode); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}
	return i.Chown(dir)
}
