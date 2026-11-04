package cli

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func TestBuildCapacity(t *testing.T) {
	cluster := newFakeCluster()
	node := cluster.addNode("node-1", "node-1", "amd64", 12000, 11961, 150000)
	reserved := &api.NodeReservedResources{
		Cpu:    api.NodeReservedCpuResources{CpuShares: 500},
		Memory: api.NodeReservedMemoryResources{MemoryMB: 1000},
		Disk:   api.NodeReservedDiskResources{DiskMB: 500},
	}
	node.ReservedResources = reserved

	allocs := []*api.Allocation{
		{
			ID:           "alloc-1",
			JobID:        "box-dev1",
			ClientStatus: "running",
			AllocatedResources: &api.AllocatedResources{
				Tasks: map[string]*api.AllocatedTaskResources{
					"box": {Cpu: api.AllocatedCpuResources{CpuShares: 2000}, Memory: api.AllocatedMemoryResources{MemoryMB: 1174, MemoryMaxMB: 1174}},
				},
				Shared: api.AllocatedSharedResources{DiskMB: 1024},
			},
		},
		{
			// A terminal allocation frees its resources again.
			ID:           "alloc-0",
			JobID:        "box-old",
			ClientStatus: "complete",
			AllocatedResources: &api.AllocatedResources{
				Tasks: map[string]*api.AllocatedTaskResources{
					"box": {Cpu: api.AllocatedCpuResources{CpuShares: 4000}, Memory: api.AllocatedMemoryResources{MemoryMB: 4096}},
				},
			},
		},
	}
	cap := BuildCapacity(node, allocs)
	require.NotNil(t, cap)
	require.Equal(t, "node-1", cap.Name)
	require.Equal(t, "amd64", cap.Arch)
	require.Equal(t, "linux", cap.OS)
	require.Equal(t, "ready", cap.Status)
	require.True(t, cap.Eligible)
	require.True(t, cap.Driver.Installed)
	require.Equal(t, "0.2.0", cap.Driver.Version)
	require.Equal(t, "2.2.0", cap.Driver.LimaVersion)
	require.Len(t, cap.VolumePlugins, 2)
	require.Equal(t, "box-volume", cap.VolumePlugins[0].ID)
	require.Equal(t, 1, cap.Boxes)

	require.Equal(t, int64(12000), cap.CPU.Total)
	require.Equal(t, int64(500), cap.CPU.Reserved)
	require.Equal(t, int64(2000), cap.CPU.Allocated)
	require.Equal(t, int64(9500), cap.CPU.Free)

	require.Equal(t, int64(11961-1000-1174), cap.Memory.Free)
	require.Equal(t, int64(150000-500-1024), cap.Disk.Free)

	// The disk footprint the driver reports is part of the capacity picture.
	cap = BuildCapacity(withDisks(node, "dev1", 200<<20, int64(8)<<30), nil)
	require.Len(t, cap.Disks, 1)
	require.Equal(t, "dev1", cap.Disks[0].BoxID)
	require.Equal(t, int64(200<<20), cap.DiskUsedBytes)
	require.Equal(t, int64(8)<<30, cap.DiskVirtualBytes)

	// A node's free resources never go negative.
	cap.CPU = usage(100, 50, 200)
	require.Equal(t, int64(0), cap.CPU.Free)
}

func TestBoxDisksFromAttributes(t *testing.T) {
	attrs := map[string]string{
		"driver.box.disk.used_bytes":         "314572800",
		"driver.box.disk.virtual_bytes":      "12884901888",
		"driver.box.disk.dev1.used_bytes":    "209715200",
		"driver.box.disk.dev1.virtual_bytes": "8589934592",
		"driver.box.disk.dev2.used_bytes":    "104857600",
		"driver.box.disk.dev2.virtual_bytes": "4294967296",
		"driver.box.disk.broken.used_bytes":  "not a number",
		"driver.box.arch":                    "x86_64",
	}
	disks, totalUsed, totalVirtual := boxDisksFromAttributes(attrs)
	require.Len(t, disks, 2, "only well-formed entries count")
	require.Equal(t, "dev1", disks[0].BoxID)
	require.Equal(t, int64(200<<20), disks[0].UsedBytes)
	require.Equal(t, int64(8)<<30, disks[0].VirtualBytes)
	require.Equal(t, "dev2", disks[1].BoxID)
	require.Equal(t, int64(300<<20), totalUsed)
	require.Equal(t, int64(12)<<30, totalVirtual)

	// Without the node totals the per-box values are summed instead.
	noTotals := map[string]string{
		"driver.box.disk.dev1.used_bytes":    "1048576",
		"driver.box.disk.dev1.virtual_bytes": "8589934592",
		"driver.box.disk.dev2.used_bytes":    "2097152",
		"driver.box.disk.dev2.virtual_bytes": "4294967296",
	}
	_, used, virtual := boxDisksFromAttributes(noTotals)
	require.Equal(t, int64(3<<20), used)
	require.Equal(t, int64(12)<<30, virtual)

	// A node whose driver is older than this attribute set reports nothing, and
	// that has to read as zero, not as an error.
	disks, used, virtual = boxDisksFromAttributes(map[string]string{"driver.box.arch": "x86_64"})
	require.Empty(t, disks)
	require.Zero(t, used)
	require.Zero(t, virtual)
}

