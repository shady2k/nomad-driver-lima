package limactl

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/identity"
)

// envOf turns an environment slice into a map.
func envOf(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		require.True(t, ok, "environment entry %q is not NAME=value", kv)
		_, dup := out[name]
		require.False(t, dup, "duplicate environment entry %q", name)
		out[name] = value
	}
	return out
}

func TestCommandEnvIsExplicitAndComplete(t *testing.T) {
	id := &identity.Identity{Name: "box", UID: 4242, GID: 4243, Home: "/home/box"}
	client := &Client{
		Bin:      "/usr/local/bin/limactl",
		Home:     "/var/lib/box/lima",
		Env:      id.Env("/var/lib/box/lima"),
		Identity: id,
	}
	cmd := client.Command(context.Background(), "list", "--format", "json")

	env := envOf(t, cmd.Env)
	require.Equal(t, "/home/box", env["HOME"], "the child must not see the root client's home")
	require.Equal(t, "/var/lib/box/lima", env["LIMA_HOME"])
	require.Contains(t, env["PATH"], "/usr/bin")
	require.NotContains(t, env, "SUDO_USER", "nothing of the caller's environment is inherited")
	if runtime.GOOS == "linux" {
		require.Equal(t, "/run/user/4242", env["XDG_RUNTIME_DIR"])
		require.Equal(t, "unix:path=/run/user/4242/bus", env["DBUS_SESSION_BUS_ADDRESS"])
	}
	require.Equal(t, []string{"/usr/local/bin/limactl", "list", "--format", "json"}, cmd.Args)
}

func TestCommandKeepsTheHomeOfTheClient(t *testing.T) {
	client := &Client{Bin: "limactl", Home: "/srv/lima", Env: []string{"PATH=/bin", "LIMA_HOME=/wrong", "HOME=/root"}}
	cmd := client.Command(context.Background(), "--version")
	env := envOf(t, cmd.Env)
	require.Equal(t, "/srv/lima", env["LIMA_HOME"], "the client's home wins over a stale LIMA_HOME")
	require.Equal(t, "/root", env["HOME"])
}

func TestCommandDropsPrivilegesForTheIdentity(t *testing.T) {
	client := &Client{Bin: "limactl", Home: "/lima"}
	cmd := client.Command(context.Background(), "--version")
	require.Nil(t, cmd.SysProcAttr, "without an identity the child inherits the driver's own identity")

	dropping := &identity.Identity{Name: "box", UID: 4242, GID: 4243, Groups: []uint32{4243, 302}, Drop: true}
	client.Identity = dropping
	cmd = client.Command(context.Background(), "--version")
	require.NotNil(t, cmd.SysProcAttr)
	require.NotNil(t, cmd.SysProcAttr.Credential)
	require.Equal(t, uint32(4242), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(4243), cmd.SysProcAttr.Credential.Gid)
	require.Equal(t, []uint32{4243, 302}, cmd.SysProcAttr.Credential.Groups)
}

func TestRunUsesTheChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell scripts")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "limactl")
	script := "#!/bin/sh\necho \"$LIMA_HOME\"; echo \"$HOME\"; echo \"$PATH\"\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	id := &identity.Identity{Name: "box", UID: os.Geteuid(), GID: os.Getegid(), Home: "/home/box"}
	client := &Client{Bin: bin, Home: "/var/lib/box/lima", Env: id.Env("/var/lib/box/lima"), Identity: id}
	out, _, err := client.Run(context.Background(), "--version")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Equal(t, "/var/lib/box/lima", lines[0])
	require.Equal(t, "/home/box", lines[1])
	require.Contains(t, lines[2], "/usr/bin")
}

// TestLongCallsOutliveTimeout keeps create, start and stop out of Timeout: a
// first boot under vz takes longer than a short call may, and `limactl start`
// cut at Timeout left a box that never started.
func TestLongCallsOutliveTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "limactl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.5\n"), 0o755))
	c := New(bin, filepath.Join(dir, "home"))
	c.Env = []string{"PATH=" + os.Getenv("PATH")}
	c.Timeout = 100 * time.Millisecond

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, "b1"))
	require.NoError(t, c.Create(ctx, "b1", filepath.Join(dir, "b1.yaml")))
	require.NoError(t, c.Stop(ctx, "b1"))
	_, _, err := c.RunLong(ctx, "start", "--tty=false", "b1")
	require.NoError(t, err)

	_, err = c.Version(ctx)
	require.Error(t, err, "a short call is still bounded by Timeout")
}
