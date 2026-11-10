package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// addBox puts a running box straight into the fake cluster.
func (ta *testApp) addBox(t *testing.T, id string) {
	t.Helper()
	spec := goodSpec()
	spec.ID = id
	job := mustJob(t, spec)
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	job.Status = strPtr("running")
	ta.cluster.jobs[derefStr(job.ID, "")] = job
	ta.cluster.runBox(job)
	volume, err := RenderVolume(VolumeSpec{BoxID: id, NodeID: "node-1", DiskGiB: spec.DiskGiB})
	require.NoError(t, err)
	ta.cluster.volumeSeq++
	volume.ID = fmt.Sprintf("vol-%d", ta.cluster.volumeSeq)
	volume.CapacityBytes = volume.RequestedCapacityMaxBytes
	volume.State = api.HostVolumeStateReady
	ta.cluster.volumes[volume.ID] = volume
	// The driver reports the disk of every box on its node.
	ta.cluster.addNodeDiskLocked("node-1", id, 300<<20, int64(spec.DiskGiB)<<30)
}

func TestCreate(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "2", "--memory-mib", "1024", "--disk-gib", "8")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "created box dev1")
	require.Contains(t, ta.errOut.String(), "box session new dev1")

	// The disk is a dynamic host volume of exactly the asked size.
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Len(t, ta.cluster.volumes, 1)
	for _, volume := range ta.cluster.volumes {
		require.Equal(t, "box-dev1", volume.Name)
		require.Equal(t, "box-volume", volume.PluginID)
		require.Equal(t, "node-1", volume.NodeID)
		require.Equal(t, int64(8)<<30, volume.RequestedCapacityMaxBytes)
	}
	job, ok := ta.cluster.jobs["box-dev1"]
	require.True(t, ok)
	require.Equal(t, "true", job.Meta["box"])
	require.Equal(t, "amd64", job.Meta["arch"])
	require.Equal(t, 1024, job.TaskGroups[0].Tasks[0].Config["memory_mib"])
	require.Equal(t, 1024+BoxMemoryOverheadMiB, *job.TaskGroups[0].Tasks[0].Resources.MemoryMB)
}

func TestCreateJSON(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "4", "--json"))
	var doc createOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "dev1", doc.Box.ID)
	require.Equal(t, "running", doc.Box.State)
	require.Equal(t, "box-dev1", doc.Job)
	require.Equal(t, "1.0.0", doc.Image.Version)
	require.Equal(t, "node-1", doc.Node)
}

func TestCreateExistingBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), `a box "dev1" already exists`)
	require.Contains(t, ta.errOut.String(), "box rm dev1 --yes")
}

func TestCreateUsageErrors(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitUsage, ta.run(t, "create"))
	require.Equal(t, ExitUsage, ta.run(t, "create", "dev1")) // no --image
	require.Equal(t, ExitUsage, ta.run(t, "create", "Dev1", "--image", "1.0.0"))
	require.Equal(t, ExitUsage, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "0"))
	require.Equal(t, ExitUsage, ta.run(t, "create", "dev1", "--image", "1.0.0", "--arch", "sparc"))
}

func TestCreateUnknownImage(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	rc := ta.run(t, "create", "dev1", "--image", "9.9.9")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "published versions: 1.0.0")
}

func TestCreateWithoutPublishedIndex(t *testing.T) {
	ta := newTestApp(t, "")
	ta.images.status = 404
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "answers 404")
	require.Contains(t, ta.errOut.String(), "BOX_IMAGE_INDEX")
}

func TestNoImageIndexConfigured(t *testing.T) {
	// There is no built-in index: without BOX_IMAGE_INDEX or --image-index the
	// command says what to set instead of guessing a URL.
	ta := newTestApp(t, indexJSON)
	ta.indexURL = ""
	require.Equal(t, "", ta.ImageIndexURL())

	// `box images` with no index still lists what the nodes baked: a draft is an
	// image, and a cluster that bakes before it publishes has only those.
	rc := ta.run(t, "images")
	require.Equal(t, ExitOK, rc)
	require.Contains(t, ta.errOut.String(), "the image index is not configured")
	require.Contains(t, ta.errOut.String(), "BOX_IMAGE_INDEX")

	ta.errOut.Reset()
	rc = ta.run(t, "create", "dev1", "--image", "1.0.0")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "the image index is not configured")
}

func TestImageIndexFlag(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	// The flag wins over the configured URL, and it works before or after the
	// command name.
	require.Equal(t, ExitOK, ta.run(t, "--image-index", "http://other.test/index.json", "images"))
	require.Equal(t, "http://other.test/index.json", ta.ImageIndexURL())
	require.Contains(t, ta.errOut.String(), "http://other.test/index.json")

	require.Equal(t, ExitOK, ta.run(t, "images", "--image-index=http://third.test/index.json"))
	require.Equal(t, "http://third.test/index.json", ta.ImageIndexURL())

	rc := ta.run(t, "images", "--image-index")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "--image-index needs a URL")
}

