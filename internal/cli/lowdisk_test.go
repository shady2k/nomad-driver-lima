package cli

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

const gib = int64(1) << 30

// roomNode is a node the box driver runs on, with the disk room it reports set by
// the test.
func roomNode(name string, free, reserve int64) *NodeCapacity {
	return &NodeCapacity{
		NodeID: name, Name: name, Arch: "amd64", Status: "ready", Eligible: true,
		Driver:        DriverInfo{Installed: true},
		Memory:        ResourceUsage{Total: 16000, Free: 12000},
		CPU:           ResourceUsage{Total: 8000, Free: 7000},
		Disk:          ResourceUsage{Total: 200000, Free: 190000},
		DiskFreeBytes: free, DiskFreeKnown: free >= 0, DiskReserveBytes: reserve, DiskTotalBytes: 400 * gib,
		VolumePlugins: []VolumePluginInfo{{ID: boxVolumePluginID}},
	}
}

// TestLowDiskKnownVersusZero: a free space that was not reported is not a full
// node, and a zero that was reported is.
func TestLowDiskKnownVersusZero(t *testing.T) {
	cases := []struct {
		name    string
		free    int64
		reserve int64
		known   bool
		low     bool
	}{
		{name: "room above the reserve", free: 50 * gib, reserve: 20 * gib, known: true, low: false},
		{name: "below the reserve", free: 10 * gib, reserve: 20 * gib, known: true, low: true},
		{name: "exactly the reserve is not below it", free: 20 * gib, reserve: 20 * gib, known: true, low: false},
		{name: "a reported zero is a full node", free: 0, reserve: 20 * gib, known: true, low: true},
		{name: "no reserve is never low", free: 1, reserve: 0, known: true, low: false},
		{name: "an unreported free space is unknown, not full", free: -1, reserve: 20 * gib, known: false, low: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := roomNode("n", tc.free, tc.reserve)
			if !tc.known {
				node.DiskFreeBytes, node.DiskFreeKnown = 0, false
			}
			require.Equal(t, tc.known, node.DiskFreeKnown)
			require.Equal(t, tc.low, node.LowDisk())
			if tc.known {
				require.NotEqual(t, "unknown", node.freeDiskCell())
			} else {
				require.Equal(t, "unknown", node.freeDiskCell())
			}
		})
	}
}

// TestDiskRoomAttributesAreNoBox: free, total and reserve begin with the prefix of
// the per-box disk attributes, and a reader that took them for a box would invent
// a box called "free".
func TestDiskRoomAttributesAreNoBox(t *testing.T) {
	attrs := map[string]string{
		"driver.box.disk.free_bytes":             strconv.FormatInt(40*gib, 10),
		"driver.box.disk.total_bytes":            strconv.FormatInt(400*gib, 10),
		"driver.box.disk.reserve_bytes":          strconv.FormatInt(20*gib, 10),
		"driver.box.disk.image_cache_free_bytes": strconv.FormatInt(30*gib, 10),
		"driver.box.disk.used_bytes":             "1048576",
		"driver.box.disk.virtual_bytes":          "8589934592",
		"driver.box.disk.dev1.used_bytes":        "2097152",
		"driver.box.disk.dev1.virtual_bytes":     "4294967296",
	}
	disks, used, virtual := boxDisksFromAttributes(attrs)
	require.Len(t, disks, 1)
	require.Equal(t, "dev1", disks[0].BoxID)
	require.Equal(t, int64(1<<20), used)
	require.Equal(t, int64(8)<<30, virtual)

	node := &api.Node{ID: "node-1", Name: "node-1", Status: api.NodeStatusReady,
		SchedulingEligibility: api.NodeSchedulingEligible, Attributes: attrs}
	capacity := BuildCapacity(node, nil)
	require.True(t, capacity.DiskFreeKnown)
	require.Equal(t, int64(40*gib), capacity.DiskFreeBytes)
	require.Equal(t, int64(20*gib), capacity.DiskReserveBytes)
	require.Equal(t, int64(400*gib), capacity.DiskTotalBytes)
	require.Len(t, capacity.Disks, 1, "the four filesystem attributes are no boxes")
}

