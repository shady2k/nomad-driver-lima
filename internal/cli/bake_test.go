package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// A bake turns a box into an image; a publish puts such an image into the store.
// Both are tasks of the driver that run no VM, and the CLI drives them: it seals
// the box from inside, stops it, registers the batch job, waits for it and says
// what the image is.

// goodProjectSpec is a box created from an image that takes a platform bundle:
// only such a box can be baked, because boxd and the executors are in the bundle.
func goodProjectSpec() BoxSpec {
	spec := goodSpec()
	spec.PlatformVersion = "1.10.0"
	spec.PlatformURL = "https://images.test/box-platform/1.10.0/box-platform-1.10.0-linux-amd64.tar.gz"
	spec.PlatformSHA256 = "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0"
	return spec
}

// addBoxSpec registers a box job and its disk, as `box create` does.
func (ta *testApp) addBoxSpec(t *testing.T, spec BoxSpec) {
	t.Helper()
	job := mustJob(t, spec)
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	job.Status = strPtr("running")
	ta.cluster.jobs[derefStr(job.ID, "")] = job
	ta.cluster.runBox(job)
	volume, err := RenderVolume(VolumeSpec{BoxID: spec.ID, NodeID: "node-1", DiskGiB: spec.DiskGiB})
	require.NoError(t, err)
	ta.cluster.volumeSeq++
	volume.ID = "vol-x"
	volume.CapacityBytes = volume.RequestedCapacityMaxBytes
	volume.State = api.HostVolumeStateReady
	ta.cluster.volumes[volume.ID] = volume
	ta.cluster.addNodeDiskLocked("node-1", spec.ID, 300<<20, int64(spec.DiskGiB)<<30)
}

// bakeDetails is what the driver's bake event carries when it made an image.
func bakeDetails() map[string]string {
	return map[string]string{
		"kind": "bake", "version": "project-1", "arch": "amd64", "name": "project", "parent": "1.0.0",
		"sha256": strings.Repeat("ab", 32), "size": "1048576", "virtual_bytes": "8589934592",
	}
}

// finishWorkJobs makes the fake cluster run every batch job the command
// registers -- the driver's task ends with the event that says what it did -- and
// run every box job again, which is what a bake's restart of the box is.
func (ta *testApp) finishWorkJobs(t *testing.T, details map[string]string, failure string) {
	t.Helper()
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		id := derefStr(job.ID, "")
		if _, isBox := BoxIDFromJobName(id); isBox {
			c.runBox(job)
			return
		}
		if !strings.HasPrefix(id, boxBakeJobPrefix) && !strings.HasPrefix(id, boxPublishJobPrefix) {
			return
		}
		ta.workJob = job
		c.allocSeq++
		event := &api.TaskEvent{Type: "Driver", Time: 1, Details: details}
		state := &api.TaskState{State: "dead", StartedAt: time.Now(), FinishedAt: time.Now(),
			Events: []*api.TaskEvent{event}}
		client := "complete"
		if failure != "" {
			event.Details = map[string]string{"kind": "exit"}
			event.DisplayMessage = failure
			state.Failed = true
			client = "failed"
		}
		c.allocs["alloc-work"] = &api.Allocation{
			ID: "alloc-work", JobID: id, Job: job, NodeID: "node-1", NodeName: "node-1",
			ClientStatus: client, DesiredStatus: "run", CreateIndex: uint64(c.allocSeq),
			TaskStates: map[string]*api.TaskState{boxTaskName: state},
		}
	}
}

// sealMayRun makes the fake guest answer the seal's gate, which `box bake` asks
// before it stops anything.
func (ta *testApp) sealMayRun() {
	ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":0,"seal":{"ok":true}}`
}

// stopBox makes a box stopped: its job is stopped and its allocation is done.
func (ta *testApp) stopBox(id string) {
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	job := ta.cluster.jobs[BoxJobName(id)]
	if job != nil {
		job.Stop = boolPtr(true)
		job.Status = strPtr("dead")
	}
	for _, alloc := range ta.cluster.allocs {
		if alloc.JobID == BoxJobName(id) {
			alloc.ClientStatus = "complete"
			if task := alloc.TaskStates[boxTaskName]; task != nil {
				task.State = "dead"
			}
		}
	}
}