func TestCreateNoSuitableNode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	// The only node loses the box-volume plugin.
	ta.cluster.nodes["node-1"].Attributes = map[string]string{
		"driver.box.version": "0.2.0",
		"driver.box.arch":    "x86_64",
		"driver.box.os":      "linux",
	}
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "box-volume host volume plugin")
}

func TestCreateArchMismatch(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--arch", "arm64", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "arm64")
}

func TestCreateVolumeFailureRollsBack(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.createVolumeErr = errors.New("no space left on node")
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "no space left on node")
	require.Empty(t, ta.cluster.jobs)
}

func TestCreateRegisterFailureDeletesVolume(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.registerErr = errors.New("job validation failed")
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "job validation failed")
	require.Empty(t, ta.cluster.volumes, "a disk no box uses is deleted again")
}

func TestCreateRegisterDeniedNamesTheMissingRules(t *testing.T) {
	// Nomad answers a refused registration with a bare "Permission denied";
	// the command says which rules a box job needs.
	ta := newTestApp(t, indexJSON)
	ta.cluster.registerErr = deniedErr{}
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "Permission denied")
	require.Contains(t, ta.errOut.String(), `host_volume "box-dev1" with mount-readwrite`)
	require.Contains(t, ta.errOut.String(), "submit-job")
	require.Empty(t, ta.cluster.volumes)

	// Any other refusal is reported as it is.
	ta = newTestApp(t, indexJSON)
	ta.cluster.registerErr = errors.New("job validation failed")
	ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.NotContains(t, ta.errOut.String(), "host_volume")
}

// deniedErr is how the Nomad API client reports a 403.
type deniedErr struct{}

func (deniedErr) Error() string   { return "Unexpected response code: 403 (Permission denied)" }
func (deniedErr) StatusCode() int { return 403 }

func TestCreateRegisterErrorKeepsTheVolumeWhenTheJobExists(t *testing.T) {
	// The server took the job and the answer was lost on the way back: the
	// disk must stay, because the box is running on it.
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.cluster.registerLostErr = errors.New("connection reset by peer")

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "registered, state running")
	require.Contains(t, ta.errOut.String(), "were kept")
	require.Len(t, ta.cluster.volumes, 1, "the disk of a registered box is never deleted")
	require.Contains(t, ta.cluster.jobs, "box-dev1")
	for _, call := range ta.cluster.calls {
		require.NotContains(t, call, "DeleteHostVolume", "nothing is deleted when the job exists")
	}
}

func TestCreateRegisterErrorKeepsEverythingWhenNomadCannotBeAsked(t *testing.T) {
	// No answer from Nomad is not "no job": nothing may be deleted.
	ta := newTestApp(t, indexJSON)
	ta.cluster.registerErr = errors.New("connection reset by peer")
	// After the registration attempt, Nomad stops answering.
	ta.cluster.registerErrAfter = true

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "did not say whether the job box-dev1 was registered")
	require.Contains(t, ta.errOut.String(), "nothing was deleted")
	require.Len(t, ta.cluster.volumes, 1, "the disk is kept when the job state is unknown")
	for _, call := range ta.cluster.calls {
		require.NotContains(t, call, "DeleteHostVolume")
		require.NotContains(t, call, "StopJob", "a job whose state is unknown is not stopped either")
	}
}

func TestCreateRegisterErrorKeepsTheVolumeWhenTheNameIsNotABox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.registerErr = errors.New("job validation failed")
	ta.cluster.mu.Lock()
	ta.cluster.jobs["box-dev1"] = &api.Job{ID: strPtr("box-dev1"), Meta: map[string]string{"owner": "someone else"}}
	ta.cluster.mu.Unlock()

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "is not a box")
	require.Len(t, ta.cluster.volumes, 1)
}

func TestCreateWaitFailureKeepsTheDiskWhenTheJobCannotBeStopped(t *testing.T) {
	// A box that never ran is removed again -- but only if its job really goes.
	// A failed delete leaves the job able to start later, so the disk stays.
	ta := newTestApp(t, indexJSON)
	ta.cluster.stopErr = errors.New("server refused the delete")

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "8", "--memory-mib", "8192", "--disk-gib", "1", "--timeout", "30s")
	require.Equal(t, ExitTimeout, rc, "the timeout is still a timeout")
	require.Contains(t, ta.errOut.String(), "could not delete the job box-dev1")
	require.Contains(t, ta.errOut.String(), "disk box-dev1 was kept")
	require.Len(t, ta.cluster.volumes, 1)
}

func TestCreatePlacementFailureIsPlainWords(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		// The scheduler could not place the group: nothing runs.
		eval := c.evals[fmt.Sprintf("eval-%d", c.registerNo)]
		eval.FailedTGAllocs = map[string]*api.AllocationMetric{
			"box": {NodesEvaluated: 3, DimensionExhausted: map[string]int{"memory": 3}},
		}
	}
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "8", "--memory-mib", "8192", "--disk-gib", "4")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "could not place the box")
	require.Contains(t, ta.errOut.String(), "ran out of memory")
	// Nothing is left behind.
	require.Empty(t, ta.cluster.jobs)
	require.Empty(t, ta.cluster.volumes)
}