// TestChooseNodeRefusesALowNode: a node below its reserve takes no new box and
// says why; a node whose free space is unknown is not refused; and one full node
// does not stop a box from going to another.
func TestChooseNodeRefusesALowNode(t *testing.T) {
	low := roomNode("low", 5*gib, 20*gib)
	healthy := roomNode("healthy", 100*gib, 20*gib)
	unknown := roomNode("unknown", -1, 20*gib)

	chosen, _, err := ChooseNode([]*NodeCapacity{low, healthy}, PlacementRequest{Arch: "amd64", CPUs: 2, MemoryMiB: 1024, DiskGiB: 8})
	require.NoError(t, err)
	require.Equal(t, "healthy", chosen.Name)

	// A node that does not report its free space is not a full node.
	chosen, _, err = ChooseNode([]*NodeCapacity{unknown}, PlacementRequest{Arch: "amd64", CPUs: 2, MemoryMiB: 1024, DiskGiB: 8})
	require.NoError(t, err)
	require.Equal(t, "unknown", chosen.Name)

	// Only a low node left: refused, with the numbers and what to do, and as a
	// refusal that waiting can change.
	chosen, problems, err := ChooseNode([]*NodeCapacity{low}, PlacementRequest{Arch: "amd64", CPUs: 2, MemoryMiB: 1024, DiskGiB: 8})
	require.Nil(t, chosen)
	require.Error(t, err)
	require.Contains(t, err.Error(), "low on disk")
	require.Contains(t, err.Error(), "5.0 GiB is free")
	require.Contains(t, err.Error(), "20.0 GiB in reserve")
	require.Contains(t, err.Error(), "box rm <id> --yes")
	require.True(t, noRoom(err), "a low node is room another box gives back")
	require.Len(t, problems, 1)

	// An unknown free space never refuses, whichever node it is asked about.
	chosen, _, err = ChooseNode([]*NodeCapacity{unknown}, PlacementRequest{Node: "unknown", Arch: "amd64", CPUs: 2, MemoryMiB: 1024, DiskGiB: 8})
	require.NoError(t, err)
	require.Equal(t, "unknown", chosen.Name)
}

// TestChooseNodeAllowsAThinDisk: a box disk is a qcow2 that grows on demand, so a
// --disk-gib larger than what is free now is not a refusal; the free space the
// node keeps is.
func TestChooseNodeAllowsAThinDisk(t *testing.T) {
	node := roomNode("n", 30*gib, 20*gib)
	// 100 GiB fits Nomad's scheduling figure and not the 30 GiB the node really
	// has: a disk is thin, so that stays allowed.
	chosen, _, err := ChooseNode([]*NodeCapacity{node}, PlacementRequest{Arch: "amd64", CPUs: 2, MemoryMiB: 1024, DiskGiB: 100})
	require.NoError(t, err)
	require.Equal(t, "n", chosen.Name, "a thin disk larger than the free space is allowed")
}

// TestCreateRefusesALowNode: the refusal happens before anything is registered, so
// a low node leaves no job and no volume behind.
func TestCreateRefusesALowNode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setNodeDiskRoom("node-1", 0, 400*gib, 20*gib)

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--arch", "amd64", "--cpus", "2", "--memory-mib", "1024", "--disk-gib", "8")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "low on disk")
	require.Contains(t, ta.errOut.String(), "no node can take this box")
	require.Zero(t, callCount(ta, "RegisterJob box-dev1"), "nothing is registered on a low node")
	require.Empty(t, ta.cluster.volumes, "no disk is made on a low node")
	require.Empty(t, ta.cluster.jobs)
}

