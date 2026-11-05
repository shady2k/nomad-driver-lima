package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// ResourceUsage is how much of one resource a node has, in the unit Nomad uses
// (MHz of CPU, MiB of memory, MiB of disk).
type ResourceUsage struct {
	Total     int64 `json:"total"`
	Reserved  int64 `json:"reserved"`
	Allocated int64 `json:"allocated"`
	Free      int64 `json:"free"`
}

// DriverInfo is what the box driver reports about one node.
type DriverInfo struct {
	// Installed reports whether Nomad lists the box driver on the node.
	Installed bool `json:"installed"`
	// Healthy reports the driver health Nomad reports; HealthReported says
	// whether Nomad reported an opinion at all.
	Healthy        bool `json:"healthy"`
	HealthReported bool `json:"-"`
	// Version, Arch, OS, VMType, LimaVersion, Instances and Orphans come from
	// the driver.box.* fingerprint attributes.
	Version     string `json:"version,omitempty"`
	Arch        string `json:"arch,omitempty"`
	OS          string `json:"os,omitempty"`
	VMType      string `json:"vm_type,omitempty"`
	LimaVersion string `json:"lima_version,omitempty"`
	Instances   string `json:"instances,omitempty"`
	Orphans     string `json:"orphans,omitempty"`
	// MemoryLimit is "enforce" or "off": whether the node caps the memory of a
	// box at all (driver.box.memory_limit).
	MemoryLimit string `json:"memory_limit,omitempty"`
}

// BoxDisk is the host footprint of one box disk, as the box driver reports it
// in the node fingerprint: a qcow2 grows on demand, so "used" (the blocks it
// occupies) is much smaller than "virtual" (the size it may grow to).
type BoxDisk struct {
	BoxID        string `json:"box_id"`
	UsedBytes    int64  `json:"used_bytes"`
	VirtualBytes int64  `json:"virtual_bytes"`
}

// The driver's disk usage attributes (internal/driver/disk.go).
const (
	boxDiskAttrPrefix    = "driver.box.disk."
	boxDiskUsedSuffix    = ".used_bytes"
	boxDiskVirtualSuffix = ".virtual_bytes"
	// boxDiskFreeAttr is the space free on the filesystem the node writes its box
	// disks to (the Lima home): a bake copies a whole disk there before it seals
	// it, so this is the number that decides whether it can.
	boxDiskFreeAttr = boxDiskAttrPrefix + "free_bytes"
)

// boxDisksFromAttributes reads the driver's per-box disk attributes of a node.
//
// The driver always publishes the two totals, so a reader never has to guess
// whether a missing attribute means zero.
func boxDisksFromAttributes(attrs map[string]string) (disks []BoxDisk, totalUsed, totalVirtual int64) {
	byBox := map[string]*BoxDisk{}
	order := []string{}
	sawTotalUsed, sawTotalVirtual := false, false
	for key, value := range attrs {
		if !strings.HasPrefix(key, boxDiskAttrPrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, boxDiskAttrPrefix)
		n, ok := parseInt64(value)
		if !ok {
			continue
		}
		switch {
		case rest == "used_bytes":
			totalUsed, sawTotalUsed = n, true
		case rest == "virtual_bytes":
			totalVirtual, sawTotalVirtual = n, true
		case strings.HasSuffix(rest, boxDiskUsedSuffix) || strings.HasSuffix(rest, boxDiskVirtualSuffix):
			isUsed := strings.HasSuffix(rest, boxDiskUsedSuffix)
			id := strings.TrimSuffix(strings.TrimSuffix(rest, boxDiskUsedSuffix), boxDiskVirtualSuffix)
			if id == "" {
				continue
			}
			disk, ok := byBox[id]
			if !ok {
				disk = &BoxDisk{BoxID: id}
				byBox[id] = disk
				order = append(order, id)
			}
			if isUsed {
				disk.UsedBytes = n
			} else {
				disk.VirtualBytes = n
			}
		}
	}
	sort.Strings(order)
	for _, id := range order {
		disks = append(disks, *byBox[id])
	}
	// A driver that predates the totals (or a hand-made attribute set) still
	// gives a usable answer: sum what is there.
	if !sawTotalUsed || !sawTotalVirtual {
		var used, virtual int64
		for _, disk := range disks {
			used += disk.UsedBytes
			virtual += disk.VirtualBytes
		}
		if !sawTotalUsed {
			totalUsed = used
		}
		if !sawTotalVirtual {
			totalVirtual = virtual
		}
	}
	return disks, totalUsed, totalVirtual
}

