package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/client/lib/cpustats"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// taskStats turns the resource usage of the VM processes into a Nomad sample.
type taskStats struct {
	mu      sync.Mutex
	tracker *cpustats.Tracker
	// vmPID is the VM process Lima's pid files do not name, and vmSearched when
	// it was last looked for: the search runs lsof, so not on every sample.
	vmPID      int
	vmSearched time.Time
}

// vmSearchInterval is how often a VM process that is not known yet is looked for.
const vmSearchInterval = 30 * time.Second

// hypervisor returns the VM process of the box, or 0 while it is not found. A
// pid that stopped answering is looked for again.
func (t *taskStats) hypervisor(paths []string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.vmPID > 0 {
		if _, err := processUsage(t.vmPID); err == nil {
			return t.vmPID
		}
		t.vmPID = 0
	}
	if time.Since(t.vmSearched) < vmSearchInterval {
		return 0
	}
	t.vmSearched = time.Now()
	t.vmPID = hypervisorPID(paths)
	return t.vmPID
}

func newTaskStats() *taskStats { return &taskStats{} }

// sample reads the qemu (driver) and hostagent processes of the box and returns
// one aggregated usage sample.
func (t *taskStats) sample(compute cpustats.Compute, pids []int) (*drivers.TaskResourceUsage, error) {
	var (
		cpuNs   float64
		rss     uint64
		found   int
		lastErr error
	)
	for _, pid := range pids {
		usage, err := processUsage(pid)
		if err != nil {
			lastErr = err
			continue
		}
		found++
		cpuNs += usage.cpuNs
		rss += usage.rssBytes
	}
	if found == 0 {
		return nil, lastErr
	}

	t.mu.Lock()
	if t.tracker == nil {
		t.tracker = cpustats.New(compute)
	}
	percent := t.tracker.Percent(cpuNs)
	ticks := t.tracker.TicksConsumed(percent)
	t.mu.Unlock()

	return &drivers.TaskResourceUsage{
		Timestamp: time.Now().UTC().UnixNano(),
		ResourceUsage: &cstructs.ResourceUsage{
			MemoryStats: &cstructs.MemoryStats{
				RSS:      rss,
				Measured: []string{"RSS"},
			},
			CpuStats: &cstructs.CpuStats{
				SystemMode: 0,
				UserMode:   cpuNs,
				TotalTicks: ticks,
				Percent:    percent,
				Measured:   []string{"Percent", "TotalTicks", "UserMode"},
			},
		},
	}, nil
}

// vmPIDs returns the pids of the processes that make up the box: the qemu (or
// vz) driver process and the lima hostagent. Lima writes both pid files into the
// instance directory, so this needs no limactl call. With vz the driver pid is
// the hostagent's own, and it is counted once.
func vmPIDs(instanceDir string) []int {
	var pids []int
	seen := map[int]bool{}
	for _, name := range []string{box.DriverPIDFile(box.VMTypeQEMU), box.DriverPIDFile(box.VMTypeVZ), "ha.pid"} {
		data, err := os.ReadFile(filepath.Join(instanceDir, name))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	return pids
}

// streamStats sends a usage sample every interval until ctx ends.
//
// A sample that cannot be taken is said once, as a task event and a warning,
// with its reason: Nomad's graphs stay empty otherwise, and nothing on the
// cluster says why.
func (d *Driver) streamStats(ctx context.Context, h *taskHandle, interval time.Duration, out chan *drivers.TaskResourceUsage) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var said string
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case <-ticker.C:
			usage, err := d.sampleBox(ctx, h)
			if err != nil {
				if msg := err.Error(); msg != said {
					said = msg
					// emit logs it as well.
					if h.tc != nil && h.tc.AllocID != "" {
						d.emit(h.tc, "stats", fmt.Sprintf("Cannot read the resource usage of box %s, so Nomad shows no graphs: %v", h.boxID, err))
					}
				}
				continue
			}
			if said != "" {
				said = ""
				d.logger.Info("reading the box's resource usage again", "task", h.tc.ID, "box", h.boxID)
			}
			select {
			case out <- usage:
			case <-ctx.Done():
				return
			case <-h.done:
				return
			}
			h.mu.Lock()
			first := !h.statsSent
			h.statsSent = true
			h.mu.Unlock()
			if first && h.tc != nil && h.tc.AllocID != "" {
				var rss uint64
				if usage.ResourceUsage != nil && usage.ResourceUsage.MemoryStats != nil {
					rss = usage.ResourceUsage.MemoryStats.RSS
				}
				d.emit(h.tc, "stats", fmt.Sprintf("Nomad has the first resource usage sample of box %s (VM processes %d MiB resident)",
					h.boxID, rss>>20))
			}
		}
	}
}

// sampleBox takes one sample of the box's VM processes. They are found by
// Lima's pid files in the instance directory, and by `limactl list` when the
// files name none.
func (d *Driver) sampleBox(ctx context.Context, h *taskHandle) (*drivers.TaskResourceUsage, error) {
	inst := h.currentInstanceDir()
	if inst == "" {
		return nil, fmt.Errorf("the task does not know its Lima instance directory")
	}
	pids := vmPIDs(inst)
	if len(pids) == 0 && d.boxm != nil {
		pids = d.limaPIDs(ctx, h.boxID)
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("no VM process found: no %s, %s or ha.pid in %s, and limactl lists none",
			box.DriverPIDFile(box.VMTypeQEMU), box.DriverPIDFile(box.VMTypeVZ), inst)
	}
	vmPID := h.stats.hypervisor(boxPaths(inst, h.volumeDir))
	if vmPID > 0 && !slices.Contains(pids, vmPID) {
		pids = append(pids, vmPID)
	}
	// On Linux the VM is the first pid file's qemu; elsewhere the pid files name
	// the hostagent, so only a process that was found counts.
	if vmPID == 0 && runtime.GOOS == "linux" && len(pids) > 0 {
		vmPID = pids[0]
	}
	if h.noteVMPID(vmPID) && h.tc != nil && h.tc.AllocID != "" {
		d.emit(h.tc, "stats", fmt.Sprintf("VM process of box %s: pid %d", h.boxID, vmPID))
	}
	usage, err := h.stats.sample(d.compute, pids)
	if err != nil {
		return nil, fmt.Errorf("read the VM processes %v: %w", pids, err)
	}
	return usage, nil
}

// boxPaths are where a box's disk may be open from: its instance directory, its
// volume, and the volume's real path, which is what lsof names.
func boxPaths(instanceDir, volumeDir string) []string {
	paths := []string{instanceDir, volumeDir}
	if real, err := filepath.EvalSymlinks(volumeDir); err == nil && real != volumeDir {
		paths = append(paths, real)
	}
	return paths
}

// limaPIDs asks limactl for the driver and host agent pids of one box.
func (d *Driver) limaPIDs(ctx context.Context, boxID string) []int {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inst, err := d.boxm.Lima().Get(listCtx, boxID)
	if err != nil || inst == nil {
		return nil
	}
	var pids []int
	for _, pid := range []int{inst.DriverPID, inst.HostAgentPID} {
		if pid > 0 && (len(pids) == 0 || pids[0] != pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}
