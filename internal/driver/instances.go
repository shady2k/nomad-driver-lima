package driver

import (
	"sort"
	"strconv"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"

	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// Per-instance attributes.
//
// Nomad accounts for an allocation, not for a VM: a box whose allocation was
// lost, stopped or is being adopted still holds its vCPUs and its memory on the
// node. The fingerprint therefore publishes every live Lima instance with its
// size and whether a running task of this driver owns it, so `box capacity` can
// subtract the VMs Nomad does not account for and show them.
const (
	// instanceAttributePrefix names the per-instance attributes:
	// driver.box.instance.<box-id>.<field>.
	instanceAttributePrefix = "driver.box.instance."
	instanceStateSuffix     = ".state"
	instanceTaskSuffix      = ".task"
	instanceCPUsSuffix      = ".cpus"
	instanceMemorySuffix    = ".memory_mib"
	instanceDiskSuffix      = ".disk_gib"
	// instanceQEMUStateSuffix is qemu's own run state, published only for a VM
	// qemu is not running: io-error, paused, postmigrate, ... A box that runs
	// normally has no key here.
	instanceQEMUStateSuffix = ".qemu_status"
)

// Values of driver.box.instance.<box-id>.task.
const (
	// taskRunning means a task of this driver owns the instance right now, so
	// Nomad counts its reservation among the allocations of the node.
	taskRunning = "running"
	// taskNone means no task owns the instance: the VM may still be running (a
	// lost allocation, a stopped allocation the client detached from, an instance
	// nobody started through Nomad).
	taskNone = "none"
)

// InstanceInfo is one Lima instance as capacity accounting sees it.
type InstanceInfo struct {
	// BoxID is the instance name, which is the box id.
	BoxID string
	// State is Lima's status ("Running", "Stopped", "Broken", ...).
	State string
	// Task is taskRunning when a task of this driver owns the instance.
	Task string
	// CPUs, MemoryMiB and DiskGiB are the instance's configured size.
	CPUs      int
	MemoryMiB int64
	DiskGiB   int64
	// QEMUStatus is what qemu says it is doing, empty when qemu is running the VM
	// normally or when its state is not known (a vz box, a VM that does not
	// answer). It is the one place that says a full host disk froze a box.
	QEMUStatus string
}

// instanceInfos describes every listed instance, sorted by box id. attached maps
// a box id to the task that owns it (see taskStore.BoxIDs), and qemu is what qemu
// last said about each VM (see Driver.qemuStatuses).
func instanceInfos(instances []*limactl.Instance, attached map[string]string, qemu map[string]string) []InstanceInfo {
	out := make([]InstanceInfo, 0, len(instances))
	for _, inst := range instances {
		if inst == nil || inst.Name == "" {
			continue
		}
		info := InstanceInfo{
			BoxID:     inst.Name,
			State:     inst.Status,
			Task:      taskNone,
			CPUs:      inst.CPUs,
			MemoryMiB: bytesToMiB(inst.Memory),
			DiskGiB:   bytesToGiB(inst.Disk),
		}
		if _, ok := attached[inst.Name]; ok {
			info.Task = taskRunning
		}
		// Only a VM qemu is not running is worth a key: a running one says nothing
		// a reader needs, and a stale key on every box would be noise.
		if status := qemu[inst.Name]; status != "" && status != qemuStateRunning {
			info.QEMUStatus = status
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BoxID < out[j].BoxID })
	return out
}

// setInstanceAttributes publishes the instances in the fingerprint.
func setInstanceAttributes(fp *drivers.Fingerprint, infos []InstanceInfo) {
	for _, info := range infos {
		prefix := instanceAttributePrefix + info.BoxID
		fp.Attributes[prefix+instanceStateSuffix] = structs.NewStringAttribute(info.State)
		fp.Attributes[prefix+instanceTaskSuffix] = structs.NewStringAttribute(info.Task)
		fp.Attributes[prefix+instanceCPUsSuffix] = structs.NewStringAttribute(strconv.Itoa(info.CPUs))
		fp.Attributes[prefix+instanceMemorySuffix] = structs.NewStringAttribute(strconv.FormatInt(info.MemoryMiB, 10))
		fp.Attributes[prefix+instanceDiskSuffix] = structs.NewStringAttribute(strconv.FormatInt(info.DiskGiB, 10))
		if info.QEMUStatus != "" {
			fp.Attributes[prefix+instanceQEMUStateSuffix] = structs.NewStringAttribute(info.QEMUStatus)
		}
	}
}

// bytesToMiB rounds a byte count up to whole MiB: a VM needs the memory it was
// given, so rounding down would understate the host it occupies.
func bytesToMiB(b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (b + (1 << 20) - 1) / (1 << 20)
}

// bytesToGiB rounds a byte count up to whole GiB.
func bytesToGiB(b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (b + (1 << 30) - 1) / (1 << 30)
}
