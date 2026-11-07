package cli

// The pass that deletes the disks of boxes whose term ran out: every case in
// which it must not delete anything, the clock itself, and the race with a
// `box start`.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// reapNow is the moment the fake clock holds in these tests, with its step at
// zero so that a pass sees one time.
var reapNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// newReapApp returns a test app whose clock does not move.
func newReapApp(t *testing.T) *testApp {
	t.Helper()
	ta := newTestApp(t, indexJSON)
	ta.clock.step = 0
	return ta
}

// stopForReap leaves a box the way `box stop` and a crash leave it: the job is
// stopped and nothing of it runs, and the disk and the description stay.
func (ta *testApp) stopForReap(t *testing.T, id string) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	job, ok := ta.cluster.jobs[BoxJobName(id)]
	require.True(t, ok, "the box has a job")
	job.Stop = boolPtr(true)
	job.Status = strPtr("dead")
	for _, alloc := range ta.cluster.allocs {
		if alloc.JobID == BoxJobName(id) {
			alloc.ClientStatus = "complete"
			alloc.DesiredStatus = "stop"
		}
	}
}

// setReapClock writes the clock of a box, the way an earlier pass would have.
func (ta *testApp) setReapClock(t *testing.T, id string, at time.Time) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	ta.cluster.variableSeq++
	ta.cluster.variables[BoxStoppedVariable(id)] = &api.Variable{
		Path:        BoxStoppedVariable(id),
		Items:       map[string]string{boxStoppedItem: at.UTC().Format(time.RFC3339)},
		ModifyIndex: ta.cluster.variableSeq,
		CreateIndex: ta.cluster.variableSeq,
	}
}

// reapClock reads the clock of a box out of the fake cluster.
func (ta *testApp) reapClock(t *testing.T, id string) (string, bool) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	v, ok := ta.cluster.variables[BoxStoppedVariable(id)]
	if !ok {
		return "", false
	}
	return v.Items[boxStoppedItem], true
}

// requireDiskKept checks that a box kept its disk, its job and its description.
func (ta *testApp) requireDiskKept(t *testing.T, id string) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	found := false
	for _, volume := range ta.cluster.volumes {
		if volume.Name == BoxVolumeName(id) {
			found = true
		}
	}
	require.True(t, found, "the disk of box %s is still there", id)
	require.Contains(t, ta.cluster.jobs, BoxJobName(id), "the job of box %s is still there", id)
}

// TestReapStartsTheClockOfAStoppedBox: the first pass that sees a box not running
// writes the time and deletes nothing.
func TestReapStartsTheClockOfAStoppedBox(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "starts its term")
	require.Contains(t, ta.errOut.String(), reapNow.Add(boxStoppedTermDefault).Format(time.RFC3339))

	clock, ok := ta.reapClock(t, "dev1")
	require.True(t, ok, "the pass wrote the clock")
	require.Equal(t, reapNow.Format(time.RFC3339), clock)
	ta.requireDiskKept(t, "dev1")

	// A second pass a minute later keeps it, and does not move the clock.
	ta.clock.advance(time.Minute)
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "its disk goes at")
	clock, ok = ta.reapClock(t, "dev1")
	require.True(t, ok)
	require.Equal(t, reapNow.Format(time.RFC3339), clock, "a later pass never moves the clock")
	ta.requireDiskKept(t, "dev1")
}

// TestReapDeletesABoxWhoseTermRanOut: 24 hours after the first pass saw it
// stopped, the disk goes with the job, the description and the clock.
func TestReapDeletesABoxWhoseTermRanOut(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "deleted box dev1")
	require.Contains(t, out, "8 GiB of disk")
	require.Contains(t, out, "stopped 1d01h")

	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Empty(t, ta.cluster.volumes, "the disk is gone")
	require.NotContains(t, ta.cluster.jobs, "box-dev1", "the job is purged")
	require.NotContains(t, ta.cluster.variables, BoxStoppedVariable("dev1"), "the clock goes with the disk")
}

