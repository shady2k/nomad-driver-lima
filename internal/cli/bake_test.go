package cli

import (
	"encoding/base64"
	"encoding/json"
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

// sealAnswer is what boxctl seal prints: a line per step, then the record.
const sealAnswer = `{"line":"closing the box's sessions and stopping its executors"}
{"line":"the workspace: deleted /work (12.0 MiB)"}
{"line":"searched 20 file(s) on the disk for 2 credential value(s): none of them is there"}
{"seal":{"image":{"schema":1,"version":"project-1","name":"project","arch":"amd64","parent":"1.0.0","platform":"mount","recipe":"IyEvYmluL2Jhc2gK","sealed_at":"2026-11-02T12:00:00Z"},"steps":["closing the box's sessions"],"files":20,"values":2}}
`

// finishWorkJobs makes the fake cluster run every batch job the command
// registers: the driver's task ends with the event that says what it did.
func (ta *testApp) finishWorkJobs(t *testing.T, details map[string]string, failure string) {
	t.Helper()
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		id := derefStr(job.ID, "")
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

// TestBakeSealsStopsAndBakes drives the whole `box bake` of a box.
func TestBakeSealsStopsAndBakes(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.finishWorkJobs(t, map[string]string{
		"kind": "bake", "version": "project-1", "arch": "amd64", "name": "project", "parent": "1.0.0",
		"sha256": strings.Repeat("ab", 32), "size": "1048576", "virtual_bytes": "8589934592",
	}, "")
	recipe := filepath.Join(t.TempDir(), "recipe.sh")
	require.NoError(t, os.WriteFile(recipe, []byte("#!/bin/bash\napt-get install -y ripgrep\n"), 0o644))
	ta.guest.responses["seal --name project --version project-1 --parent 1.0.0 --arch amd64 --recipe -"] = sealAnswer

	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--recipe", recipe, "--yes"), ta.errOut.String())

	// The recipe goes in with the seal, in the box.
	require.Contains(t, strings.Join(ta.guest.stdin, "\n"), "apt-get install -y ripgrep")
	// The seal's own lines reach the terminal as they happen.
	require.Contains(t, ta.errOut.String(), "the workspace: deleted /work")
	require.Contains(t, ta.errOut.String(), "sealing box dev1 into the image project-1")

	// The box is stopped, and the bake job carries the volume, the image and the
	// mark that keeps it out of `box ls`.
	job := ta.workJob
	require.NotNil(t, job, "the bake job was registered")
	require.Equal(t, BoxBakeJobName("dev1"), derefStr(job.ID, ""))
	require.Equal(t, "batch", derefStr(job.Type, ""))
	require.Equal(t, "bake", job.Meta[boxWorkMarker])
	group := job.TaskGroups[0]
	require.Equal(t, BoxVolumeName("dev1"), group.Volumes[boxDiskVolume].Source)
	require.True(t, group.Volumes[boxDiskVolume].ReadOnly, "a bake only reads the disk")
	task, err := boxTask(ta.workJob)
	require.NoError(t, err)
	bake, ok := task.Config["bake"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "project", bake["name"])
	require.Equal(t, "project-1", bake["version"])
	require.Equal(t, "1.0.0", bake["parent"])
	require.Equal(t, "amd64", bake["arch"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\napt-get install -y ripgrep\n")), bake["recipe"])

	// The result is printed, and the next steps with it.
	out := ta.errOut.String()
	require.Contains(t, out, "baked the image project-1 (amd64)")
	require.Contains(t, out, strings.Repeat("ab", 32))
	require.Contains(t, out, "box rm dev1 --yes")
	require.Contains(t, out, "starting it gives an empty, clean machine")
	require.Contains(t, out, "box images publish project-1 --yes")
	require.True(t, callCount(ta, "StopJob box-dev1 purge=false") > 0, "the sealed box is stopped")
	require.True(t, callCount(ta, "StopJob box-bake-dev1 purge=true") > 0, "the bake job is purged")
}

// TestBakeNeedsYes: the seal deletes the work, so a bake refuses without --yes and
// says what to do first.
func TestBakeNeedsYes(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBoxSpec(t, goodProjectSpec())
	rc := ta.run(t, "bake", "dev1", "--name", "project")
	require.Equal(t, ExitError, rc)
	out := ta.errOut.String()
	require.Contains(t, out, "seals it")
	require.Contains(t, out, "/work")
	require.Contains(t, out, "Push the work out of the box first")
	require.Contains(t, out, "--yes")
	require.Zero(t, callCount(ta, "RegisterJob box-bake"), "nothing is registered without --yes")
}

// TestBakeRefusesWhatItCannotSeal: a stopped box, an image without the bundle, and
// two recipe flags are refused before anything happens.
func TestBakeRefusesWhatItCannotSeal(t *testing.T) {
	t.Run("a stopped box", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.addBoxSpec(t, goodProjectSpec())
		ta.cluster.mu.Lock()
		ta.cluster.jobs["box-dev1"].Stop = boolPtr(true)
		for _, alloc := range ta.cluster.allocs {
			alloc.ClientStatus = "complete"
		}
		ta.cluster.mu.Unlock()
		rc := ta.run(t, "bake", "dev1", "--name", "project", "--yes")
		require.Equal(t, ExitError, rc)
		require.Contains(t, ta.errOut.String(), "the seal runs inside the box")
		require.Contains(t, ta.errOut.String(), "box start dev1")
	})
	t.Run("an image that carries its own boxd", func(t *testing.T) {
		ta := newTestApp(t, indexJSON)
		ta.addBox(t, "dev1") // an image without a platform entry
		rc := ta.run(t, "bake", "dev1", "--name", "project", "--yes")
		require.Equal(t, ExitError, rc)
		require.Contains(t, ta.errOut.String(), "takes no platform bundle")
		require.Contains(t, ta.errOut.String(), "only an image with")
	})
	t.Run("two recipe flags", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.addBoxSpec(t, goodProjectSpec())
		rc := ta.run(t, "bake", "dev1", "--name", "project", "--recipe", "/tmp/x", "--no-recipe", "--yes")
		require.Equal(t, ExitUsage, rc)
		require.Contains(t, ta.errOut.String(), "--recipe and --no-recipe")
	})
	t.Run("a name that is not a project name", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.addBoxSpec(t, goodProjectSpec())
		rc := ta.run(t, "bake", "dev1", "--name", "1.0.0", "--yes")
		require.Equal(t, ExitUsage, rc)
		require.Contains(t, ta.errOut.String(), "looks like a base image")
	})
}

// TestBakeCountsTheVersionsItCanSee: the version is NAME-N, one above the highest
// the published images and the drafts have.
func TestBakeCountsTheVersionsItCanSee(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-3", "project", "1.0.0", 3)
	ta.addBoxSpec(t, goodProjectSpec())
	ta.finishWorkJobs(t, map[string]string{"kind": "bake", "version": "project-4", "arch": "amd64",
		"name": "project", "sha256": strings.Repeat("cd", 32), "size": "1024"}, "")
	ta.guest.responses["seal --name project --version project-4 --parent 1.0.0 --arch amd64"] = sealAnswer

	require.Equal(t, ExitOK, ta.run(t, "bake", "dev1", "--name", "project", "--no-recipe", "--yes"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "project-4")
	task, err := boxTask(ta.workJob)
	require.NoError(t, err)
	bake, _ := task.Config["bake"].(map[string]any)
	require.Equal(t, "project-4", bake["version"], "3 is a draft, so the new version is 4")
}

// TestBakeJobIsNotABox: the batch jobs of an image are not boxes: neither the box
// list, nor the capacity count, nor a job name takes them for one.
func TestBakeJobIsNotABox(t *testing.T) {
	if id, ok := BoxIDFromJobName("box-bake-dev1"); ok {
		t.Fatalf("box-bake-dev1 was taken for box %q", id)
	}
	if id, ok := BoxIDFromJobName("box-publish-project-1"); ok {
		t.Fatalf("box-publish-project-1 was taken for box %q", id)
	}
	if id, ok := BoxIDFromJobName("box-dev1"); !ok || id != "dev1" {
		t.Fatalf("box-dev1 is a box: %q, %v", id, ok)
	}

	ta := newTestApp(t, platformIndexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.mu.Lock()
	ta.cluster.jobs["box-bake-dev1"] = &api.Job{
		ID: strPtr("box-bake-dev1"), Name: strPtr("box-bake-dev1"), Type: strPtr("batch"),
		Meta: map[string]string{boxWorkMarker: "bake"},
	}
	ta.cluster.allocSeq++
	ta.cluster.allocs["alloc-bake"] = &api.Allocation{
		ID: "alloc-bake", JobID: "box-bake-dev1", NodeID: "node-1", NodeName: "node-1",
		ClientStatus: "running", DesiredStatus: "run", CreateIndex: uint64(ta.cluster.allocSeq),
	}
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "ls", "--json"))
	var ls lsOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &ls))
	require.Len(t, ls.Boxes, 1, "the bake job is not a box: %+v", ls.Boxes)
	require.Equal(t, "dev1", ls.Boxes[0].ID)

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "capacity", "--json"))
	var cap capacityOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &cap))
	for _, node := range cap.Nodes {
		require.Equal(t, 1, node.Boxes, "a bake job is not a box in the capacity count either")
	}
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
	rc := ta.run(t, "bake", "dev1", "--name", "project", "--recipe", big, "--yes")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "more than the 65536")
}