func TestBuildCapacityMissingPlugin(t *testing.T) {
	node := &api.Node{
		ID: "node-2", Name: "node-2", Status: "ready", SchedulingEligibility: "eligible",
		Attributes: map[string]string{"cpu.arch": "arm64", "os.name": "ubuntu"},
		NodeResources: &api.NodeResources{
			Cpu: api.NodeCpuResources{CpuShares: 4000}, Memory: api.NodeMemoryResources{MemoryMB: 8000},
			Disk: api.NodeDiskResources{DiskMB: 50000},
		},
	}
	cap := BuildCapacity(node, nil)
	require.Equal(t, "arm64", cap.Arch)
	require.False(t, cap.Driver.Installed)
	require.Contains(t, cap.NodeProblem(PlacementRequest{}), "no box driver")
	require.False(t, cap.Ready())
}

func TestChooseNodePicksMostFreeMemory(t *testing.T) {
	small := &NodeCapacity{NodeID: "a", Name: "small", Arch: "amd64", Status: "ready", Eligible: true,
		Driver: DriverInfo{Installed: true}, Memory: ResourceUsage{Total: 4000, Free: 2000},
		CPU: ResourceUsage{Total: 4000, Free: 3000}, Disk: ResourceUsage{Total: 100000, Free: 90000},
		VolumePlugins: []VolumePluginInfo{{ID: boxVolumePluginID}}}
	large := &NodeCapacity{NodeID: "b", Name: "large", Arch: "amd64", Status: "ready", Eligible: true,
		Driver: DriverInfo{Installed: true}, Memory: ResourceUsage{Total: 16000, Free: 12000},
		CPU: ResourceUsage{Total: 8000, Free: 7000}, Disk: ResourceUsage{Total: 200000, Free: 190000},
		VolumePlugins: []VolumePluginInfo{{ID: boxVolumePluginID}}}

	chosen, problems, err := ChooseNode([]*NodeCapacity{small, large}, PlacementRequest{Arch: "amd64", CPUs: 2, MemoryMiB: 1024, DiskGiB: 8, Large: true})
	require.NoError(t, err)
	require.Empty(t, problems)
	require.Equal(t, "large", chosen.Name)

	// --node wins over the amount of free memory.
	chosen, _, err = ChooseNode([]*NodeCapacity{small, large}, PlacementRequest{Node: "small", Arch: "amd64", CPUs: 1, MemoryMiB: 512, DiskGiB: 1})
	require.NoError(t, err)
	require.Equal(t, "small", chosen.Name)

	// A node that is named but unusable yields the reason, not a silent pick.
	small.VolumePlugins = nil
	_, problems, err = ChooseNode([]*NodeCapacity{small, large}, PlacementRequest{Node: "small", Arch: "amd64", CPUs: 1, MemoryMiB: 512, DiskGiB: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no box-volume host volume plugin")
	require.Len(t, problems, 1)
}

func TestChooseNodeReasons(t *testing.T) {
	base := func() *NodeCapacity {
		return &NodeCapacity{NodeID: "a", Name: "node", Arch: "amd64", Status: "ready", Eligible: true,
			Driver: DriverInfo{Installed: true}, Memory: ResourceUsage{Total: 8000, Free: 4000},
			CPU: ResourceUsage{Total: 4000, Free: 3000}, Disk: ResourceUsage{Total: 100000, Free: 90000},
			VolumePlugins: []VolumePluginInfo{{ID: boxVolumePluginID}}}
	}
	cases := []struct {
		name   string
		change func(*NodeCapacity)
		req    PlacementRequest
		want   string
		// inherent means the node cannot take any box at all
		inherent bool
	}{
		{"down", func(n *NodeCapacity) { n.Status = "down" }, PlacementRequest{}, "is down, not ready", true},
		{"ineligible", func(n *NodeCapacity) { n.Eligible = false }, PlacementRequest{}, "not eligible", true},
		{"no driver", func(n *NodeCapacity) { n.Driver.Installed = false }, PlacementRequest{}, "no box driver", true},
		{"unhealthy", func(n *NodeCapacity) { n.Driver.HealthReported = true; n.Driver.Healthy = false }, PlacementRequest{}, "unhealthy", true},
		{"no plugin", func(n *NodeCapacity) { n.VolumePlugins = nil }, PlacementRequest{}, "host volume plugin", true},
		{"arch", func(n *NodeCapacity) {}, PlacementRequest{Arch: "arm64"}, "the box needs arm64", false},
		{"memory", func(n *NodeCapacity) {}, PlacementRequest{MemoryMiB: 8000}, "memory free", false},
		{"cpu", func(n *NodeCapacity) {}, PlacementRequest{CPUs: 9}, "CPU free", false},
		{"disk", func(n *NodeCapacity) {}, PlacementRequest{DiskGiB: 1000}, "disk free", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := base()
			tc.change(node)
			problem := node.NodeProblem(tc.req)
			require.Contains(t, problem, tc.want)
			if tc.inherent {
				require.False(t, node.Ready())
			}
		})
	}
	require.True(t, base().Ready())

	// No node at all is a not-found: there is nothing to fix in the request.
	_, _, err := ChooseNode(nil, PlacementRequest{})
	require.Error(t, err)
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), "no Nomad client node is registered")

	// An unknown --node is a not-found too.
	_, _, err = ChooseNode([]*NodeCapacity{base()}, PlacementRequest{Node: "elsewhere"})
	require.Error(t, err)
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), `no node named "elsewhere"`)
}