// TestCreateWaitTreatsALowNodeAsRoom: a low node is a state that can change, so
// `box create --wait` asks again and reports the same refusal with the same exit
// code when the time is up.
func TestCreateWaitTreatsALowNodeAsRoom(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setNodeDiskRoom("node-1", 0, 400*gib, 20*gib)

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--arch", "amd64", "--cpus", "2", "--memory-mib", "1024",
		"--disk-gib", "8", "--wait", "4s")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no room for box dev1 yet")
	require.Contains(t, ta.errOut.String(), "low on disk")
	require.Zero(t, callCount(ta, "RegisterJob box-dev1"))
}

// TestStartRefusesALowNode: a box that is started writes to its disk at once, so a
// node below its reserve refuses the start -- and nothing is changed.
func TestStartRefusesALowNode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.NoError(t, ta.cluster.StopJob(context.Background(), "box-dev1", false))
	ta.cluster.setNodeDiskRoom("node-1", 1*gib, 400*gib, 20*gib)

	rc := ta.run(t, "start", "dev1")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "cannot start")
	require.Contains(t, out, "1.0 GiB free")
	require.Contains(t, out, "20.0 GiB in reserve")
	require.Contains(t, out, "Nothing was changed")
	require.Zero(t, callCount(ta, "RegisterJob box-dev1"), "a refused start registers nothing")
}

// TestResizeRefusesToGrowADiskOnALowNode: only a resize that writes more of the
// disk is refused; one that changes CPU or memory is not.
func TestResizeRefusesToGrowADiskOnALowNode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.setNodeDiskRoom("node-1", 1*gib, 400*gib, 20*gib)

	rc := ta.run(t, "resize", "dev1", "--disk-gib", "16")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "cannot grow its disk")
	require.Zero(t, callCount(ta, "RegisterJob box-dev1"))

	// Memory alone writes nothing new, and a low node runs the box it has.
	rc = ta.run(t, "resize", "dev1", "--memory-mib", "2048")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.NotZero(t, callCount(ta, "RegisterJob box-dev1"))
}

// TestBakeRefusesAReportedZero: a node that reports no free space at all is a full
// node, and a bake there stops the box and fails later. It is refused before the
// stop instead. An unreported number keeps the driver's own check.
func TestBakeRefusesAReportedZero(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.sealMayRun()
	ta.cluster.setNodeDiskRoom("node-1", 0, 400*gib, 20*gib)

	rc := ta.run(t, "bake", "dev1", "--name", "project")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "free where a bake writes the copy")
	require.Zero(t, callCount(ta, "StopJob box-dev1"), "a full node refuses the bake before the stop")
	require.Zero(t, callCount(ta, "RegisterJob box-bake"))

	// A node whose driver has not measured its filesystem says nothing here: the
	// driver's own check is the one that refuses.
	ta2 := newTestApp(t, platformIndexJSON)
	ta2.addBoxSpec(t, goodProjectSpec())
	ta2.sealMayRun()
	ta2.cluster.setNodeDiskRoom("node-1", -1, 400*gib, 20*gib)
	require.NotEqual(t, ExitOK, ta2.run(t, "bake", "dev1", "--name", "project"))
	require.NotContains(t, ta2.errOut.String(), "free where a bake writes the copy")
	require.NotZero(t, callCount(ta2, "RegisterJob box-bake"), "the bake goes on to the driver")
}

// TestStopOfAFailedBoxReturnsAtOnce: Nomad keeps the failed status of an allocation
// whose job was then stopped, and a stop that waited for the word "stopped" waited
// out its whole timeout for it.
func TestStopOfAFailedBoxReturnsAtOnce(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	// The VM fails as the job stops, which is what Nomad leaves behind: the
	// allocation keeps the status it reached, and the job is the thing that says
	// the box is not running any more.
	ta.cluster.stopLeavesAllocFailed = true

	start := time.Now()
	rc := ta.run(t, "stop", "dev1", "--timeout", "3m")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "stop: box dev1 is stopped")
	require.Less(t, time.Since(start), 10*time.Second)
}

// TestAFailedBoxStillShowsFailed: a box that failed and was not stopped is failed.
func TestAFailedBoxStillShowsFailed(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.mu.Lock()
	for _, alloc := range ta.cluster.allocs {
		alloc.ClientStatus = "failed"
	}
	ta.cluster.mu.Unlock()

	rc := ta.run(t, "ls", "--json")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	var doc lsOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Boxes, 1)
	require.Equal(t, "failed", doc.Boxes[0].State)
}