func TestCreateWaitTimeout(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		c.allocSeq++
		alloc := &api.Allocation{
			ID: "alloc-pending", JobID: derefStr(job.ID, ""), NodeID: "node-1", NodeName: "node-1",
			ClientStatus: "pending", CreateIndex: uint64(c.allocSeq),
			TaskStates: map[string]*api.TaskState{boxTaskName: {State: "pending"}},
		}
		c.allocs[alloc.ID] = alloc
	}
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1", "--timeout", "30s")
	require.Equal(t, ExitTimeout, rc)
	require.Contains(t, ta.errOut.String(), "did not start within")
}

func TestCreateNoWait(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		c.allocSeq++
		c.allocs["alloc-pending"] = &api.Allocation{
			ID: "alloc-pending", JobID: derefStr(job.ID, ""), NodeID: "node-1", NodeName: "node-1",
			ClientStatus: "pending", CreateIndex: uint64(c.allocSeq),
			TaskStates: map[string]*api.TaskState{boxTaskName: {State: "pending"}},
		}
	}
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "1", "--wait=false")
	require.Equal(t, ExitOK, rc)
	require.Contains(t, ta.errOut.String(), "starting")
	require.Contains(t, ta.cluster.jobs, "box-dev1")
}

func TestStopKeepsTheDisk(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	// The disk is kept for now, and the command says for how long: a stop is a
	// pause, and the pass deletes the disk 24 hours later.
	require.Contains(t, ta.errOut.String(), "kept until the box has been stopped for 24 hours")
	require.Contains(t, ta.errOut.String(), "box reap")
	require.Len(t, ta.cluster.volumes, 1)
	require.True(t, *ta.cluster.jobs["box-dev1"].Stop)

	// Stopping twice is not an error.
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	require.Contains(t, ta.errOut.String(), "already stopped")
}

// hogAllocation takes most of a node's memory, so the next create does not fit
// until a test removes it.
func (ta *testApp) hogAllocation(memoryMB int64) {
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	ta.cluster.allocs["hog"] = &api.Allocation{
		ID: "hog", NodeID: "node-1", ClientStatus: "running",
		AllocatedResources: &api.AllocatedResources{Tasks: map[string]*api.AllocatedTaskResources{
			"box": {Memory: api.AllocatedMemoryResources{MemoryMB: memoryMB}},
		}},
	}
}

// TestCreateWaitAsksAgainUntilThereIsRoom is `box create --wait 30m`: the box
// does not fit, the command says so once, and it asks again until a box finishes.
func TestCreateWaitAsksAgainUntilThereIsRoom(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.hogAllocation(20000) // node-1 has 32768 MiB, so 12768 are free

	var sleeps int
	ta.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		ta.clock.advance(d)
		if sleeps == 1 {
			ta.cluster.mu.Lock()
			delete(ta.cluster.allocs, "hog")
			ta.cluster.mu.Unlock()
		}
		return nil
	}

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384", "--wait", "30m")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no room for box dev1 yet")
	require.Contains(t, ta.errOut.String(), "16896 MiB", "the line says what the box needs")
	require.Contains(t, ta.errOut.String(), "room for box dev1 after")
	require.Contains(t, ta.errOut.String(), "created box dev1")
	require.Equal(t, 2, sleeps, "the command asked twice: once refusal, once room")
}

// TestCreateWaitGivesUpWithTheRefusal: the waiting ends with the refusal the
// placement gave and its exit code, and the reason is repeated as it goes.
func TestCreateWaitGivesUpWithTheRefusal(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.mu.Lock()
	ta.cluster.nodes["node-1"].NodeResources.Memory.MemoryMB = 8000
	ta.cluster.mu.Unlock()

	var sleeps int
	ta.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		ta.clock.advance(d)
		return nil
	}

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384", "--wait", "1m")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "box: no node can take this box")
	require.Contains(t, ta.errOut.String(), "no room for box dev1 yet")
	require.Contains(t, ta.errOut.String(), "the placement is refused as it was")
	require.Equal(t, 2, sleeps, "1m of waiting is two asks 30s apart")
	require.Empty(t, ta.cluster.jobs, "nothing was created")
}

// TestCreateWaitDoesNotWaitForWhatWaitingCannotChange: a wrong architecture, a
// missing image or a usage error is refused at once.
func TestCreateWaitDoesNotWaitForWhatWaitingCannotChange(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.Sleep = func(ctx context.Context, d time.Duration) error {
		t.Errorf("nothing should be waited for, slept %s", d)
		return nil
	}
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--arch", "arm64", "--wait", "30m")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the box needs arm64")
	require.NotContains(t, ta.errOut.String(), "no room for box dev1 yet")
}

// TestCreateWaitFlagIsOptional: "--wait" alone is what it always was, and
// "--wait=false" waits for nothing.
func TestCreateWaitFlagIsOptional(t *testing.T) {
	// A bare --wait waits for the box to run and does not retry a refusal.
	ta := newTestApp(t, indexJSON)
	ta.Sleep = func(ctx context.Context, d time.Duration) error {
		if d == capacityPollInterval {
			t.Errorf("a bare --wait must not retry a refusal")
		}
		return nil
	}
	ta.cluster.mu.Lock()
	ta.cluster.nodes["node-1"].NodeResources.Memory.MemoryMB = 8000
	ta.cluster.mu.Unlock()
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384", "--wait")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "box: no node can take this box")
	require.NotContains(t, ta.errOut.String(), "no room for box dev1 yet")

	// --wait=false creates the box and does not wait for it to run.
	ta = newTestApp(t, indexJSON)
	rc = ta.run(t, "create", "dev1", "--image", "1.0.0", "--wait=false")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "starting")
	require.Len(t, ta.cluster.jobs, 1, "the job is registered even when the command does not wait")
}

