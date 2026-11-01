package driver

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Physical memory is a safety ceiling, not a scheduler entitlement. Operators
// should additionally set max_total_memory_mib to host allocatable memory after
// the host/OS reserve; zero never disables this physical ceiling.
func hostMemoryMiB() (int64, error) {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		b, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, fmt.Errorf("host memory unavailable")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("host memory invalid")
		}
		return n / 1048576, nil
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("host memory unavailable")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil && n > 0 {
				return n / 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("host memory invalid")
}

type runtimeCommitment struct{ VMs, CPUs, MemoryMiB int64 }

func (d *Driver) runtimeMemoryCommitment(rows []*State) (int64, error) {
	resources, err := d.runtimeResourceCommitment(rows)
	return resources.MemoryMiB, err
}

func (d *Driver) runtimeResourceCommitment(rows []*State) (runtimeCommitment, error) {
	if d.client == nil || d.ctx == nil {
		return runtimeCommitment{}, fmt.Errorf("runtime inventory unconfigured; memory admission retained")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	instances, err := d.client.List(ctx)
	if err != nil {
		return runtimeCommitment{}, fmt.Errorf("runtime inventory unavailable; memory admission retained")
	}
	known := map[string]bool{}
	var total runtimeCommitment
	for _, v := range instances {
		if known[v.Name] {
			return runtimeCommitment{}, fmt.Errorf("ambiguous runtime inventory")
		}
		known[v.Name] = true
		if v.Status == "Stopped" && v.DriverPID == 0 && v.HostAgentPID == 0 {
			continue
		}
		if v.Memory <= 0 || v.CPUs <= 0 {
			return runtimeCommitment{}, fmt.Errorf("runtime memory unknown; admission retained")
		}
		total.VMs++
		total.CPUs += int64(v.CPUs)
		total.MemoryMiB += (v.Memory+1048575)/1048576 + int64(d.config.MemoryOverheadMiB)
	}
	for _, s := range rows {
		if known[s.Instance] || s.Phase == "no_vm_failed" {
			continue
		}
		purged, err := d.store.purged(s)
		if err != nil {
			return runtimeCommitment{}, err
		}
		if purged {
			continue
		}
		// Absent inventory is not proof of a completed lifecycle. Uncertain
		// reservations remain until stopped/purged reconciliation establishes it.
		// A missing inventory row is not a fresh stopped observation. Even a
		// historical stopped/destroyed label cannot release an uncertain guest.
		if s.MemoryMiB <= 0 || s.CPUs <= 0 {
			return runtimeCommitment{}, fmt.Errorf("uncertain runtime resources unknown; admission retained")
		}
		total.VMs++
		total.CPUs += int64(s.CPUs)
		total.MemoryMiB += int64(s.MemoryMiB + d.config.MemoryOverheadMiB)
	}
	return total, nil
}