// TestBakeStopsSealsAndStartsTheBox drives the whole `box bake` of a box: it is
// stopped, the driver clones and seals it, and it runs again with its work.
func TestBakeStopsSealsAndStartsTheBox(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	spec := goodProjectSpec()
	ta.addBoxSpec(t, spec)
	ta.finishWorkJobs(t, bakeDetails(), "")
	ta.sealMayRun()
	recipe := filepath.Join(t.TempDir(), "recipe.sh")
	require.NoError(t, os.WriteFile(recipe, []byte("#!/bin/bash\napt-get install -y ripgrep\n"), 0o644))

	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--recipe", recipe), ta.errOut.String())

	// The seal's own gate was asked in the box, and the CLI never sealed: the
	// clone does that.
	require.Contains(t, strings.Join(ta.guest.callLines(), "\n"), boxctlPath+" info",
		"the seal's gate was asked in the box, before it was stopped")

	// The box was stopped, and it runs again afterwards.
	require.True(t, callCount(ta, "StopJob box-dev1 purge=false") > 0, "the box is stopped for the copy")
	require.True(t, callCount(ta, "RegisterJob box-dev1") > 0, "the box runs again")
	box, _, err := ta.findBox(context.Background(), "dev1")
	require.NoError(t, err)
	require.Equal(t, "running", box.State)

	// The bake job carries the volume, the image, the platform bundle the clone
	// mounts, and the box's own size: the task boots the clone in itself.
	job := ta.workJob
	require.NotNil(t, job, "the bake job was registered")
	require.Equal(t, BoxBakeJobName("dev1"), derefStr(job.ID, ""))
	require.Equal(t, "batch", derefStr(job.Type, ""))
	require.Equal(t, "bake", job.Meta[boxWorkMarker])
	group := job.TaskGroups[0]
	require.Equal(t, BoxVolumeName("dev1"), group.Volumes[boxDiskVolume].Source)
	// Read-write, although the task only reads the box's disk: Nomad places a
	// read-only request of a dynamic host volume only when the volume offers a
	// read-only access mode, and this volume is single-node-writer.
	require.False(t, group.Volumes[boxDiskVolume].ReadOnly)
	task, err := boxTask(ta.workJob)
	require.NoError(t, err)
	bake, ok := task.Config["bake"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "project", bake["name"])
	require.Equal(t, "project-1", bake["version"])
	require.Equal(t, "1.0.0", bake["parent"])
	require.Equal(t, "amd64", bake["arch"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\napt-get install -y ripgrep\n")), bake["recipe"])
	platform, ok := task.Config["platform"].(map[string]any)
	require.True(t, ok, "the clone mounts the bundle the box mounts")
	require.Equal(t, "1.10.0", platform["version"])
	require.Equal(t, spec.PlatformSHA256, platform["sha256"])
	require.Equal(t, spec.TaskCPUShares()+bakeCPUShares, *task.Resources.CPU)
	require.Equal(t, spec.TaskMemoryMiB(), *task.Resources.MemoryMB)

	// The result is printed, and the box is said to keep its work.
	out := ta.errOut.String()
	require.Contains(t, out, "stopping box dev1")
	require.Contains(t, out, "baked the image project-1 (amd64)")
	require.Contains(t, out, strings.Repeat("ab", 32))
	require.Contains(t, out, "box dev1 runs again on the same disk: /work and the sessions are still there")
	require.Contains(t, out, "keeps its work")
	require.Contains(t, out, "box images publish project-1 --yes")
	require.NotContains(t, out, "sealed and stopped")
	require.True(t, callCount(ta, "StopJob box-bake-dev1 purge=true") > 0, "the bake job is purged")
}

// TestBakeRefusesABusyBoxWithoutStoppingIt: the seal refuses a box with a turn
// running, and that is asked in the box -- before it is stopped -- because the
// clone's own daemon is fresh and would answer at once.
func TestBakeRefusesABusyBoxWithoutStoppingIt(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":1,` +
		`"seal":{"ok":false,"reason":"session s-1 is running turn 1: a seal removes the sessions and their transcripts, so cancel it first (` + "`box cancel dev1 s-1`" + `)"}}`

	rc := ta.run(t, "bake", "dev1", "--name", "project")
	require.Equal(t, ExitError, rc)
	out := ta.errOut.String()
	require.Contains(t, out, "box dev1 is busy")
	require.Contains(t, out, "running turn 1")
	require.Contains(t, out, "Nothing was stopped")
	require.Zero(t, callCount(ta, "StopJob box-dev1"), "a busy box is refused without a stop")
	require.Zero(t, callCount(ta, "RegisterJob box-bake"), "nothing is registered")
}

// TestBakeRefusesACopyThatDoesNotFit: a clone is a full copy of the disk, so a node
// without the room is refused with a sentence before the box is stopped.
func TestBakeRefusesACopyThatDoesNotFit(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.sealMayRun()
	ta.cluster.mu.Lock()
	ta.cluster.nodes["node-1"].Attributes[boxDiskAttrPrefix+"free_bytes"] = strconv.FormatInt(1<<20, 10)
	ta.cluster.mu.Unlock()

	rc := ta.run(t, "bake", "dev1", "--name", "project")
	require.Equal(t, ExitError, rc)
	out := ta.errOut.String()
	require.Contains(t, out, "free where a bake writes the copy")
	require.Contains(t, out, "needs")
	require.Zero(t, callCount(ta, "StopJob box-dev1"), "a bake whose copy does not fit does not stop the box")
	require.Zero(t, callCount(ta, "RegisterJob box-bake"), "nothing is registered")
}

// TestBakeCountsALeftoverCloneAsRoom: the clone a killed bake left is deleted by
// the driver before it measures, so its disk must not refuse the bake here.
func TestBakeCountsALeftoverCloneAsRoom(t *testing.T) {
	const used = int64(300 << 20)
	const both = used + bakeCloneGrowthBytes + used
	for _, tc := range []struct {
		name    string
		clone   string
		refuses bool
	}{
		{name: "this box's clone frees its room", clone: "dev1-bake-1"},
		{name: "another box's clone does not", clone: "dev1-other-bake-1", refuses: true},
		{name: "a box named like a clone without a number does not", clone: "dev1-bake-x", refuses: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ta := newTestApp(t, platformIndexJSON)
			ta.addBoxSpec(t, goodProjectSpec())
			ta.sealMayRun()
			ta.cluster.mu.Lock()
			ta.cluster.addNodeDiskLocked("node-1", tc.clone, used, 8<<30)
			ta.cluster.nodes["node-1"].Attributes[boxDiskAttrPrefix+"free_bytes"] = strconv.FormatInt(both-used, 10)
			ta.cluster.mu.Unlock()

			rc := ta.run(t, "bake", "dev1", "--name", "project")
			if tc.refuses {
				require.Equal(t, ExitError, rc)
				require.Contains(t, ta.errOut.String(), "free where a bake writes the copy")
				require.Zero(t, callCount(ta, "StopJob box-dev1"))
				return
			}
			require.NotContains(t, ta.errOut.String(), "free where a bake writes the copy")
			require.NotZero(t, callCount(ta, "RegisterJob box-bake"), "the bake goes on to the driver")
		})
	}
}

// TestBakeNeedsRoomForTheCopyAndTheImage: a bake writes the copy of the disk and
// then the image beside it, so the node must hold both: one filesystem holds the
// sum, and a node whose image cache is a filesystem of its own must hold each.
func TestBakeNeedsRoomForTheCopyAndTheImage(t *testing.T) {
	const used = int64(300 << 20) // what addBoxSpec makes the box's disk occupy
	const copyNeed = used + bakeCloneGrowthBytes
	const both = copyNeed + used
	for _, tc := range []struct {
		name      string
		limaFree  int64
		cacheFree int64
		refuses   bool
		refusal   []string
	}{
		{
			name:     "one filesystem, exactly enough",
			limaFree: both,
		},
		{
			name:     "one filesystem, one byte short",
			limaFree: both - 1,
			refuses:  true,
			refusal:  []string{"free where a bake writes the copy", "needs", humanBytes(copyNeed), humanBytes(used)},
		},
		{
			name:      "two filesystems, the Lima home one byte short",
			limaFree:  copyNeed - 1,
			cacheFree: used,
			refuses:   true,
			refusal:   []string{"free where a bake writes the copy", "image cache", "needs"},
		},
		{
			name:      "two filesystems, the image cache one byte short",
			limaFree:  copyNeed,
			cacheFree: used - 1,
			refuses:   true,
			refusal:   []string{"free where a bake writes the copy", "image cache"},
		},
		{
			name:      "two filesystems, each exactly enough",
			limaFree:  copyNeed,
			cacheFree: used,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ta := newTestApp(t, platformIndexJSON)
			ta.addBoxSpec(t, goodProjectSpec())
			ta.finishWorkJobs(t, bakeDetails(), "")
			ta.sealMayRun()
			ta.cluster.mu.Lock()
			attrs := ta.cluster.nodes["node-1"].Attributes
			attrs[boxDiskFreeAttr] = strconv.FormatInt(tc.limaFree, 10)
			if tc.cacheFree > 0 {
				attrs[boxDiskImageCacheFreeAttr] = strconv.FormatInt(tc.cacheFree, 10)
			}
			ta.cluster.mu.Unlock()

			rc := ta.run(t, "bake", "dev1", "--name", "project")
			if !tc.refuses {
				require.Equal(t, ExitOK, rc, ta.errOut.String())
				require.True(t, callCount(ta, "RegisterJob box-bake-dev1") > 0, "the bake is registered")
				return
			}
			require.Equal(t, ExitError, rc, ta.errOut.String())
			for _, want := range tc.refusal {
				require.Contains(t, ta.errOut.String(), want)
			}
			require.Zero(t, callCount(ta, "StopJob box-dev1"), "the refusal comes before anything is stopped")
			require.Zero(t, callCount(ta, "RegisterJob box-bake"), "nothing is registered")
		})
	}
}

