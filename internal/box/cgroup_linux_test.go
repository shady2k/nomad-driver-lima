//go:build linux

package box

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopeArgsCarryBothLimits(t *testing.T) {
	args := strings.Join(scopeArgs("box-a.scope", 1<<30, 2, "/bin/limactl", "a"), " ")
	require.Contains(t, args, "-p MemoryMax=1073741824")
	require.Contains(t, args, "-p MemorySwapMax=0")
	require.Contains(t, args, "-p CPUQuota=300%")
	require.True(t, strings.HasSuffix(args, "-- /bin/limactl start --tty=false a"), args)
}

func TestParseCPUMax(t *testing.T) {
	got, ok := parseCPUMax("300000 100000\n")
	require.True(t, ok)
	require.Equal(t, 300, got)
	for _, content := range []string{"max 100000\n", "", "300000", "300000 0"} {
		_, ok := parseCPUMax(content)
		require.False(t, ok, content)
	}
}

// The real path against the host's user manager: the scope gets the limit, the
// driver reads it back, and a unit that has none is reported as not enforced.
func TestCPULimitIsReadBackFromTheScope(t *testing.T) {
	if exec.Command("systemd-run", "--user", "--scope", "-q", "--collect", "true").Run() != nil {
		t.Skip("no systemd user manager here")
	}
	script := filepath.Join(t.TempDir(), "fake-limactl")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 3\n"), 0o755))

	m, err := New(Options{LimaHome: t.TempDir(), SystemdRun: "systemd-run"})
	require.NoError(t, err)
	unit := "box-cputest-" + filepath.Base(t.TempDir()) + ".scope"
	cmd := exec.Command("systemd-run", scopeArgs(unit, 1<<30, 2, script, "x")...)
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Wait() }()

	ctx := context.Background()
	note := ""
	for i := 0; i < 50 && !strings.Contains(note, "cpu.max="); i++ {
		note = m.cpuLimitNote(ctx, unit, 2)
		if !strings.Contains(note, "cpu.max=") {
			_ = exec.Command("sleep", "0.1").Run()
		}
	}
	if !strings.Contains(note, "cpu.max=") && !controllerDelegated(t) {
		t.Skip("this user manager has no cpu controller")
	}
	require.Contains(t, note, "cpu.max=300%")

	require.Contains(t, m.cpuLimitNote(ctx, "box-no-such-unit.scope", 2), "NOT enforced")
}

func controllerDelegated(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	group := strings.TrimPrefix(strings.TrimSpace(string(data)), "0::")
	idx := strings.Index(group, ".service")
	if idx < 0 {
		return false
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup" + group[:idx+len(".service")] + "/cgroup.controllers")
	return err == nil && strings.Contains(string(controllers), "cpu")
}
