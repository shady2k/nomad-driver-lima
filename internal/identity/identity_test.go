package identity

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// currentUser returns the passwd entry of the test process's own user.
func currentUser(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Current()
	require.NoError(t, err)
	return u
}

func TestResolveEmptyUsesCurrentUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process runs as root; the empty runtime_user is an error there")
	}
	id, err := Resolve("")
	require.NoError(t, err)
	u := currentUser(t)
	require.Equal(t, u.Username, id.Name)
	require.Equal(t, os.Geteuid(), id.UID)
	require.Equal(t, os.Getegid(), id.GID)
	require.Equal(t, u.HomeDir, id.Home)
	require.False(t, id.Drop, "a driver that is not root does not drop privileges")
	require.Empty(t, id.Groups, "groups are only needed when dropping privileges")
}

func TestResolveEmptyIsAnErrorAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only a root driver has to name its runtime user")
	}
	_, err := Resolve("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "runtime_user")
	require.Contains(t, err.Error(), "root")
}

func TestResolveRejectsRootUser(t *testing.T) {
	_, err := Resolve("root")
	require.Error(t, err, "Lima refuses to run as root")
	require.Contains(t, err.Error(), "root")
}

func TestResolveRejectsUnknownUser(t *testing.T) {
	_, err := Resolve("box-no-such-user-2f9c")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a local user")
}

func TestResolveOwnUserByName(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root has to name an unprivileged user")
	}
	u := currentUser(t)
	id, err := Resolve(u.Username)
	require.NoError(t, err)
	require.Equal(t, os.Geteuid(), id.UID)
	require.False(t, id.Drop)
}

func TestResolveOtherUserNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may use any existing unprivileged user")
	}
	// Some other existing unprivileged user: only a root driver may use it.
	other := "daemon"
	if u, err := user.Lookup(other); err != nil || u.Uid == strconv.Itoa(os.Geteuid()) {
		t.Skipf("no second unprivileged user (%q) on this host", other)
	}
	_, err := Resolve(other)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only a driver that runs as root")
}

func TestEnvIsExplicit(t *testing.T) {
	id := &Identity{Name: "box", UID: 4242, GID: 4243, Home: "/home/box"}
	env := id.Env("/var/lib/box/lima")
	got := map[string]string{}
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		require.True(t, ok, "env entry %q is not NAME=value", kv)
		got[name] = value
	}
	require.Equal(t, "/home/box", got["HOME"])
	require.Equal(t, "/var/lib/box/lima", got["LIMA_HOME"])
	require.Contains(t, got["PATH"], "/usr/bin")
	if runtime.GOOS == "linux" {
		require.Equal(t, "/run/user/4242", got["XDG_RUNTIME_DIR"])
		require.Equal(t, "unix:path=/run/user/4242/bus", got["DBUS_SESSION_BUS_ADDRESS"])
	}
	// Nothing of the caller's environment is inherited: the root client's
	// secrets must not reach a VM process.
	require.Len(t, env, len(got), "no duplicate or inherited entries")
}

func TestConfigureSetsACredentialOnlyWhenDropping(t *testing.T) {
	plain := &Identity{Name: "box", UID: 4242, GID: 4243, Home: "/home/box"}
	cmd := exec.Command("true")
	plain.Configure(cmd)
	require.Nil(t, cmd.SysProcAttr, "a driver that is not root inherits its own identity")

	dropping := &Identity{Name: "box", UID: 4242, GID: 4243, Groups: []uint32{4243, 302}, Drop: true}
	cmd = exec.Command("true")
	dropping.Configure(cmd)
	require.NotNil(t, cmd.SysProcAttr)
	require.NotNil(t, cmd.SysProcAttr.Credential)
	require.Equal(t, uint32(4242), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(4243), cmd.SysProcAttr.Credential.Gid)
	require.Equal(t, []uint32{4243, 302}, cmd.SysProcAttr.Credential.Groups, "the kvm group travels with the user")
}

func TestChownDoesNothingWithoutDrop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	before, err := os.Stat(path)
	require.NoError(t, err)

	id := &Identity{Name: "box", UID: 4242, GID: 4243}
	require.NoError(t, id.Chown(path))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.Sys(), after.Sys(), "no chown is attempted without root")
}

func TestOwns(t *testing.T) {
	dir := t.TempDir()
	me := &Identity{UID: os.Geteuid(), GID: os.Getegid()}
	owned, err := me.Owns(dir)
	require.NoError(t, err)
	require.True(t, owned)

	other := &Identity{UID: os.Geteuid() + 1}
	owned, err = other.Owns(dir)
	require.NoError(t, err)
	require.False(t, owned)
}

func TestEnsureDirCreatesWithMode(t *testing.T) {
	id := &Identity{Name: "box", UID: os.Geteuid(), GID: os.Getegid()}
	dir := filepath.Join(t.TempDir(), "a", "b")
	require.NoError(t, id.EnsureDir(dir, 0o700))
	st, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, st.IsDir())
	require.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "the umask must not narrow the mode")
}

// TestEnsureDirSetsEveryLevelsMode checks the levels EnsureDir makes on the way
// to the leaf, not only the leaf: a directory the driver creates as the parent of
// a Lima home is walked by limactl, and the umask of the client (077 on a
// hardened node) must not narrow it.
func TestEnsureDirSetsEveryLevelsMode(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	id := &Identity{Name: "box", UID: os.Geteuid(), GID: os.Getegid()}
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "c")
	require.NoError(t, id.EnsureDir(dir, 0o711))
	for _, level := range []string{"a", filepath.Join("a", "b"), filepath.Join("a", "b", "c")} {
		st, err := os.Stat(filepath.Join(root, level))
		require.NoError(t, err, "%s must exist", level)
		require.Equal(t, os.FileMode(0o711), st.Mode().Perm(),
			"%s is a level the runtime user walks, so the umask must not narrow it either", level)
	}
}

// TestEnsureDirGivesEveryLevelToTheIdentity checks the owner of each level. Only
// a root driver hands a directory to another user (without root the identity is
// the caller's own, and Chown does nothing), so this test needs root.
func TestEnsureDirGivesEveryLevelToTheIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a directory to another user needs a root driver")
	}
	u, err := user.Lookup("nobody")
	require.NoError(t, err)
	uid, err := strconv.Atoi(u.Uid)
	require.NoError(t, err)
	gid, err := strconv.Atoi(u.Gid)
	require.NoError(t, err)
	id := &Identity{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir}
	// A parent of the Lima home that does not exist yet: the driver makes it, and
	// it is the level that stopped limactl when it stayed root's.
	root := filepath.Join(t.TempDir(), "box")
	dir := filepath.Join(root, "lima")
	require.NoError(t, id.EnsureDir(dir, 0o700))
	for _, level := range []string{root, dir} {
		owned, err := id.Owns(level)
		require.NoError(t, err)
		require.True(t, owned, "%s must belong to %s: limactl enters every level of its Lima home", level, id.Name)
	}
}