// TestBakeTakesNoYes: nothing is destroyed any more, so the flag that said "seal
// it anyway" is gone.
func TestBakeTakesNoYes(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	rc := ta.run(t, "bake", "dev1", "--name", "project", "--yes")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "yes")
	require.Zero(t, callCount(ta, "StopJob box-dev1"))
}

// TestBakeRefusesWhatItCannotSeal: an image without the bundle and two recipe
// flags are refused before anything happens.
func TestBakeRefusesWhatItCannotSeal(t *testing.T) {
	t.Run("an image that carries its own boxd", func(t *testing.T) {
		ta := newTestApp(t, indexJSON)
		ta.addBox(t, "dev1") // an image without a platform entry
		rc := ta.run(t, "bake", "dev1", "--name", "project")
		require.Equal(t, ExitError, rc)
		require.Contains(t, ta.errOut.String(), "takes no platform bundle")
		require.Contains(t, ta.errOut.String(), "only an image with")
	})
	t.Run("two recipe flags", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.addBoxSpec(t, goodProjectSpec())
		rc := ta.run(t, "bake", "dev1", "--name", "project", "--recipe", "/tmp/x", "--no-recipe")
		require.Equal(t, ExitUsage, rc)
		require.Contains(t, ta.errOut.String(), "--recipe and --no-recipe")
	})
	t.Run("a name that is not a project name", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.addBoxSpec(t, goodProjectSpec())
		rc := ta.run(t, "bake", "dev1", "--name", "1.0.0")
		require.Equal(t, ExitUsage, rc)
		require.Contains(t, ta.errOut.String(), "looks like a base image")
	})
	t.Run("an orphan", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.addBoxSpec(t, goodProjectSpec())
		ta.cluster.mu.Lock()
		delete(ta.cluster.jobs, "box-dev1")
		ta.cluster.mu.Unlock()
		rc := ta.run(t, "bake", "dev1", "--name", "project")
		require.Equal(t, ExitError, rc)
		require.Contains(t, ta.errOut.String(), "is an orphan")
	})
}