// TestOptionalDurationFlag reads both shapes of a flag whose value may be left
// out, and does not take a box id for a duration.
func TestOptionalDurationFlag(t *testing.T) {
	newWait := func() (*flag.FlagSet, *optionalDuration) {
		fs := newFlagSet("test")
		wait := &optionalDuration{on: true}
		fs.Var(wait, "wait", "")
		return fs, wait
	}

	fs, wait := newWait()
	require.NoError(t, parseFlags(fs, joinOptionalValue([]string{"--wait", "30m", "dev1"}, "wait", isDurationValue), 1))
	require.True(t, wait.on)
	require.Equal(t, 30*time.Minute, wait.d)
	require.Equal(t, "dev1", fs.Arg(0))

	fs, wait = newWait()
	require.NoError(t, parseFlags(fs, joinOptionalValue([]string{"dev1", "--wait"}, "wait", isDurationValue), 1))
	require.True(t, wait.on)
	require.Zero(t, wait.d, "a bare --wait carries no duration")

	fs, _ = newWait()
	require.NoError(t, parseFlags(fs, joinOptionalValue([]string{"--wait", "dev1"}, "wait", isDurationValue), 1))
	require.Equal(t, "dev1", fs.Arg(0), "a box id is not a duration, so it is not the flag's value")

	fs, wait = newWait()
	require.NoError(t, parseFlags(fs, joinOptionalValue([]string{"--wait=false", "dev1"}, "wait", isDurationValue), 1))
	require.False(t, wait.on)
	require.True(t, wait.given())

	// A value that is not a duration is a usage error, not a silent box id.
	fs, _ = newWait()
	require.Equal(t, ExitUsage, ExitCode(parseFlags(fs, joinOptionalValue([]string{"--wait", "3x", "dev1"}, "wait", isDurationValue), 1)))
}

func TestStartUsesTheSameDisk(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "old disk")
	require.Len(t, ta.cluster.volumes, 1, "the volume is not recreated")
}

// TestStartTakesTheNodesCurrentReservation: an existing box keeps the reservation
// it was created with until a resize, an upgrade -- or a start, which
// re-registers the job and reads the node's figure again.
func TestStartTakesTheNodesCurrentReservation(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.mu.Lock()
	node := ta.cluster.nodes["node-1"]
	node.Attributes["cpu.numcores"] = "8"
	ta.cluster.mu.Unlock()
	ta.addBox(t, "dev1")
	require.Equal(t, 2*1000, *ta.cluster.jobs["box-dev1"].TaskGroups[0].Tasks[0].Resources.CPU,
		"the box was made with the default reservation of 1000 MHz a vCPU")

	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	ta.cluster.mu.Lock()
	node.Attributes["driver.box.cpu_overcommit"] = "2"
	ta.cluster.mu.Unlock()

	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	require.Equal(t, 2*750, *ta.cluster.jobs["box-dev1"].TaskGroups[0].Tasks[0].Resources.CPU,
		"the start took the node's overcommit: 750 MHz a vCPU")
}

func TestStartWithoutDisk(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	ta.cluster.volumes = map[string]*api.HostVolume{}
	rc := ta.run(t, "start", "dev1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "disk of box dev1 is gone")
}

func TestRemove(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "deleted box dev1 and its disk")
	require.Empty(t, ta.cluster.jobs)
	require.Empty(t, ta.cluster.volumes)
}

func TestRemoveWaitsForTheVolumeRegistration(t *testing.T) {
	// Nomad answers the delete before its volume list has dropped the name, and
	// `box create` refuses a name that is still listed: rm waits for it.
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.volumeDeleteLag = 3

	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())
	require.Empty(t, ta.cluster.jobs)
	require.Empty(t, ta.cluster.volumes, "the volume is deleted")
	require.Empty(t, ta.cluster.pendingVolumes, "and Nomad no longer lists it")
	require.Contains(t, ta.errOut.String(), "deleted box dev1 and its disk")
}

func TestRemoveReportsAVolumeNomadStillLists(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.volumeDeleteLag = 10000 // it never disappears within the wait

	rc := ta.run(t, "rm", "dev1", "--yes", "--timeout", "20s")
	require.Equal(t, ExitTimeout, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "box-dev1 is still registered")
	require.Contains(t, ta.errOut.String(), "state ready")
	require.Contains(t, ta.errOut.String(), "cannot be created until Nomad drops the registration")
}

