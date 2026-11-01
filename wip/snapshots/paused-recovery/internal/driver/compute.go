package driver

import (
	"fmt"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

// Keep exactly the scheduler's scale. Never silently repair upstream CPU detection
// in a driver: that would give admission and accounting different units.
func nomadComputePerCore(total, cores uint64) (int, error) {
	if total == 0 || cores == 0 {
		return 0, fmt.Errorf("Nomad compute topology unavailable")
	}
	n := total / cores
	if total%cores != 0 {
		n++
	}
	if n > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("Nomad compute value overflows host integer")
	}
	return int(n), nil
}
func metricAttributes(perCore int) map[string]*pstructs.Attribute {
	return map[string]*pstructs.Attribute{
		"driver.lima.compute.ready":                     pstructs.NewBoolAttribute(perCore > 0),
		"driver.lima.metrics.memory.scope":              pstructs.NewStringAttribute("host_vm_process_rss"),
		"driver.lima.metrics.memory.guest_usage":        pstructs.NewBoolAttribute(false),
		"driver.lima.metrics.memory.complete_vm_memory": pstructs.NewBoolAttribute(false),
		"driver.lima.metrics.cpu.scope":                 pstructs.NewStringAttribute("host_vm_process"),
		"driver.lima.metrics.cpu.percent_basis":         pstructs.NewStringAttribute("100_percent_per_core"),
		"driver.lima.metrics.cpu.ticks_basis":           pstructs.NewStringAttribute("nomad_agent_compute"),
		"driver.lima.cpu.nomad_compute_per_core":        pstructs.NewIntAttribute(int64(perCore), ""),
	}
}

// Division/subtraction avoid overflow from adversarial task configuration values.
func reservationCovers(cpus, perCore, guestMiB, overheadMiB, cpuReserved, memoryReserved int64) bool {
	if cpus < 1 || perCore < 1 || guestMiB < 1 || overheadMiB < 1 || cpuReserved < 1 || memoryReserved < overheadMiB {
		return false
	}
	return cpus <= cpuReserved/perCore && guestMiB <= memoryReserved-overheadMiB
}