// TestBakeLeavesAStoppedBoxStopped: a bake of a stopped box works -- its disk is
// copied all the same -- and it does not start the box.
func TestBakeLeavesAStoppedBoxStopped(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.stopBox("dev1")
	ta.finishWorkJobs(t, bakeDetails(), "")

	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--no-recipe"), ta.errOut.String())
	require.Zero(t, callCount(ta, "StopJob box-dev1"), "the box was already stopped")
	require.Zero(t, callCount(ta, "RegisterJob box-dev1"), "a stopped box stays stopped")
	require.NotContains(t, ta.errOut.String(), "runs again")
	require.Contains(t, ta.errOut.String(), "baked the image project-1 (amd64)")
}

// TestBakeSaysWhenTheBoxDoesNotComeBack: a box that was running and cannot be
// started again is an error that names `box start`, and the image is not lost
// with it.
func TestBakeSaysWhenTheBoxDoesNotComeBack(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.sealMayRun()
	ta.finishWorkJobs(t, bakeDetails(), "")
	// The restart fails: Nomad rejects the box's job the second time.
	ta.cluster.registerErr = fmt.Errorf("no eligible nodes")
	require.NotNil(t, ta.run(t, "bake", "dev1", "--name", "project", "--no-recipe"))
	require.Contains(t, ta.errOut.String(), "does not run again")
	require.Contains(t, ta.errOut.String(), "box start dev1")
}