func TestRemoveDoesNotWaitWithoutAVolume(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.jobs["box-dev1"] = &api.Job{ID: strPtr("box-dev1"), Meta: map[string]string{"box": "true", "box_id": "dev1"},
		Status: strPtr("dead"), Stop: boolPtr(true),
		TaskGroups: []*api.TaskGroup{{Name: strPtr("box"), Tasks: []*api.Task{{Name: "box", Driver: "box"}}}}}

	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no volume left to delete")
}

func TestRemoveRefusesWithoutYesOnAPipe(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "rm", "dev1")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "pass --yes")
	require.Len(t, ta.cluster.volumes, 1)
}

func TestRemoveAsksOnATerminal(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.term.interactive = true
	ta.in.WriteString("y\n")
	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1"), ta.errOut.String())
	require.Empty(t, ta.cluster.volumes)

	ta.addBox(t, "dev2")
	ta.in.Reset()
	ta.in.WriteString("n\n")
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "rm", "dev2"))
	require.Contains(t, ta.errOut.String(), "nothing was deleted")
	require.Len(t, ta.cluster.volumes, 1)
}

func TestResizeCPUsAndMemory(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--cpus", "4", "--memory-mib", "2048"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "cpus 2 -> 4")
	job := ta.cluster.jobs["box-dev1"]
	task := job.TaskGroups[0].Tasks[0]
	require.Equal(t, 4, task.Config["cpus"])
	require.Equal(t, 2048, task.Config["memory_mib"])
	require.Equal(t, 4000, *task.Resources.CPU)
	require.Equal(t, 2048+BoxMemoryOverheadMiB, *task.Resources.MemoryMB)
}

// The disk grows in the driver, from the job's disk_gib: Nomad's dynamic host
// volume keeps the capacity its plugin reported when the volume was created, so
// the resize re-registers the job and leaves the volume record alone.
func TestResizeDiskGrowsTheJobAndLeavesTheVolumeAlone(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--disk-gib", "16"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "disk 8 -> 16 GiB")
	require.Equal(t, 16, ta.cluster.jobs["box-dev1"].TaskGroups[0].Tasks[0].Config["disk_gib"])
	for _, volume := range ta.cluster.volumes {
		require.Equal(t, int64(8)<<30, volume.CapacityBytes,
			"the volume record keeps the size its plugin reported: a register cannot change it")
		require.Equal(t, int64(8)<<30, volume.RequestedCapacityMaxBytes)
	}
	// No volume write at all, and no stop of the job first: the box restarts
	// because its job changed, and the driver grows the disk then.
	require.Zero(t, callCount(ta, "RegisterHostVolume"), "calls: %v", ta.cluster.calls)
	require.Zero(t, callCount(ta, "StopJob"), "calls: %v", ta.cluster.calls)
}

// The record and the disk disagree after a disk resize, and `box status` says
// which is which instead of printing the stale number as the size of the disk.
func TestStatusExplainsTheVolumeRecordAndTheDiskSize(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	// The driver reports a 16 GiB disk; the volume was created at 8 GiB.
	ta.cluster.mu.Lock()
	ta.cluster.addNodeDiskLocked("node-1", "dev1", 300<<20, int64(16)<<30)
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "disk    16.0 GiB")
	require.Contains(t, out, "Nomad records 8.0 GiB for the volume")
	require.Contains(t, out, "no update changes that")
}

func TestResizeDiskCannotShrink(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "resize", "dev1", "--disk-gib", "4")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "can only grow")
}

func TestResizeNeedsAChange(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitUsage, ta.run(t, "resize", "dev1"))
	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--cpus", "2"))
	require.Contains(t, ta.errOut.String(), "nothing to change")
}

func TestRunReturnsTheGuestExitCode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "run", "dev1", "--", "true"))
	last := ta.guest.calls[len(ta.guest.calls)-1]
	require.Equal(t, []string{"setsid", "-w", "sh", "-c", runWrapper}, last[:5], "the command runs in a session of its own")
	require.Equal(t, "true", last[len(last)-1])

	ta.guest.runExit = 42
	require.Equal(t, 42, ta.run(t, "run", "dev1", "--", "false"))
}

func TestRunNeedsACommand(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitUsage, ta.run(t, "run", "dev1"))
	require.Equal(t, ExitUsage, ta.run(t, "run", "dev1", "--"))
}

func TestSSHNeedsATerminal(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "ssh", "dev1")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "needs a terminal")
}

func TestSSHUsesARawTerminal(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.term.interactive = true
	require.Equal(t, ExitOK, ta.run(t, "ssh", "dev1"), ta.errOut.String())
	require.True(t, ta.term.raw)
	require.True(t, ta.term.restored)
	require.True(t, ta.guest.tty)
	require.Equal(t, []string{"bash", "-l"}, ta.guest.calls[len(ta.guest.calls)-1])
}

func TestCopyIntoTheBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	local := t.TempDir()
	tree := filepath.Join(local, "work")
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "a.txt"), []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("beta"), 0o644))

	require.Equal(t, ExitOK, ta.run(t, "cp", tree, "dev1:/work"), ta.errOut.String())
	require.Equal(t, "alpha", ta.guest.read(t, "/work/a.txt"))
	require.Equal(t, "beta", ta.guest.read(t, "/work/sub/b.txt"))
	require.Contains(t, ta.errOut.String(), "copied")

	// A single file to an explicit path.
	file := filepath.Join(local, "one.txt")
	require.NoError(t, os.WriteFile(file, []byte("one"), 0o644))
	require.Equal(t, ExitOK, ta.run(t, "cp", file, "dev1:/work/one.txt"), ta.errOut.String())
	require.Equal(t, "one", ta.guest.read(t, "/work/one.txt"))
}

func TestCopyOutOfTheBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.write(t, "/work/out/a.txt", "alpha")
	ta.guest.write(t, "/work/out/sub/b.txt", "beta")
	local := t.TempDir()

	require.Equal(t, ExitOK, ta.run(t, "cp", "dev1:/work/out", local), ta.errOut.String())
	data, err := os.ReadFile(filepath.Join(local, "out", "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha", string(data))

	// The same tree renamed to one file is a file copy.
	target := filepath.Join(local, "single.txt")
	ta.guest.write(t, "/work/only.txt", "only")
	require.Equal(t, ExitOK, ta.run(t, "cp", "dev1:/work/only.txt", target), ta.errOut.String())
	data, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "only", string(data))
}

func TestCopyMissingSource(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "cp", filepath.Join(t.TempDir(), "absent"), "dev1:/work")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "no local file")

	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "cp", "dev1:/work/absent.txt", filepath.Join(t.TempDir(), "x"))
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "No such file")
}

func TestCopyNeedsOneBoxSide(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitUsage, ta.run(t, "cp", "a", "b"))
	require.Equal(t, ExitUsage, ta.run(t, "cp", "dev1:/a", "dev2:/b"))
}

func TestLogs(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.logData["stdout"] = "{\"kind\":\"turn_start\"}\n"
	ta.cluster.logData["stderr"] = "box driver: guest log command exited\n"
	require.Equal(t, ExitOK, ta.run(t, "logs", "dev1"))
	require.Contains(t, ta.out.String(), "turn_start")
	require.Contains(t, ta.out.String(), "guest log command")
}

func TestStatusOfUnknownBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	rc := ta.run(t, "status", "nosuch")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "no boxes yet")
}

func TestGuestCommandsNeedARunningBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	ta.errOut.Reset()
	rc := ta.run(t, "run", "dev1", "--", "true")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "the box dev1 is stopped")
	require.Contains(t, ta.errOut.String(), "`box start dev1` runs it again")
}

func TestSessionCommands(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["session new --executor prime --model p/m --title fix the build"] = `{"session":"s-1"}`
	ta.guest.responses["session get s-1"] = `{"id":"s-1","executor":"prime","model":"p/m","title":"fix the build","state":"idle","last_turn":0}`
	ta.guest.responses["session ls"] = `{"sessions":[{"id":"s-1","executor":"prime","model":"p/m","state":"idle","last_turn":1}]}`
	ta.guest.responses["send s-1"] = `{"session":"s-1","turn":1}`
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"all done","stop_reason":"end_turn"}`
	ta.guest.responses["events s-1 --limit 200"] = `{"events":[{"seq":1,"ts":"2026-11-02T10:00:00Z","kind":"turn_start","summary":"started"}],"next":2}`
	ta.guest.responses["cancel s-1"] = `{"session":"s-1","turn":1,"state":"cancelled"}`
	ta.guest.responses["session close s-1"] = `{"session":"s-1","state":"closed"}`

	require.Equal(t, ExitOK, ta.run(t, "session", "new", "dev1", "--executor", "prime", "--model", "p/m", "--title", "fix the build"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "started session s-1")

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "session", "ls", "dev1"))
	require.Contains(t, ta.out.String(), "s-1")

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "send", "dev1", "s-1", "fix", "the", "build"), ta.errOut.String())
	require.Equal(t, "fix the build", ta.guest.stdin[len(ta.guest.stdin)-1])

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "all done")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1"))
	require.Contains(t, ta.out.String(), "turn_start")

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "cancel", "dev1", "s-1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "cancelled")

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "session", "close", "dev1", "s-1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "closed session s-1")
}

func TestSessionNewNeedsExecutorAndModel(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitUsage, ta.run(t, "session", "new", "dev1"))
	require.Equal(t, ExitUsage, ta.run(t, "session", "new", "dev1", "--executor", "prime"))
}

func TestSendWhileBusy(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.errors["send s-1"] = boxctlAnswer{json: `{"error":{"code":"busy","message":"turn 1 is running"}}`, code: 1}
	rc := ta.run(t, "send", "dev1", "s-1", "hello")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "still working on a turn")
}

func TestWaitTimesOutWhileRunning(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 9"] = `{"session":"s-1","turn":2,"state":"running"}`
	rc := ta.run(t, "wait", "dev1", "s-1", "--timeout", "10s")
	require.Equal(t, ExitTimeout, rc)
	require.Contains(t, ta.errOut.String(), "still running")
}

// TestWaitFailsOnAFailedTurn: a turn that ended in an error is exit 1, so a
// script cannot take it for a good one.
func TestWaitFailsOnAFailedTurn(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 9"] = `{"session":"s-1","turn":1,"state":"error","error":"Model gw/nope not found"}`
	rc := ta.run(t, "wait", "dev1", "s-1", "--timeout", "10s")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "ended error")
	require.Contains(t, ta.errOut.String(), "Model gw/nope not found")

	ta.guest.responses["wait s-1 --timeout 9"] = `{"session":"s-1","turn":2,"state":"done","text":"ok"}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "10s"))
}