func TestSummarizeArchesNeverAddsArchitectures(t *testing.T) {
	nodes := []*NodeCapacity{
		{Name: "a", Arch: "amd64", Status: "ready", Eligible: true, Driver: DriverInfo{Installed: true},
			VolumePlugins: []VolumePluginInfo{{ID: boxVolumePluginID}},
			Memory:        ResourceUsage{Free: 4000}, CPU: ResourceUsage{Free: 3000}, Disk: ResourceUsage{Free: 1000}},
		{Name: "b", Arch: "arm64", Status: "ready", Eligible: true, Driver: DriverInfo{Installed: true},
			VolumePlugins: []VolumePluginInfo{{ID: boxVolumePluginID}},
			Memory:        ResourceUsage{Free: 8000}, CPU: ResourceUsage{Free: 6000}, Disk: ResourceUsage{Free: 2000}},
	}
	summary := summarizeArches(nodes)
	require.Len(t, summary, 2)
	require.Equal(t, "amd64", summary[0].Arch)
	require.Equal(t, int64(4000), summary[0].MemoryMiB)
	require.Equal(t, 1, summary[0].ReadyNodes)
	require.Equal(t, "arm64", summary[1].Arch)
	require.Equal(t, int64(8000), summary[1].MemoryMiB)
}