// TestBakeCountsTheVersionsItCanSee: the version is NAME-N, one above the highest
// the published images and the drafts have.
func TestBakeCountsTheVersionsItCanSee(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-3", "project", "1.0.0", 3)
	ta.addBoxSpec(t, goodProjectSpec())
	details := bakeDetails()
	details["version"] = "project-4"
	ta.finishWorkJobs(t, details, "")
	ta.sealMayRun()

	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--no-recipe"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "project-4")
	task, err := boxTask(ta.workJob)
	require.NoError(t, err)
	bake, _ := task.Config["bake"].(map[string]any)
	require.Equal(t, "project-4", bake["version"], "3 is a draft, so the new version is 4")
}

// job returns one registered job of the fake cluster.
func (ta *testApp) job(t *testing.T, id string) *api.Job {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	job, ok := ta.cluster.jobs[id]
	require.True(t, ok, "the job %s is registered", id)
	return job
}

// withDraft publishes a draft of a project image on the first node, the way the
// driver reports one in its fingerprint: one attribute per field of the record.
func (ta *testApp) withDraft(t *testing.T, version, name, parent string, n int) *DraftImage {
	t.Helper()
	sha := strings.Repeat("7f", 32)
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	node := ta.cluster.nodes["node-1"]
	require.NotNil(t, node)
	if node.Attributes == nil {
		node.Attributes = map[string]string{}
	}
	prefix := draftAttributePrefix + version + "."
	node.Attributes[prefix+"arch"] = "amd64"
	node.Attributes[prefix+"name"] = name
	node.Attributes[prefix+"parent"] = parent
	node.Attributes[prefix+"sha256"] = sha
	node.Attributes[prefix+"size"] = "1048576"
	node.Attributes[prefix+"virtual_bytes"] = strconv.FormatInt(int64(n)<<30, 10)
	node.Attributes[prefix+"built_at"] = "2026-11-02T11:00:00Z"
	node.Attributes[prefix+"platform"] = "mount"
	node.Attributes[prefix+"recipe"] = base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\necho recipe\n"))
	draft := DraftImage{Version: version, Arch: "amd64", Name: name, Parent: parent, SHA256: sha,
		Size: 1048576, VirtualBytes: int64(n) << 30, BuiltAt: "2026-11-02T11:00:00Z",
		Platform: "mount", Recipe: node.Attributes[prefix+"recipe"], NodeID: "node-1", NodeName: "node-1"}
	return &draft
}

// TestBakeRefusesARecipeItCannotCarry: the recipe travels in the image's record.
func TestBakeRefusesARecipeItCannotCarry(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	big := filepath.Join(t.TempDir(), "big.sh")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("#!/bin/bash\n", 7000)), 0o644))
	rc := ta.run(t, "bake", "dev1", "--name", "project", "--recipe", big)
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "more than the 65536")
}

// TestBakePrintsTheTaskLog: the driver writes every step of a bake -- and every
// line of the seal -- into the task's own log, which is where the command reads
// them. Nomad keeps ten task events, and a seal prints more.
func TestBakePrintsTheTaskLog(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.finishWorkJobs(t, bakeDetails(), "")
	ta.sealMayRun()
	ta.cluster.mu.Lock()
	ta.cluster.logData["stdout"] = "cloned box dev1 into dev1-bake-1: a copy of its stopped disk\n" +
		"  guest/no-secrets.sh: 21 check(s) passed\n" +
		"deleted the clone dev1-bake-1\n"
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--no-recipe"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "cloned box dev1 into dev1-bake-1")
	require.Contains(t, out, "guest/no-secrets.sh: 21 check(s) passed")
	require.Contains(t, out, "deleted the clone dev1-bake-1")
}