// TestReapDeletesAnOrphanDisk: a volume nothing describes is not running, and
// gets the same term.
func TestReapDeletesAnOrphanDisk(t *testing.T) {
	ta := newReapApp(t)
	ta.cluster.addVolume("orphan1", "node-1", 8<<30)
	ta.setReapClock(t, "orphan1", reapNow.Add(-30*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "deleted box orphan1")
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Empty(t, ta.cluster.volumes)
}

// TestReapKeepsTheDiskOfABoxThatRuns: a running box is not touched, however long
// it idles, and a clock it had is cleared.
func TestReapKeepsTheDiskOfABoxThatRuns(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "1 running or starting")
	_, ok := ta.reapClock(t, "dev1")
	require.False(t, ok, "a box that runs is not on a term")
	ta.requireDiskKept(t, "dev1")
}

// TestReapKeepsTheDiskOfABoxWithALiveAllocation: Nomad keeps the client status an
// allocation reached, so a stopped job can still hold a live allocation; the
// disk is not deleted under it.
func TestReapKeepsTheDiskOfABoxWithALiveAllocation(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	// The job is stopped and the allocation is still live: a stop that has not
	// finished.
	ta.cluster.mu.Lock()
	for _, alloc := range ta.cluster.allocs {
		if alloc.JobID == BoxJobName("dev1") {
			alloc.ClientStatus = "running"
			alloc.DesiredStatus = "stop"
		}
	}
	ta.cluster.mu.Unlock()
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "it is still stopping")
	require.Contains(t, ta.errOut.String(), "allocation(s) of it are live")
	ta.requireDiskKept(t, "dev1")
}

// TestReapKeepsTheDiskOfAPendingBox: a box that is starting, or waiting for room,
// is not stopped.
func TestReapKeepsTheDiskOfAPendingBox(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.cluster.mu.Lock()
	ta.cluster.jobs["box-dev1"].Status = strPtr("pending")
	ta.cluster.jobs["box-dev1"].Stop = boolPtr(false)
	for allocID, alloc := range ta.cluster.allocs {
		if alloc.JobID == BoxJobName("dev1") {
			alloc.ClientStatus = "pending"
			alloc.DesiredStatus = "run"
			_ = allocID
		}
	}
	ta.cluster.mu.Unlock()
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "0 deleted")
	require.Contains(t, ta.errOut.String(), "1 running or starting")
	ta.requireDiskKept(t, "dev1")
}

// TestReapNeverDeletesWhatItCannotRead is the rule that makes the pass safe: an
// unreadable state is never "stopped".
func TestReapNeverDeletesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(ta *testApp)
		want   string
	}{
		{
			name:   "the job cannot be read",
			break_: func(ta *testApp) { ta.cluster.jobErr = errors.New("Nomad is not answering") },
			want:   "cannot read the job of box dev1",
		},
		{
			name:   "the allocations cannot be read",
			break_: func(ta *testApp) { ta.cluster.jobAllocsErr = errors.New("Nomad is not answering") },
			want:   "cannot read the allocations of box dev1",
		},
		{
			name:   "the node cannot be read",
			break_: func(ta *testApp) { ta.cluster.nodeErr = map[string]error{"node-1": errors.New("no route to node")} },
			want:   "its node node-1 cannot be read",
		},
		{
			name:   "the node is down",
			break_: func(ta *testApp) { ta.cluster.setNodeStatus("node-1", "down") },
			want:   "is down",
		},
		{
			name: "the clock cannot be read",
			break_: func(ta *testApp) {
				ta.cluster.variableReadErrPath = map[string]error{
					BoxStoppedVariable("dev1"): errors.New("Permission denied"),
				}
			},
			want: "cannot read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ta := newReapApp(t)
			ta.addBox(t, "dev1")
			ta.stopForReap(t, "dev1")
			ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
			tc.break_(ta)

			require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), tc.want, ta.errOut.String())
			ta.requireDiskKept(t, "dev1")
		})
	}
}

// TestReapFailsWhenTheClusterCannotBeRead: without the volumes there is nothing
// to pass over, and the command says so instead of deleting something.
func TestReapFailsWhenTheClusterCannotBeRead(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
	ta.cluster.volumeListErr = errors.New("Permission denied")

	require.NotEqual(t, ExitOK, ta.run(t, "reap"))
	require.Contains(t, ta.errOut.String(), "cannot list the host volumes")
	ta.requireDiskKept(t, "dev1")
}