// TestCapacityShowsTheDriversFreeSpace: the text names the number the driver
// reports, says what Nomad's disk figure is, and warns about a low node.
func TestCapacityShowsTheDriversFreeSpace(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setNodeDiskRoom("node-1", 5*gib, 400*gib, 20*gib)

	rc := ta.run(t, "capacity")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	table := ta.out.String()
	require.Contains(t, table, "FREE disk (driver)")
	require.Contains(t, table, "5.0 GiB/400.0 GiB", "the driver's free space and the size of its filesystem")
	require.Contains(t, table, "DISK free/total (scheduler)", "Nomad's figure is named as what it is")

	out := ta.errOut.String()
	require.Contains(t, out, "LOW DISK")
	require.Contains(t, out, "5.0 GiB is free")
	require.Contains(t, out, "20.0 GiB in reserve")
	require.Contains(t, out, "freezes when the free space reaches zero")
	require.Contains(t, out, "not free space", "the summary never calls the scheduler's sum free disk")

	// The JSON keeps Nomad's disk figure and adds the driver's numbers.
	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "capacity", "--json")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	var doc capacityOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Nodes, 1)
	node := doc.Nodes[0]
	require.Equal(t, int64(5*gib), node.DiskFreeBytes)
	require.True(t, node.DiskFreeKnown)
	require.Equal(t, int64(20*gib), node.DiskReserveBytes)
	require.Equal(t, int64(400*gib), node.DiskTotalBytes)
	require.Equal(t, int64(150000), node.Disk.Free, "disk_mib keeps Nomad's arithmetic")
	require.Len(t, doc.Arches, 1)
	require.Equal(t, int64(5*gib), doc.Arches[0].DiskFreeBytes)
}

// TestCapacityNeverShowsAnUnknownFreeSpaceAsHealthy: a driver that did not report
// it says unknown, and no number takes its place.
func TestCapacityNeverShowsAnUnknownFreeSpaceAsHealthy(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setNodeDiskRoom("node-1", -1, 400*gib, 20*gib)

	rc := ta.run(t, "capacity")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.out.String(), "unknown")
	require.NotContains(t, ta.errOut.String(), "LOW DISK")
	require.Contains(t, ta.errOut.String(), "did not report free disk space")

	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "capacity", "--json")
	require.Equal(t, ExitOK, rc)
	var doc capacityOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.False(t, doc.Nodes[0].DiskFreeKnown)
}

// pausedNode publishes what the driver read from qemu for one box of the node.
func pausedNode(t *testing.T, ta *testApp, boxID, state string) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	node := ta.cluster.nodes["node-1"]
	prefix := "driver.box.instance." + boxID
	node.Attributes[prefix+".state"] = "Running"
	node.Attributes[prefix+".task"] = "running"
	node.Attributes[prefix+".cpus"] = "2"
	node.Attributes[prefix+".memory_mib"] = "1024"
	node.Attributes[prefix+".disk_gib"] = "8"
	node.Attributes[prefix+".qemu_status"] = state
}

// TestLsShowsAPausedVM: the STATE column says the VM is paused, and the JSON keeps
// Nomad's state plus qemu's in a field of its own.
func TestLsShowsAPausedVM(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	pausedNode(t, ta, "dev1", "io-error")

	rc := ta.run(t, "ls")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.out.String(), "paused:io-error")
	require.Contains(t, ta.errOut.String(), "qemu has paused its VM (io-error)")

	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "ls", "--json")
	require.Equal(t, ExitOK, rc)
	var doc lsOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "running", doc.Boxes[0].State, "Nomad's state is unchanged: the box is still running")
	require.Equal(t, "io-error", doc.Boxes[0].VMState)
}