func TestSendReadsAFile(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["send s-1"] = `{"session":"s-1","turn":7}`
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	require.NoError(t, os.WriteFile(prompt, []byte("long prompt from a file"), 0o644))
	require.Equal(t, ExitOK, ta.run(t, "send", "dev1", "s-1", "--file", prompt), ta.errOut.String())
	require.Equal(t, "long prompt from a file", ta.guest.stdin[len(ta.guest.stdin)-1])
}

func TestSendReadsStdin(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["send s-1"] = `{"session":"s-1","turn":8}`
	ta.in.WriteString("a prompt on stdin")
	require.Equal(t, ExitOK, ta.run(t, "send", "dev1", "s-1"), ta.errOut.String())
	require.Contains(t, ta.guest.stdin[len(ta.guest.stdin)-1], "a prompt on stdin")
}

func TestUnknownCommand(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitUsage, ta.run(t, "frobnicate"))
	require.Contains(t, ta.errOut.String(), "unknown command")
}

func TestHelp(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "help"))
	require.Contains(t, ta.errOut.String(), "exit codes")
	for _, command := range []string{"capacity", "create", "ls", "status", "cp", "logs", "session", "wait"} {
		require.Contains(t, ta.errOut.String(), command)
	}
}

func TestExitCodeMapping(t *testing.T) {
	require.Equal(t, ExitOK, ExitCode(nil))
	require.Equal(t, ExitError, ExitCode(errors.New("boom")))
	require.Equal(t, ExitUsage, ExitCode(usagef("u")))
	require.Equal(t, ExitNotFound, ExitCode(notFoundf("n")))
	require.Equal(t, ExitTimeout, ExitCode(timeoutf("t")))
	require.Equal(t, ExitLeftRunning, ExitCode(leftRunningf("r")))
	require.Equal(t, ExitError, ExitCode(wrapf(errors.New("cause"), "context")))
	require.Contains(t, wrapf(errors.New("cause"), "context").Error(), "context: cause")
	require.Equal(t, "short words", oneLine("  short\n words ", 40))
}

// TestRunKeepsTheFlagsAfterTheSeparator pins the fix that made `box run` drop a
// command's own -h: global flags are only taken before "--".
func TestRunKeepsTheFlagsAfterTheSeparator(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")

	rc := ta.run(t, "run", "dev1", "--", "df", "-h", "/")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Len(t, ta.guest.calls, 1)
	require.Equal(t, []string{"df", "-h", "/"}, ta.guest.calls[0][6:])
}

func TestGlobalFlagsStillWorkBeforeTheCommand(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "--json", "run", "dev1", "--", "uname", "-a")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.True(t, ta.JSON)
	require.Equal(t, []string{"uname", "-a"}, ta.guest.calls[0][6:])
}

// TestCreateNotesMoreThanAQuarterOfANode: a box that takes a large part of a
// shared node is created anyway -- free resources are there to be used -- and the
// note on stderr says what it holds and how to give it back. The exit code does
// not change.
func TestCreateNotesMoreThanAQuarterOfANode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "8", "--memory-mib", "8192", "--disk-gib", "4"), ta.errOut.String())
	msg := ta.errOut.String()
	require.Contains(t, msg, "note: this box takes 8 of 12 vCPU on node node-1")
	require.Contains(t, msg, "the node is shared")
	require.Contains(t, msg, "`box rm dev1 --yes` when the task is done")
	require.Contains(t, msg, "created box dev1", "the box really was created")
	require.Contains(t, ta.cluster.jobs, "box-dev1")
}

// TestCreateSaysNothingAboutTheDefaultBox: the default box fits in a quarter of a
// normal node, and a note about a box that fits is noise.
func TestCreateSaysNothingAboutTheDefaultBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "the node is shared")
}

// TestResizeNotesMoreThanAQuarterOfTheNode: the same note, and the same exit
// code, when a box grows.
func TestResizeNotesMoreThanAQuarterOfTheNode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--cpus", "8"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "note: this box takes 8 of 12 vCPU on node node-1")
	require.Contains(t, ta.errOut.String(), "`box rm dev1 --yes` when the task is done")
}

// TestRunStopsTheGuestCommandWhenItsTimeoutEnds: a silent command that runs
// out of --timeout is stopped in the box too, and the run says so with exit 4.
func TestRunStopsTheGuestCommandWhenItsTimeoutEnds(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.block = true
	rc := ta.run(t, "run", "dev1", "--timeout", "50ms", "--", "git", "clone", "-q", "https://example.com/x")
	require.Equal(t, ExitTimeout, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "ran out of its --timeout 50ms")
	require.Contains(t, ta.errOut.String(), "stopped too")
	stop := ta.guest.calls[len(ta.guest.calls)-1]
	run := ta.guest.calls[len(ta.guest.calls)-2]
	require.Equal(t, "sh", stop[0])
	require.Contains(t, stop[2], "kill -TERM")
	require.Equal(t, run[5], stop[3], "the stop names the pid file of that run")

	require.Equal(t, ExitUsage, ta.run(t, "run", "dev1", "--timeout", "soon", "--", "true"))
}