// TestReapKeepsTheDiskDuringABake: a bake stops the box and can take hours, and
// its disk is what the bake reads.
func TestReapKeepsTheDiskDuringABake(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
	ta.addWorkJob(t, BoxBakeJobName("dev1"), map[string]string{boxWorkMarker: "bake", "box_id": "dev1"})

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "a bake of it is running")
	ta.requireDiskKept(t, "dev1")
}

// TestReapKeepsTheDiskDuringAPublishOnItsNode: a publish reads an image from the
// node the disk is on.
func TestReapKeepsTheDiskDuringAPublishOnItsNode(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
	ta.addWorkJob(t, BoxPublishJobName("1.0.0"), map[string]string{boxWorkMarker: "publish", "image": "1.0.0"})

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "a publish on its node is running")
	ta.requireDiskKept(t, "dev1")
}

// TestReapKeepsTheDiskWhenAStartClaimsTheClockFirst: `box start` deletes the
// clock before it registers the job, so a checked delete that finds it gone is a
// box that came back.
func TestReapKeepsTheDiskWhenAStartClaimsTheClockFirst(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
	ta.cluster.onVariableUpdateChecked = func(c *fakeCluster, path string) {
		// The start got there first: it cleared the clock.
		delete(c.variables, path)
	}

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "a box start cleared its clock")
	ta.requireDiskKept(t, "dev1")
}

// TestStartRefusesABoxAPassIsDeleting: a pass that claimed the clock owns the
// box. The start must not register a job on a disk that is going, and it says why.
func TestStartRefusesABoxAPassIsDeleting(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))
	ta.cluster.mu.Lock()
	ta.cluster.variables[BoxStoppedVariable("dev1")].Items[boxStoppedStateItem] = boxStoppedStateDeleting
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitError, ta.run(t, "start", "dev1", "--wait=false"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "a reap pass is deleting the disk of box dev1")
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.True(t, *ta.cluster.jobs["box-dev1"].Stop, "no job was registered")
}

// TestStartRefusesWhenTheClockChangedUnderIt: the delete of the clock is checked,
// so a pass that claimed it between the read and the delete is not overridden.
func TestStartRefusesWhenTheClockChangedUnderIt(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))
	claimed := false
	ta.cluster.onVariableDeleteChecked = func(c *fakeCluster, path string) {
		if claimed {
			return
		}
		// The pass claimed the clock first: same path, a new index, "deleting".
		claimed = true
		v := c.variables[path]
		c.variableSeq++
		v.Items[boxStoppedStateItem] = boxStoppedStateDeleting
		v.ModifyIndex = c.variableSeq
	}

	require.Equal(t, ExitError, ta.run(t, "start", "dev1", "--wait=false"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "a reap pass is deleting the disk of box dev1")
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.True(t, *ta.cluster.jobs["box-dev1"].Stop, "no job was registered")
}

// TestReapKeepsTheDiskWhenTheBoxComesBack: the state is read again immediately
// before the disk goes, so a start that registered the job between two reads is
// seen.
func TestReapKeepsTheDiskWhenTheBoxComesBack(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
	ta.cluster.onJobRead = func(c *fakeCluster, id string, call int) {
		if call != 2 {
			return
		}
		// `box start` registered the job again, and an allocation runs.
		job := c.jobs[id]
		job.Stop = boolPtr(false)
		job.Status = strPtr("running")
		c.allocSeq++
		c.allocs["alloc-back"] = &api.Allocation{
			ID: "alloc-back", JobID: id, NodeID: "node-1", NodeName: "node-1",
			ClientStatus: "running", DesiredStatus: "run", CreateIndex: uint64(c.allocSeq),
			TaskStates: map[string]*api.TaskState{boxTaskName: {State: "running"}},
		}
	}

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "it came back while this pass looked")
	ta.requireDiskKept(t, "dev1")
}