// parseInt64 parses a fingerprint attribute value.
func parseInt64(value string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// BoxInstance is one live Lima instance on a node, as the box driver reports it
// in the node fingerprint (internal/driver/instances.go).
//
// Nomad accounts for an allocation, not for a VM: a box whose allocation was
// lost, stopped or is still being adopted holds its vCPUs and its memory all the
// same. The driver says whether a running task owns the instance, and
// BuildCapacity subtracts the ones it does not.
type BoxInstance struct {
	BoxID string `json:"box_id"`
	// State is Lima's status: Running, Stopped, Broken, ...
	State string `json:"state"`
	// Task is true when a running task of the box driver owns the instance.
	Task bool `json:"task"`
	// CPUs is the instance's vCPU count, MemoryMiB its guest RAM and DiskGiB the
	// size of its disk.
	CPUs      int   `json:"cpus"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskGiB   int64 `json:"disk_gib"`
}

// RunningUncounted reports whether the VM occupies the node while Nomad's
// accounting does not cover it.
func (i BoxInstance) RunningUncounted() bool {
	return !i.Task && !isTerminalInstanceState(i.State)
}

// isTerminalInstanceState reports whether a Lima instance is down: a stopped box
// holds its disk but no memory and no vCPUs.
func isTerminalInstanceState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "stopped", "broken", "":
		return true
	}
	return false
}

// The driver's per-instance attributes (internal/driver/instances.go).
const (
	boxInstanceAttrPrefix   = "driver.box.instance."
	boxInstanceStateSuffix  = ".state"
	boxInstanceTaskSuffix   = ".task"
	boxInstanceCPUsSuffix   = ".cpus"
	boxInstanceMemorySuffix = ".memory_mib"
	boxInstanceDiskSuffix   = ".disk_gib"
	boxInstanceTaskRunning  = "running"
)

// boxInstancesFromAttributes reads the driver's per-instance attributes.
func boxInstancesFromAttributes(attrs map[string]string) []BoxInstance {
	byBox := map[string]*BoxInstance{}
	order := []string{}
	for key, value := range attrs {
		// An empty value is the driver clearing the key of a box that is gone.
		if !strings.HasPrefix(key, boxInstanceAttrPrefix) || value == "" {
			continue
		}
		rest := strings.TrimPrefix(key, boxInstanceAttrPrefix)
		boxID := ""
		field := ""
		for _, suffix := range []string{boxInstanceStateSuffix, boxInstanceTaskSuffix, boxInstanceCPUsSuffix,
			boxInstanceMemorySuffix, boxInstanceDiskSuffix} {
			if strings.HasSuffix(rest, suffix) {
				boxID, field = strings.TrimSuffix(rest, suffix), suffix
				break
			}
		}
		if boxID == "" || field == "" {
			continue
		}
		inst, ok := byBox[boxID]
		if !ok {
			inst = &BoxInstance{BoxID: boxID}
			byBox[boxID] = inst
			order = append(order, boxID)
		}
		switch field {
		case boxInstanceStateSuffix:
			inst.State = value
		case boxInstanceTaskSuffix:
			inst.Task = strings.EqualFold(strings.TrimSpace(value), boxInstanceTaskRunning)
		case boxInstanceCPUsSuffix:
			inst.CPUs = int(intOrZero(value))
		case boxInstanceMemorySuffix:
			inst.MemoryMiB = intOrZero(value)
		case boxInstanceDiskSuffix:
			inst.DiskGiB = intOrZero(value)
		}
	}
	sort.Strings(order)
	out := make([]BoxInstance, 0, len(order))
	for _, id := range order {
		out = append(out, *byBox[id])
	}
	return out
}

// intOrZero parses a fingerprint attribute value, 0 when it is not a number.
func intOrZero(value string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// VolumePluginInfo is one dynamic host volume plugin found on a node.
type VolumePluginInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// NodeCapacity is one node as a place to run boxes.
type NodeCapacity struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	// Arch is amd64 or arm64.
	Arch string `json:"arch"`
	// OS is the guest operating system the node can run (driver.box.os).
	OS string `json:"os"`
	// HostOS is the operating system of the host itself (os.name).
	HostOS string `json:"host_os,omitempty"`
	// Status is the Nomad node status (ready, down, ...).
	Status string `json:"status"`
	// Eligible reports the scheduling eligibility.
	Eligible bool `json:"eligible"`
	// CPU is in MHz, Memory and Disk in MiB.
	CPU    ResourceUsage `json:"cpu_shares"`
	Memory ResourceUsage `json:"memory_mib"`
	Disk   ResourceUsage `json:"disk_mib"`
	// Driver is the box driver on this node.
	Driver DriverInfo `json:"driver"`
	// VolumePlugins are the dynamic host volume plugins of the node.
	VolumePlugins []VolumePluginInfo `json:"volume_plugins,omitempty"`
	// Boxes is the number of boxes whose disk is on this node: a stopped box
	// holds its disk, so it counts, and a box Nomad has no job for still counts.
	Boxes int `json:"boxes"`
	// BoxesStopped is how many of Boxes are stopped or gone: no job, and no
	// memory or vCPU taken.
	BoxesStopped int `json:"boxes_stopped,omitempty"`
	// OrphanDisks is how many disks on this node belong to no box: nothing
	// describes them, and `box rm <id> --yes` deletes one.
	OrphanDisks int `json:"orphan_disks,omitempty"`
	// Disks is the host footprint of every box disk on the node, stopped boxes
	// and orphans included.
	Disks []BoxDisk `json:"box_disks,omitempty"`
	// DiskUsedBytes is what all those disks occupy, in bytes.
	DiskUsedBytes int64 `json:"disk_used_bytes"`
	// DiskVirtualBytes is the size they may grow to, in bytes.
	DiskVirtualBytes int64 `json:"disk_virtual_bytes"`
	// DiskFreeBytes is the space free on the filesystem the node writes its box
	// disks to, as the box driver measures it: a bake copies a whole disk there.
	DiskFreeBytes int64 `json:"disk_free_bytes"`
	// Instances is every live Lima instance on the node, with its size and
	// whether a running task owns it.
	Instances []BoxInstance `json:"box_instances,omitempty"`
	// Uncounted is the running instances Nomad's accounting does not cover: their
	// memory and vCPUs are taken off the free resources below.
	Uncounted []BoxInstance `json:"uncounted_instances,omitempty"`
	// CPUMHzPerVCPU is what one vCPU reserves on this node.
	CPUMHzPerVCPU int64 `json:"cpu_mhz_per_vcpu"`
	// UncountedMemoryMiB and UncountedCPUShares are what those VMs take.
	UncountedMemoryMiB int64 `json:"uncounted_memory_mib"`
	UncountedCPUShares int64 `json:"uncounted_cpu_shares"`
}

// Ready reports whether the node can take a new box at all.
func (n *NodeCapacity) Ready() bool { return n.NodeProblem(PlacementRequest{}) == "" }

// hasVolumePlugin reports whether the node has the box-volume plugin.
func (n *NodeCapacity) hasVolumePlugin(id string) bool {
	for _, p := range n.VolumePlugins {
		if p.ID == id {
			return true
		}
	}
	return false
}

// nodeArch reads the architecture of a node: the box driver's own attribute
// first, then Nomad's cpu.arch and kernel.arch.
func nodeArch(attrs map[string]string, driver DriverInfo) string {
	for _, key := range []string{"driver.box.arch", "cpu.arch", "kernel.arch"} {
		if v := strings.TrimSpace(attrs[key]); v != "" {
			return NormalizeArch(v)
		}
	}
	if driver.Arch != "" {
		return NormalizeArch(driver.Arch)
	}
	return ""
}

// boxAttrPrefix is the prefix of every attribute the box driver publishes.
const boxAttrPrefix = "driver.box."

// nodeAttributes returns the node's attributes with the box driver's own taken
// from its driver info.
//
// Nomad merges a driver's fingerprint into node.Attributes key by key and never
// removes a key the driver stops sending, so a box deleted from the node would
// stay there for good (client/node_updater.go). The driver info holds the last
// fingerprint as a whole, so it is the one to read. A node whose driver info
// carries no attributes keeps the merged ones.
func nodeAttributes(node *api.Node) map[string]string {
	info := node.Drivers[boxDriverName]
	if info == nil || len(info.Attributes) == 0 {
		if node.Attributes == nil {
			return map[string]string{}
		}
		return node.Attributes
	}
	attrs := make(map[string]string, len(node.Attributes)+len(info.Attributes))
	for k, v := range node.Attributes {
		if !strings.HasPrefix(k, boxAttrPrefix) {
			attrs[k] = v
		}
	}
	for k, v := range info.Attributes {
		attrs[k] = v
	}
	return attrs
}

// BuildCapacity turns one node and its allocations into a NodeCapacity.
//
// Free resources are the node's totals minus what the client reserves minus
// what its live allocations use, so this is exactly what Nomad itself would
// offer to a new box on that node.
func BuildCapacity(node *api.Node, allocs []*api.Allocation) *NodeCapacity {
	if node == nil {
		return nil
	}
	attrs := nodeAttributes(node)
	driver := DriverInfo{
		Version:     attrs["driver.box.version"],
		Arch:        attrs["driver.box.arch"],
		OS:          attrs["driver.box.os"],
		VMType:      attrs["driver.box.vm_type"],
		LimaVersion: attrs["driver.box.lima_version"],
		Instances:   attrs["driver.box.instances"],
		Orphans:     attrs["driver.box.orphans"],
		MemoryLimit: attrs["driver.box.memory_limit"],
	}
	if info, ok := node.Drivers[boxDriverName]; ok && info != nil {
		driver.Installed = true
		driver.Healthy = info.Healthy
		driver.HealthReported = true
	}
	if driver.Version != "" {
		driver.Installed = true
	}

	cap := &NodeCapacity{
		NodeID:   node.ID,
		Name:     node.Name,
		Arch:     nodeArch(attrs, driver),
		OS:       driver.OS,
		HostOS:   attrs["os.name"],
		Status:   node.Status,
		Eligible: node.SchedulingEligibility == api.NodeSchedulingEligible,
		Driver:   driver,
	}
	cap.Disks, cap.DiskUsedBytes, cap.DiskVirtualBytes = boxDisksFromAttributes(attrs)
	cap.DiskFreeBytes, _ = strconv.ParseInt(strings.TrimSpace(attrs[boxDiskFreeAttr]), 10, 64)
	cap.Instances = boxInstancesFromAttributes(attrs)
	var uncountedVCPUs int64
	for _, inst := range cap.Instances {
		if !inst.RunningUncounted() {
			continue
		}
		cap.Uncounted = append(cap.Uncounted, inst)
		cap.UncountedMemoryMiB += inst.MemoryMiB
		uncountedVCPUs += int64(inst.CPUs)
	}

	// CPU and memory totals and reservations come from the node resources.
	var (
		cpuTotal, cpuReserved      int64
		memTotal, memReserved      int64
		diskTotal, diskReserved    int64
		cpuAllocated, memAllocated int64
		diskAllocated              int64
	)
	if res := node.NodeResources; res != nil {
		cpuTotal = res.Cpu.CpuShares
		memTotal = res.Memory.MemoryMB
		diskTotal = res.Disk.DiskMB
	} else if res := node.Resources; res != nil {
		cpuTotal = int64(derefInt(res.CPU, 0))
		memTotal = int64(derefInt(res.MemoryMB, 0))
		diskTotal = int64(derefInt(res.DiskMB, 0))
	}
	if res := node.ReservedResources; res != nil {
		cpuReserved = int64(res.Cpu.CpuShares)
		memReserved = int64(res.Memory.MemoryMB)
		diskReserved = int64(res.Disk.DiskMB)
	} else if res := node.Reserved; res != nil {
		cpuReserved = int64(derefInt(res.CPU, 0))
		memReserved = int64(derefInt(res.MemoryMB, 0))
		diskReserved = int64(derefInt(res.DiskMB, 0))
	}

	for _, alloc := range allocs {
		if alloc == nil || isTerminalAlloc(alloc.ClientStatus) {
			continue
		}
		if alloc.Job != nil && IsBoxJob(alloc.Job) {
			cap.Boxes++
		} else if _, ok := BoxIDFromJobName(alloc.JobID); ok {
			cap.Boxes++
		}
		if alloc.AllocatedResources == nil {
			continue
		}
		for _, task := range alloc.AllocatedResources.Tasks {
			if task == nil {
				continue
			}
			cpuAllocated += task.Cpu.CpuShares
			if task.Memory.MemoryMaxMB > task.Memory.MemoryMB {
				memAllocated += task.Memory.MemoryMaxMB
			} else {
				memAllocated += task.Memory.MemoryMB
			}
		}
		diskAllocated += alloc.AllocatedResources.Shared.DiskMB
	}

	cap.CPU = usage(cpuTotal, cpuReserved, cpuAllocated)
	cap.CPUMHzPerVCPU = mhzPerVCPU(cpuTotal, cpuReserved, attrs["cpu.numcores"])
	cap.UncountedCPUShares = uncountedVCPUs * cap.CPUMHzPerVCPU
	cap.Memory = usage(memTotal, memReserved, memAllocated)
	cap.Disk = usage(diskTotal, diskReserved, diskAllocated)

	// A running VM without a running allocation is not in cpuAllocated or
	// memAllocated, but its host memory and its vCPUs are taken. Take them off
	// the free resources, so this node does not look roomier than it is.
	//
	// The disk is not taken off: NodeCapacity.Disk is the node's own disk, while a
	// box disk is a file on a dynamic host volume whose real size is reported in
	// Disks and DiskUsedBytes.
	cap.CPU.Free = clampSubtract(cap.CPU.Free, cap.UncountedCPUShares)
	cap.Memory.Free = clampSubtract(cap.Memory.Free, cap.UncountedMemoryMiB)

	for key, version := range attrs {
		const prefix = "plugins.host_volume."
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, ".version") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".version")
		cap.VolumePlugins = append(cap.VolumePlugins, VolumePluginInfo{ID: id, Version: version})
	}
	sort.Slice(cap.VolumePlugins, func(i, j int) bool { return cap.VolumePlugins[i].ID < cap.VolumePlugins[j].ID })

	if cap.OS == "" {
		cap.OS = cap.HostOS
	}
	if cap.Status == "" {
		cap.Status = "unknown"
	}
	return cap
}

func usage(total, reserved, allocated int64) ResourceUsage {
	free := total - reserved - allocated
	if free < 0 {
		free = 0
	}
	return ResourceUsage{Total: total, Reserved: reserved, Allocated: allocated, Free: free}
}

// boxCPUSharesPerVCPU is the CPU reservation of one vCPU on a node that does not
// say how many cores it has.
const boxCPUSharesPerVCPU = 1000

// mhzPerVCPU is the reservation of one vCPU on a node: its core, less its part of
// what the client keeps for itself. One vCPU can keep one core busy, and a node
// offers exactly as many vCPUs as it has cores, none above and none lost to the
// client's reservation.
func mhzPerVCPU(total, reserved int64, numCores string) int64 {
	cores, err := strconv.ParseInt(strings.TrimSpace(numCores), 10, 64)
	if err != nil || cores <= 0 || total-reserved < cores {
		return boxCPUSharesPerVCPU
	}
	return (total - reserved) / cores
}

// vcpuMHz is the reservation of one vCPU on this node.
func (n *NodeCapacity) vcpuMHz() int64 {
	if n.CPUMHzPerVCPU > 0 {
		return n.CPUMHzPerVCPU
	}
	return boxCPUSharesPerVCPU
}

// clampSubtract removes amount from free, never going below zero.
func clampSubtract(free, amount int64) int64 {
	if amount <= 0 {
		return free
	}
	free -= amount
	if free < 0 {
		return 0
	}
	return free
}

// isTerminalAlloc reports whether an allocation no longer uses resources.
func isTerminalAlloc(clientStatus string) bool {
	switch clientStatus {
	case "complete", "failed", "lost":
		return true
	}
	return false
}

// NodeProblem returns the plain-words reason this node cannot take the box, or
// "" when it can.
func (n *NodeCapacity) NodeProblem(req PlacementRequest) string {
	switch {
	case n.Status != api.NodeStatusReady:
		return fmt.Sprintf("node %s is %s, not ready", n.Name, n.Status)
	case !n.Eligible:
		return fmt.Sprintf("node %s is not eligible for new allocations", n.Name)
	case !n.Driver.Installed:
		return fmt.Sprintf("node %s has no box driver installed", n.Name)
	case n.Driver.Installed && n.Driver.HealthReported && !n.Driver.Healthy:
		return fmt.Sprintf("the box driver on node %s is unhealthy", n.Name)
	case !n.hasVolumePlugin(boxVolumePluginID):
		return fmt.Sprintf("node %s has no %s host volume plugin, so it cannot hold a box disk", n.Name, boxVolumePluginID)
	case NormalizeArch(req.Arch) != "" && n.Arch != NormalizeArch(req.Arch):
		return fmt.Sprintf("node %s runs %s images, the box needs %s", n.Name, n.Arch, NormalizeArch(req.Arch))
	}
	if need := req.TaskMemoryMiB(); need > 0 && n.Memory.Free < int64(need) {
		return fmt.Sprintf("node %s has %d MiB of memory free, the box needs %d MiB", n.Name, n.Memory.Free, need)
	}
	if need := int64(req.CPUs) * n.vcpuMHz(); need > 0 && n.CPU.Free < need {
		return fmt.Sprintf("node %s has %d MHz of CPU free, the box needs %d MHz", n.Name, n.CPU.Free, need)
	}
	if need := int64(req.DiskGiB) * 1024; need > 0 && n.Disk.Free < need {
		return fmt.Sprintf("node %s has %d MiB of disk free, the box disk needs %d MiB", n.Name, n.Disk.Free, need)
	}
	return ""
}

func (r PlacementRequest) TaskMemoryMiB() int {
	overhead := r.MemoryOverheadMiB
	if overhead == 0 {
		overhead = BoxMemoryOverheadMiB
	}
	return r.MemoryMiB + overhead
}

// PlacementRequest is what a new box needs from a node.
type PlacementRequest struct {
	Arch              string
	CPUs              int
	MemoryMiB         int
	MemoryOverheadMiB int
	DiskGiB           int
	// Node is an explicit node name or id (--node).
	Node string
}

// quarterShare is the part of a node a box is worth a word about: a node is
// shared by every agent's boxes, so a box that takes more than a quarter of its
// CPU, memory or disk is said on stderr. It is not a refusal -- free resources
// are there to be used, and a box the CLI will not create is work that does not
// start -- so nothing here changes an exit code.
const quarterShare = 4

// ShareNote says, in plain words, how a box of this size takes more than a
// quarter of the node's CPU, memory or disk, or "" when it does not.
func (n *NodeCapacity) ShareNote(id string, cpus, memoryMiB, diskGiB int) string {
	var over []string
	if vcpus := n.vcpus(); vcpus/quarterShare > 0 && int64(cpus) > vcpus/quarterShare {
		over = append(over, fmt.Sprintf("%d of %d vCPU", cpus, vcpus))
	}
	if limit := n.Memory.Total / quarterShare; limit > 0 && int64(memoryMiB) > limit {
		over = append(over, shareMemory(int64(memoryMiB), n.Memory.Total)+" of memory")
	}
	if limit := n.Disk.Total / quarterShare; limit > 0 && int64(diskGiB)*1024 > limit {
		over = append(over, fmt.Sprintf("%d of %d GiB of disk", diskGiB, n.Disk.Total/1024))
	}
	if len(over) == 0 {
		return ""
	}
	return fmt.Sprintf("this box takes %s on node %s; the node is shared: `box stop %s` or `box rm %s --yes` it "+
		"when the work is done", strings.Join(over, ", "), n.Name, id, id)
}

// shareMemory renders a pair of memory sizes in one unit, so that "8 of 16 GiB"
// reads as the comparison it is.
func shareMemory(taken, total int64) string {
	if total >= 10*1024 {
		return fmt.Sprintf("%.0f of %.0f GiB", float64(taken)/1024, float64(total)/1024)
	}
	return fmt.Sprintf("%d of %d MiB", taken, total)
}

// vcpus is how many vCPUs the node offers: its CPU total in the units one vCPU of
// it reserves.
func (n *NodeCapacity) vcpus() int64 {
	mhz := n.vcpuMHz()
	if mhz <= 0 {
		return 0
	}
	return n.CPU.Total / mhz
}

// ChooseNode picks the node a new box goes to.
//
// An explicit --node must match exactly and must be usable; otherwise the node
// with the most free memory wins, so boxes spread out. A failure explains every
// node that was rejected, so the operator can fix the cluster instead of
// guessing.
func ChooseNode(nodes []*NodeCapacity, req PlacementRequest) (*NodeCapacity, []string, error) {
	if len(nodes) == 0 {
		return nil, nil, notFoundf("no Nomad client node is registered; start a client with the box driver and the %s plugin",
			boxVolumePluginID)
	}
	var (
		best     *NodeCapacity
		problems []string
	)
	candidates := make([]*NodeCapacity, 0, len(nodes))
	for _, node := range nodes {
		if req.Node != "" && node.Name != req.Node && node.NodeID != req.Node {
			continue
		}
		if problem := node.NodeProblem(req); problem != "" {
			problems = append(problems, problem)
			continue
		}
		candidates = append(candidates, node)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Memory.Free != candidates[j].Memory.Free {
			return candidates[i].Memory.Free > candidates[j].Memory.Free
		}
		return candidates[i].Name < candidates[j].Name
	})
	if len(candidates) > 0 {
		best = candidates[0]
	}
	if best != nil {
		return best, problems, nil
	}
	if req.Node != "" && len(problems) == 0 {
		return nil, problems, notFoundf("there is no node named %q", req.Node)
	}
	sort.Strings(problems)
	return nil, problems, errorf("no node can take this box: %s", strings.Join(problems, "; "))
}

// nodeCapacities loads every node with its allocations.
func (a *App) nodeCapacities(ctx context.Context) ([]*NodeCapacity, error) {
	stubs, err := a.Nomad.NodeList(ctx)
	if err != nil {
		return nil, wrapf(err, "cannot list the Nomad nodes")
	}
	out := make([]*NodeCapacity, 0, len(stubs))
	for _, stub := range stubs {
		if stub == nil {
			continue
		}
		node, err := a.Nomad.NodeInfo(ctx, stub.ID)
		if err != nil {
			return nil, wrapf(err, "cannot read node %s", stub.Name)
		}
		allocs, err := a.Nomad.NodeAllocations(ctx, stub.ID)
		if err != nil {
			return nil, wrapf(err, "cannot list the allocations of node %s", stub.Name)
		}
		if cap := BuildCapacity(node, allocs); cap != nil {
			out = append(out, cap)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Arch != out[j].Arch {
			return out[i].Arch < out[j].Arch
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// findNode looks one node up by name or id.
func (a *App) findNode(ctx context.Context, name string) (*NodeCapacity, error) {
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if node.Name == name || node.NodeID == name {
			return node, nil
		}
	}
	return nil, notFoundf("there is no node named %q; `box capacity` lists the nodes", name)
}

// archSummary sums the free resources per architecture. Architectures are never
// added together: a vCPU on one architecture cannot run an image of the other.
type archSummary struct {
	Arch       string `json:"arch"`
	Nodes      int    `json:"nodes"`
	CPUShares  int64  `json:"cpu_shares_free"`
	MemoryMiB  int64  `json:"memory_mib_free"`
	DiskMiB    int64  `json:"disk_mib_free"`
	ReadyNodes int    `json:"ready_nodes"`
}

func summarizeArches(nodes []*NodeCapacity) []archSummary {
	byArch := map[string]*archSummary{}
	order := []string{}
	for _, node := range nodes {
		arch := node.Arch
		if arch == "" {
			arch = "unknown"
		}
		summary, ok := byArch[arch]
		if !ok {
			summary = &archSummary{Arch: arch}
			byArch[arch] = summary
			order = append(order, arch)
		}
		summary.Nodes++
		summary.CPUShares += node.CPU.Free
		summary.MemoryMiB += node.Memory.Free
		summary.DiskMiB += node.Disk.Free
		if node.Ready() {
			summary.ReadyNodes++
		}
	}
	sort.Strings(order)
	out := make([]archSummary, 0, len(order))
	for _, arch := range order {
		out = append(out, *byArch[arch])
	}
	return out
}

// humanMiB renders MiB as GiB when it is large enough to read better.
func humanMiB(mib int64) string {
	if mib >= 10*1024 {
		return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

func humanMHz(mhz int64) string {
	return strconv.FormatInt(mhz, 10)
}

func clampPercent(used, total int64) int {
	if total <= 0 {
		return 0
	}
	p := int(math.Round(float64(used) * 100 / float64(total)))
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