// TestWaitSaysATurnEndedWithoutAMessage: a done turn with no text says so,
// instead of printing nothing.
func TestWaitSaysATurnEndedWithoutAMessage(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 599"] = `{"session":"s-1","turn":2,"state":"done"}`
	rc := ta.run(t, "wait", "dev1", "s-1")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "without a final message")
	require.Contains(t, ta.errOut.String(), "box events dev1 s-1")
}

// TestTopPrintsWhatTheGuestIsDoing: box top is one line of the guest's load.
func TestTopPrintsWhatTheGuestIsDoing(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = `{"boxd_version":"0.1.15","usage":{"at":"2026-11-04T16:44:00Z","cpu_percent":91.5,"iowait_percent":42,` +
		`"load1":3.2,"load5":2.1,"mem_total_mib":1960,"mem_available_mib":120,"disk_path":"/","disk_total_mib":15360,` +
		`"disk_free_mib":11264,"top":"go 900 MiB, node 300 MiB"}}`
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1"), ta.errOut.String())
	out := ta.errOut.String()
	for _, want := range []string{"16:44:00", "CPU 92% (iowait 42%)", "load 3.2 2.1", "memory 1840 of 1960 MiB used (120 available)",
		"disk / 4.0 of 15.0 GiB used", "go 900 MiB"} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "memory pressure", "a boxd that reports no pressure adds no field")
}

// TestTopShowsTheMemoryStall: the kernel's own pressure numbers are part of what
// the guest is doing, and they are what says a guest is stalling rather than
// working.
func TestTopShowsTheMemoryStall(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = `{"boxd_version":"0.1.15","usage":{"at":"2026-11-04T16:44:00Z","cpu_percent":12,` +
		`"mem_total_mib":4096,"mem_available_mib":90,"memory_pressure":{"some_avg10":72.5,"full_avg10":31.0},` +
		`"disk_path":"/","disk_total_mib":10240,"disk_free_mib":9216,"top":"cc1 900 MiB"}}`
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "memory pressure some 72.5% full 31.0% (avg10)")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1", "--json"), ta.errOut.String())
	var doc topOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.NotNil(t, doc.Usage.MemoryPressure)
	require.Equal(t, 31.0, doc.Usage.MemoryPressure.FullAvg10)
}

// TestWaitWarnsAboutWhatTheTurnLeftRunning: a done turn with processes still
// running is said on stderr, after the turn's own text, with how to look at
// them and how to go on -- and the exit code stays the turn's, because when the
// turn ended is the executor's decision and boxd reports it as given.
func TestWaitWarnsAboutWhatTheTurnLeftRunning(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done",
		"text":"the build failed, fixing it",
		"stop_reason":"end_turn",
		"still_running":{"processes":[
			{"pid":123,"command":"go build ./...","age_seconds":250,"cpu_seconds":12.5,"children":4},
			{"pid":456,"command":"python3 -m http.server","age_seconds":30,"cpu_seconds":0.2,"children":0}
		],"more":2}}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	// The turn's own words are on stdout, as without a report.
	require.Contains(t, ta.out.String(), "the build failed, fixing it")
	// The warning is on stderr, after them.
	err := ta.errOut.String()
	require.Contains(t, err, "the turn ended, but these processes it may have started are still running in the box:")
	require.Contains(t, err, "go build ./... (pid 123, 4m10s, 4 children, 12.5s CPU)")
	require.Contains(t, err, "python3 -m http.server (pid 456, 30s, 0.2s CPU)")
	require.Contains(t, err, "and 2 more")
	require.Contains(t, err, "`box top dev1`")
	require.Contains(t, err, "`box run dev1 -- ps -ef`")
	require.Contains(t, err, "`box send dev1 s-1`")
	require.Contains(t, err, "wait for them")
	require.Less(t, strings.Index(err, "finished: done"), strings.Index(err, "still running in the box"),
		"the warning comes after the turn's own line")
	require.NotContains(t, ta.out.String(), "still running", "the warning is not part of the answer")

	// --json carries the field and prints nothing on stderr: the document is
	// the whole answer, and the exit code is unchanged.
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m", "--json"))
	require.Contains(t, ta.out.String(), `"still_running"`)
	require.Contains(t, ta.out.String(), `"command": "go build ./..."`)
	require.Empty(t, strings.TrimSpace(ta.errOut.String()), "a JSON answer says it in the document")
}

// TestWaitWithoutLeftoversSaysNothingExtra: a turn that left nothing running is
// the turn and nothing else, so the warning is never noise.
func TestWaitWithoutLeftoversSaysNothingExtra(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"all done","stop_reason":"end_turn"}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "still running")
	require.NotContains(t, ta.out.String(), "still_running")

	// An empty report is the same as none: boxd leaves the field out, and a
	// boxd that sends an empty list says nothing either.
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"all done","still_running":{"processes":[]}}`
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "still running")
}