// TestReapOneBoxFailingDoesNotStopThePass: the other boxes are still deleted, and
// the command exits 1 so the job's log shows it.
func TestReapOneBoxFailingDoesNotStopThePass(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.addBox(t, "dev2")
	ta.stopForReap(t, "dev1")
	ta.stopForReap(t, "dev2")
	ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
	ta.setReapClock(t, "dev2", reapNow.Add(-40*time.Hour))
	ta.cluster.onDeleteHostVolume = func(c *fakeCluster, id string) error {
		if volume := c.volumes[id]; volume != nil && volume.Name == BoxVolumeName("dev1") {
			return errors.New("the volume plugin did not answer")
		}
		return nil
	}

	require.Equal(t, ExitError, ta.run(t, "reap"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "box dev1: its disk could not be deleted")
	require.Contains(t, out, "deleted box dev2")
	require.Contains(t, out, "1 box(es) could not be deleted")

	// The disk of the box that failed is still there, and its clock was put back:
	// the next pass tries again instead of waiting another 24 hours.
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.NotContains(t, ta.cluster.jobs, "box-dev2")
	require.Len(t, ta.cluster.volumes, 1, "the disk of the box that failed is still there")
	clock, ok := ta.cluster.variables[BoxStoppedVariable("dev1")]
	require.True(t, ok, "the clock of the box that failed was put back")
	require.Equal(t, reapNow.Add(-40*time.Hour).Format(time.RFC3339), clock.Items[boxStoppedItem])
}

// TestReapDryRunDeletesNothing: the pass says what it would delete and when, and
// changes nothing -- not even the clock of a box it has not seen before.
func TestReapDryRunDeletesNothing(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.addBox(t, "dev2")
	ta.stopForReap(t, "dev1")
	ta.stopForReap(t, "dev2")
	ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))
	ta.setReapClock(t, "dev2", reapNow.Add(-3*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap", "--dry-run"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "box dev1: 8 GiB of disk, stopped 1d01h ago: this pass would delete it")
	require.Contains(t, out, "box dev2: 8 GiB of disk, stopped 3h00m ago: its disk goes at")
	require.Contains(t, out, "nothing was deleted")
	ta.requireDiskKept(t, "dev1")
	ta.requireDiskKept(t, "dev2")

	// A box no pass has seen is not given a time, and its clock is not written by
	// a dry run.
	ta.addBox(t, "dev3")
	ta.stopForReap(t, "dev3")
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "--dry-run"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "box dev3: 8 GiB of disk, not running; this pass starts its term")
	_, ok := ta.reapClock(t, "dev3")
	require.False(t, ok, "a dry run writes no clock")
}

// TestReapDryRunChangesNothing: a dry run is what an operator runs to look at a
// pass before trusting it, so it asks Nomad only questions, whatever state a box
// is in. The list names what may be asked, so a write added on any path fails
// here without this test having to know about it.
func TestReapDryRunChangesNothing(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "running")
	ta.setReapClock(t, "running", reapNow.Add(-40*time.Hour))
	ta.addBox(t, "due")
	ta.stopForReap(t, "due")
	ta.setReapClock(t, "due", reapNow.Add(-25*time.Hour))
	ta.addBox(t, "waiting")
	ta.stopForReap(t, "waiting")
	ta.setReapClock(t, "waiting", reapNow.Add(-3*time.Hour))
	ta.addBox(t, "unseen")
	ta.stopForReap(t, "unseen")
	due, _ := ta.reapClock(t, "due")
	running, _ := ta.reapClock(t, "running")

	ta.cluster.callsMu.Lock()
	ta.cluster.calls = nil
	ta.cluster.callsMu.Unlock()
	require.Equal(t, ExitOK, ta.run(t, "reap", "--dry-run"), ta.errOut.String())

	ta.cluster.callsMu.Lock()
	calls := append([]string(nil), ta.cluster.calls...)
	ta.cluster.callsMu.Unlock()
	require.NotEmpty(t, calls)
	readOnly := []string{"HostVolumes", "HostVolume ", "JobList", "Job ", "JobAllocations ", "Allocations",
		"Allocation ", "NodeInfo ", "NodeList", "VariableRead "}
	for _, call := range calls {
		asks := false
		for _, prefix := range readOnly {
			asks = asks || strings.HasPrefix(call, prefix)
		}
		require.True(t, asks, "a dry run must only read, and it called %s", call)
	}

	// What an operator would see afterwards: the clocks say what they said.
	got, ok := ta.reapClock(t, "running")
	require.True(t, ok, "a dry run leaves the clock of a box that runs")
	require.Equal(t, running, got)
	got, ok = ta.reapClock(t, "due")
	require.True(t, ok)
	require.Equal(t, due, got)
}