// TestRebuildBakesTheTemporaryBoxAndDeletesIt: a rebuild creates its temporary box
// from the newer base image, runs the recipe, bakes it -- the same bake as any box,
// on a clone -- and deletes the box instead of starting it again.
func TestRebuildBakesTheTemporaryBoxAndDeletesIt(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.finishWorkJobs(t, map[string]string{"kind": "bake", "version": "project-3", "arch": "amd64",
		"name": "project", "parent": "1.0.0", "sha256": strings.Repeat("ef", 32), "size": "2048"}, "")

	require.Equal(t, ExitOK, ta.run(t, "bake", "--rebuild", "project-2", "--base", "1.0.0"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "rebuilding project-2: a temporary box project-rebuild")
	require.Contains(t, out, "baked the image project-3 (amd64)")
	require.Contains(t, out, "from    project-2 (rebuilt on 1.0.0)")

	// The temporary box was stopped for its bake, and it was deleted instead of
	// being started again.
	require.True(t, callCount(ta, "StopJob box-project-rebuild purge=false") > 0, "the temporary box is stopped")
	require.Equal(t, 1, countRegisterOf(ta, "box-project-rebuild"),
		"the temporary box is registered once (its create) and not started again")
	require.True(t, callCount(ta, "StopJob box-project-rebuild purge=true") > 0, "the temporary box is deleted")
	// And the bake job it registered is an ordinary bake of that box.
	require.Equal(t, BoxBakeJobName("project-rebuild"), derefStr(ta.workJob.ID, ""))
}

func TestRebuildArm64UsesRequestedVersionWhenAmd64HasSameBase(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.addNode("node-arm", "node-arm", "arm64", 12000, 32768, 150000)
	ta.withArm64Draft(t, "project-2", "1.0.0")
	ta.withDraft(t, "project-3", "project", "1.0.0", 8)
	ta.finishWorkJobs(t, map[string]string{"kind": "bake", "version": "project-3", "arch": "arm64",
		"name": "project", "parent": "1.0.0", "sha256": strings.Repeat("ef", 32), "size": "2048"}, "")
	require.Equal(t, ExitOK, ta.run(t, "bake", "--rebuild", "project-2", "--base", "1.0.0", "--arch", "arm64", "--version", "project-3"), ta.errOut.String())
	task, err := boxTask(ta.workJob)
	require.NoError(t, err)
	bake := task.Config["bake"].(map[string]any)
	require.Equal(t, "project-3", bake["version"])
	require.Equal(t, "arm64", bake["arch"])
}

func TestRebuildArm64RefusesAmd64VersionWithDifferentBase(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.addNode("node-arm", "node-arm", "arm64", 12000, 32768, 150000)
	ta.withArm64Draft(t, "project-2", "1.0.0")
	ta.withDraft(t, "project-3", "project", "2.0.0", 8)
	rc := ta.run(t, "bake", "--rebuild", "project-2", "--base", "1.0.0", "--arch", "arm64", "--version", "project-3")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "must have rebuild base 1.0.0 as parent")
	require.Zero(t, callCount(ta, "RegisterJob box-project-rebuild"))
}

// withArm64Draft publishes only the other architecture of an image version.
func (ta *testApp) withArm64Draft(t *testing.T, version, parent string) {
	d := ta.withDraft(t, version, "project", parent, 8)
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	node := ta.cluster.nodes[d.NodeID]
	prefix := draftAttributePrefix + version + "."
	for _, key := range []string{"arch", "name", "parent", "sha256", "size", "virtual_bytes", "built_at", "platform", "recipe"} {
		delete(node.Attributes, prefix+key)
	}
	for key, value := range map[string]string{"arch": "arm64", "name": "project", "parent": parent, "sha256": strings.Repeat("8a", 32), "size": "1048576", "virtual_bytes": strconv.FormatInt(8<<30, 10), "built_at": "2026-11-02T11:00:00Z", "platform": "mount", "recipe": base64.StdEncoding.EncodeToString([]byte("echo recipe\n"))} {
		node.Attributes[prefix+key] = value
	}
}