func TestNodeCapacitiesFromCluster(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.addNode("node-2", "node-2", "arm64", 4000, 8000, 50000)
	ta.cluster.runBox(mustJob(t, goodSpec()))

	nodes, err := ta.nodeCapacities(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	require.Equal(t, "node-1", nodes[0].Name, "sorted by architecture, then name")
	require.Equal(t, "node-2", nodes[1].Name)
	require.Equal(t, 1, nodes[0].Boxes)

	_, err = ta.findNode(context.Background(), "node-2")
	require.NoError(t, err)
	_, err = ta.findNode(context.Background(), "nope")
	require.Equal(t, ExitNotFound, ExitCode(err))

	ta.cluster.mu.Lock()
	for _, alloc := range ta.cluster.allocs {
		alloc.ClientStatus = "running"
	}
	ta.cluster.mu.Unlock()
	nodes, err = ta.nodeCapacities(context.Background())
	require.NoError(t, err)
	require.True(t, nodes[0].Memory.Free <= nodes[0].Memory.Total)
	_ = time.Now
}

// withDisks adds the driver's disk usage attributes of one box to a node.
func withDisks(node *api.Node, boxID string, used, virtual int64) *api.Node {
	const prefix = boxDiskAttrPrefix
	node.Attributes[prefix+boxID+".used_bytes"] = strconv.FormatInt(used, 10)
	node.Attributes[prefix+boxID+".virtual_bytes"] = strconv.FormatInt(virtual, 10)
	node.Attributes[prefix+"used_bytes"] = strconv.FormatInt(used, 10)
	node.Attributes[prefix+"virtual_bytes"] = strconv.FormatInt(virtual, 10)
	return node
}

// mustJob renders a job or fails the test.
func mustJob(t *testing.T, spec BoxSpec) *api.Job {
	t.Helper()
	job, err := RenderJob(spec)
	require.NoError(t, err)
	return job
}

// withInstance publishes the driver's per-instance attributes for one box.
func withInstance(node *api.Node, boxID, state, task string, cpus int, memoryMiB, diskGiB int64) *api.Node {
	if node.Attributes == nil {
		node.Attributes = map[string]string{}
	}
	prefix := boxInstanceAttrPrefix + boxID
	node.Attributes[prefix+boxInstanceStateSuffix] = state
	node.Attributes[prefix+boxInstanceTaskSuffix] = task
	node.Attributes[prefix+boxInstanceCPUsSuffix] = strconv.Itoa(cpus)
	node.Attributes[prefix+boxInstanceMemorySuffix] = strconv.FormatInt(memoryMiB, 10)
	node.Attributes[prefix+boxInstanceDiskSuffix] = strconv.FormatInt(diskGiB, 10)
	return node
}

func TestBoxInstancesFromAttributes(t *testing.T) {
	attrs := map[string]string{
		"driver.box.instance.dev1.state":      "Running",
		"driver.box.instance.dev1.task":       "running",
		"driver.box.instance.dev1.cpus":       "2",
		"driver.box.instance.dev1.memory_mib": "1024",
		"driver.box.instance.dev1.disk_gib":   "8",
		"driver.box.instance.dev2.state":      "Running",
		"driver.box.instance.dev2.task":       "none",
		"driver.box.instance.dev2.cpus":       "4",
		"driver.box.instance.dev2.memory_mib": "3072",
		"driver.box.instance.dev2.disk_gib":   "16",
		"driver.box.instance.dev3.state":      "Stopped",
		"driver.box.instance.dev3.task":       "none",
		"driver.box.instance.dev3.cpus":       "not a number",
		"driver.box.arch":                     "x86_64",
	}
	instances := boxInstancesFromAttributes(attrs)
	require.Len(t, instances, 3)
	require.Equal(t, "dev1", instances[0].BoxID)
	require.True(t, instances[0].Task)
	require.Equal(t, 2, instances[0].CPUs)
	require.Equal(t, int64(1024), instances[0].MemoryMiB)
	require.False(t, instances[0].RunningUncounted(), "a VM with a task is accounted for")

	require.Equal(t, "dev2", instances[1].BoxID)
	require.False(t, instances[1].Task)
	require.Equal(t, int64(3072), instances[1].MemoryMiB)
	require.True(t, instances[1].RunningUncounted())

	require.Equal(t, "dev3", instances[2].BoxID)
	require.Equal(t, "Stopped", instances[2].State)
	require.Equal(t, 0, instances[2].CPUs, "an unreadable number is zero")
	require.False(t, instances[2].RunningUncounted(), "a stopped VM takes no memory")

	require.Empty(t, boxInstancesFromAttributes(map[string]string{"driver.box.arch": "x86_64"}))
}

// TestBuildCapacitySubtractsUncountedVMs is A3: a running VM without a running
// allocation holds vCPUs and memory that Nomad's accounting does not cover.
func TestBuildCapacitySubtractsUncountedVMs(t *testing.T) {
	cluster := newFakeCluster()
	node := cluster.addNode("node-1", "node-1", "amd64", 12000, 11961, 150000)
	// Two running VMs, only one of them owned by a task.
	withInstance(node, "counted", "Running", "running", 2, 1174, 8)
	withInstance(node, "lost", "Running", "none", 4, 3072, 16)
	withInstance(node, "stopped", "Stopped", "none", 2, 1024, 8)

	alloc := &api.Allocation{
		ID:           "alloc-1",
		JobID:        "box-counted",
		ClientStatus: "running",
		AllocatedResources: &api.AllocatedResources{
			Tasks: map[string]*api.AllocatedTaskResources{
				"box": {Cpu: api.AllocatedCpuResources{CpuShares: 2000}, Memory: api.AllocatedMemoryResources{MemoryMB: 1174}},
			},
		},
	}
	cap := BuildCapacity(node, []*api.Allocation{alloc})
	require.Len(t, cap.Instances, 3)
	require.Len(t, cap.Uncounted, 1)
	require.Equal(t, "lost", cap.Uncounted[0].BoxID)
	require.Equal(t, int64(3072), cap.UncountedMemoryMiB)
	require.Equal(t, int64(4000), cap.UncountedCPUShares)

	// The allocation is already counted by Nomad; the lost VM is not.
	require.Equal(t, int64(11961-1174-3072), cap.Memory.Free)
	require.Equal(t, int64(12000-2000-4000), cap.CPU.Free)

	// The node is still usable, and the numbers are in its JSON.
	require.Empty(t, cap.NodeProblem(PlacementRequest{}))
}

// TestBuildCapacityUncountedNeverGoesNegative covers a node whose VMs alone
// exceed what Nomad thinks is free.
func TestBuildCapacityUncountedNeverGoesNegative(t *testing.T) {
	cluster := newFakeCluster()
	node := cluster.addNode("node-1", "node-1", "amd64", 2000, 2048, 50000)
	withInstance(node, "lost", "Running", "none", 8, 8192, 32)
	withInstance(node, "broken", "Broken", "none", 2, 1024, 8)

	cap := BuildCapacity(node, nil)
	require.Equal(t, int64(0), cap.Memory.Free)
	require.Equal(t, int64(0), cap.CPU.Free)
	require.Len(t, cap.Uncounted, 1, "a broken instance is down, not uncounted")
	require.Contains(t, cap.NodeProblem(PlacementRequest{CPUs: 2, MemoryMiB: 1024}), "memory free")
}

// TestBuildCapacityReportsMemoryLimit keeps the operator's decision visible: a
// node with memory_limit = "off" runs boxes without a cap.
func TestBuildCapacityReportsMemoryLimit(t *testing.T) {
	cluster := newFakeCluster()
	node := cluster.addNode("node-1", "node-1", "amd64", 12000, 11961, 150000)
	node.Attributes["driver.box.memory_limit"] = "off"
	require.Equal(t, "off", BuildCapacity(node, nil).Driver.MemoryLimit)
}

// TestBuildCapacityIgnoresKeysOfAGoneBox: Nomad never removes a driver
// attribute from node.Attributes when the driver stops sending it, so a deleted
// box stays there. The driver info holds the last fingerprint whole, and that
// is what capacity reads.
func TestBuildCapacityIgnoresKeysOfAGoneBox(t *testing.T) {
	cluster := newFakeCluster()
	node := cluster.addNode("node-1", "node-1", "amd64", 12000, 11961, 150000)
	withInstance(node, "gone", "Running", "none", 4, 3072, 16)
	node.Attributes[boxDiskAttrPrefix+"gone"+boxDiskUsedSuffix] = "1048576"
	node.Attributes[boxDiskAttrPrefix+"gone"+boxDiskVirtualSuffix] = "8589934592"

	live := map[string]string{}
	for k, v := range node.Attributes {
		if strings.HasPrefix(k, boxAttrPrefix) && !strings.Contains(k, ".gone.") {
			live[k] = v
		}
	}
	live["driver.box.orphans"] = "lost"
	node.Drivers[boxDriverName].Attributes = live

	cap := BuildCapacity(node, nil)
	require.Empty(t, cap.Instances, "the gone box is not a VM on the node")
	require.Empty(t, cap.Uncounted, "and its memory is not taken off the free memory")
	require.Empty(t, cap.Disks)
	require.Equal(t, int64(11961), cap.Memory.Free)
	require.Equal(t, "lost", cap.Driver.Orphans)
	require.Equal(t, "amd64", cap.Arch, "the node's own attributes are still read")
	require.NotEmpty(t, cap.Driver.Version)
}