// TestStatusShowsAPausedVM: the top of a status says the VM is frozen, with the
// free space of the node, while Nomad still reports the box running.
func TestStatusShowsAPausedVM(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	pausedNode(t, ta, "dev1", "io-error")
	ta.cluster.setNodeDiskRoom("node-1", 3*gib, 400*gib, 20*gib)

	rc := ta.run(t, "status", "dev1")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "PAUSED")
	require.Contains(t, out, "paused by qemu (io-error)")
	require.Contains(t, out, "the host could not write its disk")
	require.Contains(t, out, "3.0 GiB is free on its node")
	require.Contains(t, out, "docs/known-limits.md")
	require.Contains(t, out, "state   running", "Nomad and Lima both go on saying the box runs")
}

// TestStatusSaysTheNodeIsLow: a healthy box on a low node still tells the operator.
func TestStatusSaysTheNodeIsLow(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.setNodeDiskRoom("node-1", 2*gib, 400*gib, 20*gib)

	rc := ta.run(t, "status", "dev1")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "keeps 20.0 GiB in reserve")
	require.Contains(t, ta.errOut.String(), "a new box is refused there")
}

// TestVmStateSentence: what qemu reported is said, and what it did not is not.
func TestVmStateSentence(t *testing.T) {
	require.Empty(t, vmStateSentence("dev1", "", 0, false))
	require.Empty(t, vmStateSentence("dev1", "running", 0, false))

	io := vmStateSentence("dev1", "io-error", 3*gib, true)
	require.Contains(t, io, "paused by qemu (io-error)")
	require.Contains(t, io, "the host could not write its disk")
	require.Contains(t, io, "3.0 GiB is free on its node")
	require.Contains(t, io, "A full host disk is the usual reason")

	// An unknown free space is not in the sentence at all.
	require.NotContains(t, vmStateSentence("dev1", "io-error", 0, false), "is free on its node")

	// A state qemu reported for another reason states no cause.
	paused := vmStateSentence("dev1", "paused", 0, false)
	require.Contains(t, paused, "paused by qemu (paused)")
	require.NotContains(t, paused, "could not write")
	require.NotContains(t, paused, "full disk")

	require.Equal(t, "lost contact; The VM of box dev1 is paused by qemu (paused).",
		withVMNote("lost contact", "the VM of box dev1 is paused by qemu (paused)"))
	require.Equal(t, "lost contact.", withVMNote("lost contact.", ""))
}

// TestStateCell is the STATE column of `box ls`.
func TestStateCell(t *testing.T) {
	require.Equal(t, "running", stateCell(BoxRecord{State: "running"}))
	require.Equal(t, "paused:io-error", stateCell(BoxRecord{State: "running", VMState: "io-error"}))
	require.Equal(t, "-", stateCell(BoxRecord{}))
}

// TestJobStoppedOfAFailedBox: a stopped job with a terminal allocation is stopped
// for whoever asks, and a failed box that was not stopped is failed.
func TestJobStoppedOfAFailedBox(t *testing.T) {
	stopped := true
	alloc := &api.AllocationListStub{ID: "alloc-1", ClientStatus: "failed", CreateIndex: 1,
		TaskStates: map[string]*api.TaskState{boxTaskName: {State: "dead"}}}
	job := &api.Job{ID: strPtr("box-dev1"), Stop: &stopped,
		Meta:       map[string]string{"box": "true", "box_id": "dev1"},
		TaskGroups: []*api.TaskGroup{{Name: strPtr(boxGroupName), Tasks: []*api.Task{{Name: boxTaskName, Driver: "box"}}}}}

	rec := boxRecord(job, []*api.AllocationListStub{alloc}, time.Now())
	require.Equal(t, "stopped", rec.State)

	job.Stop = boolPtr(false)
	rec = boxRecord(job, []*api.AllocationListStub{alloc}, time.Now())
	require.Equal(t, "failed", rec.State)
	require.Contains(t, strings.ToLower(describeAllocState(rec)), "failed")
}

// describeAllocState is the state a person reads in a box record, used by the test
// above to keep the sentence in one place.
func describeAllocState(rec BoxRecord) string { return rec.State }
