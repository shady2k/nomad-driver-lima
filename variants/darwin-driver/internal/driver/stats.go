package driver

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/shirou/gopsutil/v3/process"
)

// Process RSS/CPU are not guest utilization or complete VM physical memory.
// VZ includes its in-process hostagent but may account guest RAM elsewhere.
func cpuPercent(previous, current float64, elapsed time.Duration) (float64, error) {
	if elapsed <= 0 || current < previous || math.IsNaN(current) || math.IsInf(current, 0) {
		return 0, fmt.Errorf("invalid CPU sample")
	}
	return (current - previous) / elapsed.Seconds() * 100, nil
}

func (d *Driver) TaskStats(ctx context.Context, id string, interval time.Duration) (<-chan *cstructs.TaskResourceUsage, error) {
	perCore, _ := d.admissionCompute()
	if perCore < 1 {
		return nil, fmt.Errorf("Nomad compute topology pending; no synthetic CPU ticks")
	}
	t, err := d.get(id)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	v, err := d.owned(ctx, &s)
	if err != nil {
		return nil, err
	}
	if v.DriverPID <= 0 || v.Status != "Running" {
		return nil, fmt.Errorf("running Lima VM process identity unavailable")
	}
	p, err := process.NewProcess(int32(v.DriverPID))
	if err != nil {
		return nil, err
	}
	command, err := p.CmdlineWithContext(ctx)
	if err != nil || !strings.Contains(command, s.Instance) {
		return nil, fmt.Errorf("VM process command identity unavailable or mismatched")
	}
	birth, err := p.CreateTimeWithContext(ctx)
	if err != nil {
		return nil, err
	}
	first, err := p.TimesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	lastCPU, lastTime := first.Total(), time.Now()
	if interval < time.Second {
		interval = time.Second
	}
	ch := make(chan *cstructs.TaskResourceUsage, 1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(ch)
		timer := time.NewTicker(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			case <-t.done:
				return
			case <-timer.C:
			}
			owned, e := d.owned(ctx, &s)
			if e != nil || owned.DriverPID != v.DriverPID || owned.Status != "Running" {
				return
			}
			fresh, e := process.NewProcess(int32(v.DriverPID))
			if e != nil {
				return
			}
			created, e := fresh.CreateTimeWithContext(ctx)
			if e != nil || created != birth {
				return
			}
			cpu, e := p.TimesWithContext(ctx)
			if e != nil {
				return
			}
			mem, e := p.MemoryInfoWithContext(ctx)
			if e != nil {
				return
			}
			now := time.Now()
			percent, e := cpuPercent(lastCPU, cpu.Total(), now.Sub(lastTime))
			if e != nil {
				return
			}
			lastCPU, lastTime = cpu.Total(), now
			resource := &cstructs.ResourceUsage{
				MemoryStats: &cstructs.MemoryStats{RSS: mem.RSS, Measured: []string{"RSS"}},
				CpuStats:    &cstructs.CpuStats{Percent: percent, TotalTicks: percent / 100 * float64(perCore), Measured: []string{"Percent", "TotalTicks"}},
			}
			usage := &cstructs.TaskResourceUsage{Timestamp: now.UnixNano(), ResourceUsage: resource, Pids: map[string]*cstructs.ResourceUsage{strconv.Itoa(v.DriverPID): resource}}

			select {
			case ch <- usage:
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			case <-t.done:
				return
			}
		}
	}()
	return ch, nil
}
