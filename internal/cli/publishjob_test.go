package cli

// Every publish is the Nomad job box-store, whatever the version: the job id is
// the name of the store variable, so the task's own workload identity reads it
// with no ACL policy at all. One name for all publishes is what these tests pin:
// a run holds it until it ends, a run that lost the race is refused instead of
// purging the other one, the allocation a run reads is its own, and its cleanup
// removes only its own job.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// publishApp returns an app with one draft of project-2 and a store configured.
func publishApp(t *testing.T) *testApp {
	t.Helper()
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{
		"url":   "https://store.example.com",
		"token": "a-store-token",
	}}
	return ta
}

// publishJobUnder returns the job another run left under the publish name, in the
// state Nomad reports it.
func publishJobUnder(ta *testApp, status, version, arch string) {
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	ta.cluster.jobs[boxPublishJobID] = &api.Job{
		ID: strPtr(boxPublishJobID), Name: strPtr(BoxPublishJobName(version)),
		Type: strPtr("batch"), Status: strPtr(status),
		Meta: map[string]string{boxWorkMarker: "publish", "image": version, "arch": arch},
	}
}

// publishAllocation is one allocation of the publish job, ended with the event
// that says where the image went.
func publishAllocation(id string, job *api.Job, index uint64, eventIndex string) *api.Allocation {
	event := &api.TaskEvent{Type: "Driver", Time: 1,
		Details: map[string]string{"kind": "publish", "index": eventIndex}}
	state := &api.TaskState{State: "dead", StartedAt: time.Now(), FinishedAt: time.Now(),
		Events: []*api.TaskEvent{event}}
	return &api.Allocation{ID: id, JobID: boxPublishJobID, Job: job, NodeID: "node-1", NodeName: "node-1",
		ClientStatus: "complete", DesiredStatus: "run", CreateIndex: index,
		TaskStates: map[string]*api.TaskState{boxTaskName: state}}
}

// callIndex is the position of the first recorded Nomad call with a prefix, or -1.
func callIndex(ta *testApp, prefix string) int {
	ta.cluster.callsMu.Lock()
	defer ta.cluster.callsMu.Unlock()
	for i, call := range ta.cluster.calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

// TestPublishIsRefusedWhileAnotherOneRuns: the name box-store is the store's, so a
// publish that is running or starting holds it. The second run says which image
// holds it and leaves it alone: the first one is uploading.
func TestPublishIsRefusedWhileAnotherOneRuns(t *testing.T) {
	for _, status := range []string{"running", "pending"} {
		t.Run(status, func(t *testing.T) {
			ta := publishApp(t)
			publishJobUnder(ta, status, "project-9", "arm64")

			rc := ta.run(t, "images", "publish", "project-2", "--yes")
			require.Equal(t, ExitError, rc)
			require.Contains(t, ta.errOut.String(),
				"another publish is running: project-9 (arm64), job box-store")
			require.Contains(t, ta.errOut.String(), `"nomad job stop -purge box-store"`)
			require.Zero(t, callCount(ta, "StopJob box-store"), "the running publish is not purged")
			require.Zero(t, callCount(ta, "RegisterNewJob"), "nothing is registered while the name is taken")
		})
	}
}

// TestPublishPurgesTheLeftoverOfTheLastRun: a dead job under the name is what a
// publish that ended leaves. It is purged first, or Nomad would answer the
// registration with the old run and its allocation would answer for this one.
func TestPublishPurgesTheLeftoverOfTheLastRun(t *testing.T) {
	ta := publishApp(t)
	ta.finishWorkJobs(t, map[string]string{"kind": "publish", "index": "https://store.example.com/index.json"}, "")
	publishJobUnder(ta, "dead", "project-1", "amd64")

	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
	stop, register := callIndex(ta, "StopJob box-store purge=true"), callIndex(ta, "RegisterNewJob box-store")
	require.True(t, stop >= 0, "the leftover is purged: %v", ta.cluster.calls)
	require.True(t, register > stop, "the leftover is purged before the registration: %v", ta.cluster.calls)
	require.Contains(t, ta.errOut.String(), "published project-2 (amd64)")
}

// TestPublishLosesTheRegistrationRace: another publish registered between this
// run's read of the job and its own registration. The registration is atomic, so
// this run is refused -- and it does not purge the run that won.
func TestPublishLosesTheRegistrationRace(t *testing.T) {
	ta := publishApp(t)
	ta.cluster.onRegisterNewJob = func(c *fakeCluster, job *api.Job) {
		c.jobs[boxPublishJobID] = &api.Job{
			ID: strPtr(boxPublishJobID), Status: strPtr("running"),
			Meta: map[string]string{boxWorkMarker: "publish", "image": "project-7", "arch": "amd64"},
		}
	}

	rc := ta.run(t, "images", "publish", "project-2", "--yes")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "another publish is running: project-7 (amd64), job box-store")
	require.Zero(t, callCount(ta, "StopJob box-store"), "the publish that won the name is not purged")
}

// TestPublishReadsItsOwnAllocation: the wait polls every two seconds, so a
// neighbour can register between two polls and leave a newer allocation beside
// this run's. The newer one is not this run's answer.
func TestPublishReadsItsOwnAllocation(t *testing.T) {
	ta := publishApp(t)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		if derefStr(job.ID, "") != boxPublishJobID {
			return
		}
		mine, theirs := uint64(c.allocSeq+1), uint64(c.allocSeq+2)
		c.allocSeq = int(theirs)
		c.allocs["alloc-mine"] = publishAllocation("alloc-mine", job, mine,
			"https://store.example.com/box-base/index.json")
		c.allocs["alloc-theirs"] = publishAllocation("alloc-theirs", job, theirs,
			"https://store.example.com/box-base/another.json")
	}

	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "index   https://store.example.com/box-base/index.json")
	require.NotContains(t, ta.errOut.String(), "another.json",
		"a newer allocation of the same job is not this run's result")
}