// TestReapReadsAClockItCannotParseAsAFailure: a clock that is not a time is not a
// missing clock, and not a zero one either.
func TestReapReadsAClockItCannotParseAsAFailure(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.cluster.mu.Lock()
	ta.cluster.variableSeq++
	ta.cluster.variables[BoxStoppedVariable("dev1")] = &api.Variable{
		Path:        BoxStoppedVariable("dev1"),
		Items:       map[string]string{boxStoppedItem: "yesterday"},
		ModifyIndex: ta.cluster.variableSeq,
	}
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "not an RFC3339 time")
	ta.requireDiskKept(t, "dev1")
}

// TestReapIgnoresADiskThatIsNotReady: a volume that is being created or deleted is
// not a box that stopped.
func TestReapIgnoresADiskThatIsNotReady(t *testing.T) {
	ta := newReapApp(t)
	volume := ta.cluster.addVolume("dev1", "node-1", 8<<30)
	ta.cluster.mu.Lock()
	volume.State = api.HostVolumeStatePending
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "is pending")
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Len(t, ta.cluster.volumes, 1)
}

// TestReapIgnoresAVolumeThatIsNotABoxDisk: a host volume of somebody else is not
// a box.
func TestReapIgnoresAVolumeThatIsNotABoxDisk(t *testing.T) {
	ta := newReapApp(t)
	ta.cluster.mu.Lock()
	ta.cluster.volumeSeq++
	ta.cluster.volumes["vol-other"] = &api.HostVolume{
		ID: "vol-other", Name: "data", NodeID: "node-1", State: api.HostVolumeStateReady,
	}
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "0 box disks")
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Len(t, ta.cluster.volumes, 1)
}

// TestReapJSON is the machine-readable form of a pass.
func TestReapJSON(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "reap", "--json"), ta.out.String())
	out := ta.out.String()
	require.Contains(t, out, `"deleted": 1`)
	require.Contains(t, out, `"id": "dev1"`)
	require.Contains(t, out, `"outcome": "deleted"`)
	require.Contains(t, out, `"disk_gib": 8`)
}

// addWorkJob registers a bake or a publish job with a live allocation on node-1,
// which is what "a bake is running" looks like to a pass.
func (ta *testApp) addWorkJob(t *testing.T, jobID string, meta map[string]string) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	job := &api.Job{ID: strPtr(jobID), Name: strPtr(jobID), Type: strPtr("batch"), Meta: meta}
	job.Status = strPtr("running")
	ta.cluster.jobs[jobID] = job
	ta.cluster.runBox(job)
}

// TestStartClearsTheClock: a box that comes back is not on a term any more.
func TestStartClearsTheClock(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-20*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "start", "dev1", "--wait=false"), ta.errOut.String())
	_, ok := ta.reapClock(t, "dev1")
	require.False(t, ok, "the start deleted the clock")
}

// TestRemoveClearsTheClock: a box created later with the same id must not inherit
// the clock of the one that was deleted.
func TestRemoveClearsTheClock(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-20*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())
	_, ok := ta.reapClock(t, "dev1")
	require.False(t, ok, "rm deleted the clock with the disk")
}

// TestCreateClearsAClockLeftByAnOlderBox: the id was used before, and the clock of
// that box survived a removal that could not finish.
func TestCreateClearsAClockLeftByAnOlderBox(t *testing.T) {
	// A moving clock: `box create` waits for its box, and a clock that does not
	// move never ends that wait.
	ta := newTestApp(t, indexJSON)
	ta.setReapClock(t, "dev1", reapNow.Add(-30*time.Hour))
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }

	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512",
		"--disk-gib", "4"), ta.errOut.String())
	_, ok := ta.reapClock(t, "dev1")
	require.False(t, ok, "the new box does not inherit the old clock")
}

// TestAFrozenTestClockCannotSpinAWait: the guard that would have caught the run
// that ate the machine. A command that waits with a clock that does not move used
// to loop without end, and the fake cluster kept every call; now the sleep that
// cannot end fails the test with a stack.
func TestAFrozenTestClockCannotSpinAWait(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")

	require.PanicsWithValue(t, testWaitSpinPanic, func() {
		_ = ta.run(t, "start", "dev1")
	})
}

