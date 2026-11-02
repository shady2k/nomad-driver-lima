//go:build darwin

package box

import (
	"context"
	"fmt"

	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// startInstance boots the box. macOS has no cgroups, so the memory budget can
// only be off; the driver's configuration refuses memory_limit = "enforce" on a
// non-Linux host, and this is the second guard for a caller that builds its own
// options.
func (m *Manager) startInstance(ctx context.Context, b *Box) (string, error) {
	spec := b.spec
	if !m.opts.MemoryLimitOff {
		return "none", fmt.Errorf("box %s cannot get a memory cgroup: macOS has no cgroups, so memory_limit must be %q", spec.BoxID, MemoryLimitOff)
	}
	b.event("cgroup", fmt.Sprintf("Box %s starts without a memory cgroup (macOS has no cgroups; %d MiB guest + %d MiB overhead not enforced)", spec.BoxID, spec.MemoryMiB, m.opts.MemoryOverheadMiB))
	return m.startDirect(ctx, b)
}

// cgroupModeFor reports that macOS has no cgroups.
func (m *Manager) cgroupModeFor(inst *limactl.Instance, boxID string) string { return "none" }