// TestEarliestAllocationPicksTheOldestWhateverItsState: the client status does
// not decide which allocation is a run's own; the create index does. A run's
// allocation can be complete while a neighbour's newer one still runs.
func TestEarliestAllocationPicksTheOldestWhateverItsState(t *testing.T) {
	oldest := &api.AllocationListStub{ID: "alloc-oldest", ClientStatus: "complete", CreateIndex: 1}
	newer := &api.AllocationListStub{ID: "alloc-newer", ClientStatus: "running", CreateIndex: 2}
	newest := &api.AllocationListStub{ID: "alloc-newest", ClientStatus: "failed", CreateIndex: 3}

	require.Equal(t, oldest, earliestAllocation([]*api.AllocationListStub{newer, oldest, newest}))
	require.Equal(t, oldest, earliestAllocation([]*api.AllocationListStub{newest, newer, oldest}))
	require.Nil(t, earliestAllocation(nil))
	require.Nil(t, earliestAllocation([]*api.AllocationListStub{nil, nil}))
}

// TestPublishReadsItsOwnAllocationWhenItIsComplete: this run's allocation ended
// complete, while a neighbour that registered after it still runs -- or failed.
// The oldest allocation is this run's whatever state it reached: the neighbour's
// is not this run's to wait for, to report or to fail on.
func TestPublishReadsItsOwnAllocationWhenItIsComplete(t *testing.T) {
	for _, status := range []string{"running", "failed"} {
		t.Run(status, func(t *testing.T) {
			ta := publishApp(t)
			ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
				if derefStr(job.ID, "") != boxPublishJobID {
					return
				}
				mine, theirs := uint64(c.allocSeq+1), uint64(c.allocSeq+2)
				c.allocSeq = int(theirs)
				c.allocs["alloc-mine"] = publishAllocation("alloc-mine", job, mine,
					"https://store.example.com/box-base/index.json")
				other := publishAllocation("alloc-theirs", job, theirs,
					"https://store.example.com/box-base/another.json")
				other.ClientStatus = status
				if status == "failed" {
					other.TaskStates[boxTaskName].Failed = true
					other.TaskStates[boxTaskName].Events = []*api.TaskEvent{{
						Type: "Driver", Time: 1, Details: map[string]string{"kind": "exit"},
						DisplayMessage: "the neighbour failed",
					}}
				}
				c.allocs["alloc-theirs"] = other
			}

			require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "published project-2 (amd64)")
			require.Contains(t, ta.errOut.String(), "index   https://store.example.com/box-base/index.json")
			require.NotContains(t, ta.errOut.String(), "another.json")
			require.NotContains(t, ta.errOut.String(), "the neighbour failed")
		})
	}
}

// TestPublishReportsOnlyItsOwnAllocation: this run's allocation ended complete
// without a publish event, and a neighbour's newer allocation carries one. The
// neighbour's event is not this run's result: the command fails saying its own
// job never said where the image went.
func TestPublishReportsOnlyItsOwnAllocation(t *testing.T) {
	ta := publishApp(t)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		if derefStr(job.ID, "") != boxPublishJobID {
			return
		}
		mine, theirs := uint64(c.allocSeq+1), uint64(c.allocSeq+2)
		c.allocSeq = int(theirs)
		c.allocs["alloc-mine"] = publishAllocation("alloc-mine", job, mine, "")
		c.allocs["alloc-theirs"] = publishAllocation("alloc-theirs", job, theirs,
			"https://store.example.com/box-base/another.json")
	}

	require.Equal(t, ExitError, ta.run(t, "images", "publish", "project-2", "--yes"))
	require.Contains(t, ta.errOut.String(), "ended without saying where the image went")
	require.NotContains(t, ta.errOut.String(), "published project-2")
	require.NotContains(t, ta.errOut.String(), "another.json")
}

// TestPublishPurgesOnlyItsOwnJob: the job under the name is this run's only while
// its modify index is the one this run's registration returned. Another run's job
// is not this run's to purge.
func TestPublishPurgesOnlyItsOwnJob(t *testing.T) {
	ta := publishApp(t)
	publishJobUnder(ta, "running", "project-9", "arm64")

	// The job is another run's: a different modify index than this run's.
	ta.purgePublishJob(context.Background(), 7)
	require.Zero(t, callCount(ta, "StopJob"), "another run's publish job is not purged")

	ta.cluster.mu.Lock()
	ta.cluster.jobs[boxPublishJobID].JobModifyIndex = uint64Ptr(7)
	ta.cluster.mu.Unlock()
	ta.purgePublishJob(context.Background(), 7)
	require.Equal(t, 1, callCount(ta, "StopJob box-store purge=true"), "this run's own job is purged")
}

// TestPublishThatTimedOutHoldsTheName: a publish that runs past --timeout goes on
// uploading. A second run is refused rather than purging it, so the upload is
// never cut off by an operator who tried again.
func TestPublishThatTimedOutHoldsTheName(t *testing.T) {
	ta := publishApp(t)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {} // the task never ends

	rc := ta.run(t, "images", "publish", "project-2", "--yes", "--timeout", "1m")
	require.Equal(t, ExitTimeout, rc)
	require.Contains(t, ta.errOut.String(), "did not finish within")

	require.Equal(t, ExitError, ta.run(t, "images", "publish", "project-2", "--yes"))
	require.Contains(t, ta.errOut.String(), "another publish is running: project-2 (amd64), job box-store")
	require.Equal(t, 1, callCount(ta, "RegisterNewJob box-store"), "the upload was not registered twice")
}
