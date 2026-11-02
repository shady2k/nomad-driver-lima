//go:build linux

package box

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// startInstance boots the box inside a transient systemd scope so the qemu
// process and the lima hostagent share one memory limit. Spike S3.d measured
// that memory.max alone is soft because the host swaps: memory.swap.max=0 is
// what makes the cap real. The hostagent is reparented to PID 1 but stays in the
// scope, so the cap survives `limactl start` returning.
//
// A scope that cannot be created fails the task. The earlier fallback ("systemd
// is not reachable, start without a limit") made a misconfigured node look
// healthy while every box on it could take the host's memory; a node without
// cgroup v2 is the operator's problem to declare with memory_limit = "off", and
// the fingerprint reports that decision.
func (m *Manager) startInstance(ctx context.Context, b *Box) (string, error) {
	spec := b.spec
	if m.opts.MemoryLimitOff {
		b.event("cgroup", fmt.Sprintf("memory_limit is \"off\" in the plugin configuration: box %s starts without a memory cgroup (%d MiB guest + %d MiB overhead not enforced)",
			spec.BoxID, spec.MemoryMiB, m.opts.MemoryOverheadMiB))
		return m.startDirect(ctx, b)
	}
	limitBytes := int64(spec.MemoryMiB+m.opts.MemoryOverheadMiB) * 1024 * 1024
	unit := "box-" + spec.BoxID + ".scope"
	// A scope left behind by an earlier, killed driver refuses to start again
	// ("Unit ... was already loaded"), so clear a stale unit first.
	m.resetScope(ctx, unit)

	stderr, err := m.runScope(ctx, unit, limitBytes, spec.BoxID)
	if err != nil && strings.Contains(stderr, "already loaded") {
		b.event("cgroup", fmt.Sprintf("Cgroup unit %s was left behind by an earlier run; stopping it and trying again", unit))
		m.stopScope(ctx, unit)
		time.Sleep(time.Second)
		stderr, err = m.runScope(ctx, unit, limitBytes, spec.BoxID)
	}
	if err != nil && limactlRan(stderr) {
		// The scope was made and limactl ran inside it: the failure is lima's,
		// and its own fatal line says why.
		return "none", fmt.Errorf("start box %s in cgroup %s: %v: %s", spec.BoxID, unit, err, limaFailure(stderr))
	}
	if err != nil {
		return "none", fmt.Errorf("box %s cannot run under the memory cgroup %s (%d MiB, no swap): %v: %s — "+
			"the VM was not started, because a box without its memory limit is not the reservation the task asked for. "+
			"Give the runtime user a working cgroup v2 user manager (systemd-run --user --scope true), "+
			"or set memory_limit = %q in the plugin \"box\" block on a node that has none",
			spec.BoxID, unit, limitBytes/(1024*1024), err, firstLine(stderr), MemoryLimitOff)
	}
	b.event("cgroup", fmt.Sprintf("Box %s runs in cgroup %s: memory.max=%d MiB, memory.swap.max=0", spec.BoxID, unit, limitBytes/(1024*1024)))
	return "systemd-scope", nil
}

// runScope starts limactl inside a fresh scope with the memory cap. The
// command's own stderr comes back with the error: systemd-run says there why
// the scope was refused, and the caller quotes it.
func (m *Manager) runScope(ctx context.Context, unit string, limitBytes int64, boxID string) (string, error) {
	args := []string{
		"--user", "--scope", "--collect", "-q",
		"--unit", unit,
		"-p", "MemoryMax=" + strconv.FormatInt(limitBytes, 10),
		"-p", "MemorySwapMax=0",
		"-p", "OOMPolicy=continue",
		"--", m.opts.Limactl, "start", "--tty=false", boxID,
	}
	cmd := m.childCommand(ctx, m.opts.SystemdRun, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

// systemctl runs a user-level systemctl command and ignores failures. It runs as
// the runtime user, because the scope it touches is that user's scope.
func (m *Manager) systemctl(ctx context.Context, args ...string) {
	cmd := m.childCommand(ctx, "systemctl", append([]string{"--user"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
}

func (m *Manager) resetScope(ctx context.Context, unit string) {
	// reset-failed clears a failed unit so systemd-run can reuse the name.
	m.systemctl(ctx, "reset-failed", unit)
}

// stopScope removes a stale scope, including any process still inside it.
func (m *Manager) stopScope(ctx context.Context, unit string) {
	m.systemctl(ctx, "stop", unit)
	m.systemctl(ctx, "reset-failed", unit)
}

// limactlRan reports whether stderr holds limactl's own log lines, which means
// systemd-run made the scope and the command inside it is what failed.
func limactlRan(stderr string) bool {
	return strings.Contains(stderr, "level=")
}

// limaFailure picks the line of limactl's stderr that says why it failed: the
// last fatal or error line, else the last line.
func limaFailure(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "level=fatal") || strings.Contains(lines[i], "level=error") {
			return strings.TrimSpace(lines[i])
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return "no stderr"
}

func firstLine(s string) string {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "no stderr"
}

// cgroupModeFor reports whether the running VM is inside the box's memory scope.
func (m *Manager) cgroupModeFor(inst *limactl.Instance, boxID string) string {
	if inst == nil || inst.DriverPID <= 0 {
		return "unknown"
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(inst.DriverPID) + "/cgroup")
	if err != nil {
		return "unknown"
	}
	if strings.Contains(string(data), "box-"+boxID+".scope") {
		return "systemd-scope"
	}
	return "none"
}
