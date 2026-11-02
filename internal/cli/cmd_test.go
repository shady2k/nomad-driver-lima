package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

	rc := ta.run(t, "images")
	require.Equal(t, ExitError, rc)
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
	require.Contains(t, ta.errOut.String(), "disk was kept")
	require.Len(t, ta.cluster.volumes, 1)
	require.True(t, *ta.cluster.jobs["box-dev1"].Stop)

	// Stopping twice is not an error.
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	require.Contains(t, ta.errOut.String(), "already stopped")
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

func TestResizeDisk(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--disk-gib", "16"), ta.errOut.String())
	for _, volume := range ta.cluster.volumes {
		require.Equal(t, int64(16)<<30, volume.CapacityBytes, "the volume grows with the disk")
	}
	require.Equal(t, 16, ta.cluster.jobs["box-dev1"].TaskGroups[0].Tasks[0].Config["disk_gib"])
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
	require.Equal(t, []string{"true"}, ta.guest.calls[len(ta.guest.calls)-1])

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
	require.Contains(t, ta.errOut.String(), "not running")
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
	require.Equal(t, []string{"df", "-h", "/"}, ta.guest.calls[0])
}

func TestGlobalFlagsStillWorkBeforeTheCommand(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "--json", "run", "dev1", "--", "uname", "-a")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.True(t, ta.JSON)
	require.Equal(t, []string{"uname", "-a"}, ta.guest.calls[0])
}