// TestListShowsTheTimeLeft: `box ls` says when the disk of a stopped box goes, and
// says so when no pass has seen the box yet instead of inventing a time.
func TestListShowsTheTimeLeft(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.addBox(t, "dev2")
	ta.addBox(t, "dev3")
	ta.stopForReap(t, "dev1")
	ta.stopForReap(t, "dev2")
	ta.setReapClock(t, "dev1", reapNow.Add(-1*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	// The table is what `box ls` writes, and the lines about a box are notes.
	table := ta.out.String()
	require.Contains(t, table, "DISK UNTIL")
	require.Contains(t, table, "23h", "the time left of dev1 is shown")
	require.Contains(t, table, "not seen", "dev2 has no clock, and says so")
	require.Contains(t, ta.errOut.String(), "no reap pass has seen dev2 not running yet")

	// The JSON carries it as fields of its own.
	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--json"), ta.out.String())
	require.Contains(t, ta.out.String(), `"removes_at": "2026-11-03T11:00:00Z"`)
	require.Contains(t, ta.out.String(), `"stopped_at": "2026-11-02T11:00:00Z"`)
}

// TestStatusShowsTheDiskTerm: `box status` says when the disk goes, or that no
// pass has seen the box stopped yet.
func TestStatusShowsTheDiskTerm(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")

	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no reap pass has seen this box stopped yet")

	ta.setReapClock(t, "dev1", reapNow.Add(-1*time.Hour))
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "deleted at 2026-11-03T11:00:00Z")
	require.Contains(t, ta.errOut.String(), "in 23h")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"), ta.out.String())
	require.Contains(t, ta.out.String(), `"removes_at": "2026-11-03T11:00:00Z"`)
}

// TestCapacityShowsTheTimeLeft: the disk lines of `box capacity` say when a disk
// that is not in use goes.
func TestCapacityShowsTheTimeLeft(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-2*time.Hour))

	require.Equal(t, ExitOK, ta.run(t, "capacity"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "dev1 is stopped")
	require.Contains(t, out, "its disk is deleted in 22h")

	// The same term is a field of the disk under --json.
	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "capacity", "--json"), ta.out.String())
	require.Contains(t, ta.out.String(), `"removes_at": "2026-11-03T10:00:00Z"`)
	require.Contains(t, ta.out.String(), `"stopped_at": "2026-11-02T10:00:00Z"`)
}

// TestReapHelpAndUsage: the command takes no arguments and has a flag.
func TestReapHelpAndUsage(t *testing.T) {
	ta := newReapApp(t)
	require.Equal(t, ExitUsage, ta.run(t, "reap", "extra"), ta.errOut.String())
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "help", "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "--dry-run")
	require.Contains(t, ta.errOut.String(), "box reap term", "the help says how the term is read and set")
}

// TestListDoesNotShowAJobThatIsNotABox: a job whose name starts with box- is not
// a box unless its meta says so. The periodic reap job is named outside box-<id>
// for the same reason: a box job is box-<id>, and box-reap would be a box of the
// id "reap".
func TestListDoesNotShowAJobThatIsNotABox(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.cluster.mu.Lock()
	ta.cluster.jobs["reap-boxes"] = &api.Job{ID: strPtr("reap-boxes"), Name: strPtr("reap-boxes"),
		Type: strPtr("batch"), Status: strPtr("running")}
	ta.cluster.jobs["box-lookalike"] = &api.Job{ID: strPtr("box-lookalike"), Name: strPtr("box-lookalike"),
		Type: strPtr("batch"), Status: strPtr("dead"), Stop: boolPtr(true)}
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	out := ta.out.String()
	require.Contains(t, out, "dev1")
	require.NotContains(t, out, "reap-boxes")
	require.NotContains(t, out, "lookalike")
}

// TestAStartThatCannotRenderLeavesTheClock: a start is refused before the clock
// is touched when the box's job cannot be rendered. The box is still stopped, so
// the next pass must find its old term, not start a new one and give the disk
// another 24 hours.
func TestAStartThatCannotRenderLeavesTheClock(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	at := reapNow.Add(-20 * time.Hour)
	ta.setReapClock(t, "dev1", at)
	ta.cluster.mu.Lock()
	ta.cluster.jobs["box-dev1"].TaskGroups[0].Tasks[0].Config["image"] = nil
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitUsage, ta.run(t, "start", "dev1", "--wait=false"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no image is set for the box")
	clock, ok := ta.reapClock(t, "dev1")
	require.True(t, ok, "a start that changed nothing keeps the clock")
	require.Equal(t, at.Format(time.RFC3339), clock)
}
