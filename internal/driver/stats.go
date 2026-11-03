package driver

import (
	"context"
	"os"
	"path/filepath"
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
func (d *Driver) streamStats(ctx context.Context, h *taskHandle, interval time.Duration, out chan *drivers.TaskResourceUsage) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case <-ticker.C:
			inst := h.currentInstanceDir()
			if inst == "" {
				continue
			}
			usage, err := h.stats.sample(d.compute, vmPIDs(inst))
			if err != nil {
				d.logger.Debug("collect box stats", "task", h.tc.ID, "error", err)
				continue
			}
			select {
			case out <- usage:
			case <-ctx.Done():
				return
			case <-h.done:
				return
			}
		}
	}
}