// TestBakeVersionRefusalsDoNotStopOrRegister checks explicit version validation before side effects.
func TestBakeVersionRefusalsDoNotStopOrRegister(t *testing.T) {
	for _, tc := range []struct {
		name, version, want string
		setup               func(*testApp)
	}{
		{"wrong name", "other-1", "belongs to", nil},
		{"already exists for box arch", "project-2", "already exists", func(ta *testApp) { ta.withDraft(t, "project-2", "project", "1.0.0", 8) }},
		{"target does not exist", "project-3", "does not exist", nil},
		{"opposite architecture has wrong parent", "project-2", "must have the box's source image", func(ta *testApp) { ta.withArm64Draft(t, "project-2", "2.0.0") }},
		{"opposite architecture missing", "project-2", "does not exist", func(ta *testApp) {
			ta.withArm64Draft(t, "project-2", "1.0.0")
			ta.cluster.mu.Lock()
			n := ta.cluster.nodes["node-1"]
			delete(n.Attributes, draftAttributePrefix+"project-2.arch")
			delete(n.Attributes, draftAttributePrefix+"project-2.name")
			delete(n.Attributes, draftAttributePrefix+"project-2.parent")
			delete(n.Attributes, draftAttributePrefix+"project-2.sha256")
			delete(n.Attributes, draftAttributePrefix+"project-2.size")
			delete(n.Attributes, draftAttributePrefix+"project-2.virtual_bytes")
			delete(n.Attributes, draftAttributePrefix+"project-2.built_at")
			delete(n.Attributes, draftAttributePrefix+"project-2.platform")
			delete(n.Attributes, draftAttributePrefix+"project-2.recipe")
			ta.cluster.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ta := newTestApp(t, platformIndexJSON)
			ta.addBoxSpec(t, goodProjectSpec())
			if tc.setup != nil {
				tc.setup(ta)
			}
			ta.sealMayRun()
			rc := ta.run(t, "bake", "dev1", "--name", "project", "--version", tc.version, "--no-recipe")
			wantCode := ExitError
			if tc.name == "wrong name" {
				wantCode = ExitUsage
			}
			if tc.name == "target does not exist" || tc.name == "opposite architecture missing" {
				wantCode = ExitNotFound
			}
			require.Equal(t, wantCode, rc)
			require.Contains(t, ta.errOut.String(), tc.want)
			require.Zero(t, callCount(ta, "StopJob box-dev1"))
			require.Zero(t, callCount(ta, "RegisterJob box-bake"))
		})
	}
}

func TestBakeVersionRegistersRequestedVersionForArm64(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.addNode("node-arm", "node-arm", "arm64", 12000, 32768, 150000)
	spec := goodProjectSpec()
	spec.Arch, spec.NodeID = "arm64", "node-arm"
	spec.ImageVersion = "1.1.1"
	spec.ImageURL = "https://images.test/box-base/1.1.1/arm64/box.qcow2"
	ta.addBoxSpec(t, spec)
	ta.withDraft(t, "project-1", "project", "1.1.1", 8)
	ta.withDraftArch(t, "project-1", "amd64", "1.1.1", "recipe-one")
	ta.finishWorkJobs(t, map[string]string{"kind": "bake", "version": "project-1", "arch": "arm64", "name": "project", "parent": "1.1.1", "sha256": strings.Repeat("ab", 32), "size": "1048576"}, "")
	ta.sealMayRun()
	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--version", "project-1"), ta.errOut.String())
	task, err := boxTask(ta.workJob)
	require.NoError(t, err)
	bake := task.Config["bake"].(map[string]any)
	require.Equal(t, "project-1", bake["version"])
	require.Equal(t, "arm64", bake["arch"])
}

func (ta *testApp) withDraftArch(t *testing.T, version, arch, parent, recipe string) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	node := ta.cluster.nodes["node-1"]
	prefix := draftAttributePrefix + version + "."
	node.Attributes[prefix+"arch"] = arch
	node.Attributes[prefix+"parent"] = parent
	node.Attributes[prefix+"recipe"] = encodeRecipe([]byte(recipe))
}

// countRegisterOf counts the registrations of one job.
func countRegisterOf(ta *testApp, id string) int {
	return callCount(ta, "RegisterJob "+id)
}
