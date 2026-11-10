package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// Project images in the commands that deal with them: the listing, creating a box
// from a draft, publishing a draft, and the store a publish reads.

// TestImagesListsProjectImagesAndDrafts: the table says which images are base
// images, which are projects, and which of them exist on one node only.
func TestImagesListsProjectImagesAndDrafts(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 12)
	require.Equal(t, ExitOK, ta.run(t, "images"), ta.errOut.String())

	table := ta.out.String()
	require.Contains(t, table, "KIND")
	require.Contains(t, table, "BEHIND")
	require.Contains(t, table, "base")
	require.Contains(t, table, "project-2")
	require.Contains(t, table, "draft on node-1")
	require.Contains(t, table, "parent 1.0.0, newest base 2.0.0")
	// The hint to create a box names a base image, never a project image.
	require.Contains(t, ta.errOut.String(), "create a box with: box create <id> --image 2.0.0")
	require.Contains(t, ta.errOut.String(), "box images publish project-2 --yes")
	// The hint says how to size the box: the default is light work, a build needs
	// more from the start, and the node is shared.
	require.Contains(t, ta.errOut.String(), "a build or a test run needs more memory from the start")
	require.Contains(t, ta.errOut.String(), "the node is shared, so give back what you do not use")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "images", "--json"), ta.errOut.String())
	var doc imagesOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Drafts, 1)
	require.Equal(t, "project-2", doc.Drafts[0].Version)
	require.Equal(t, "node-1", doc.Drafts[0].NodeID)
	require.Equal(t, "1.0.0", doc.Drafts[0].Parent)
}

// TestLatestNeverPicksAProjectImage: `--image latest` means the newest base image.
func TestLatestNeverPicksAProjectImage(t *testing.T) {
	ta := newTestApp(t, `{
	  "schema": 1,
	  "images": {
	    "1.0.0": {"amd64": {"url": "https://images.test/1.0.0/amd64/box.qcow2", "sha256": "`+
		strings.Repeat("a1", 32)+`", "size": 1024, "platform": "mount"}},
	    "project-9": {"amd64": {"url": "file:///box-draft/project-9/amd64", "sha256": "`+
		strings.Repeat("b2", 32)+`", "size": 1024, "kind": "project", "name": "project", "platform": "mount"}}
	  },
	  "platforms": {"1.0.0": {"amd64": {"url": "https://images.test/p.tar.gz", "sha256": "`+
		strings.Repeat("c3", 32)+`", "size": 1024}}}
	}`)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev9", "--image", "latest", "--cpus", "1",
		"--memory-mib", "512", "--disk-gib", "4"), ta.errOut.String())
	image, ok := boxTaskConfig(t, ta, "box-dev9")["image"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "1.0.0", ta.cluster.jobs["box-dev9"].Meta["image"])
	require.Equal(t, "https://images.test/1.0.0/amd64/box.qcow2", image["url"])
}

// TestCreateFromADraft: a draft exists on one node, so the box runs there, its disk
// is at least the image's, and its url is never fetched.
func TestCreateFromADraft(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	draft := ta.withDraft(t, "project-2", "project", "1.0.0", 12)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }

	require.Equal(t, ExitOK, ta.run(t, "create", "dev2", "--image", "project-2", "--cpus", "1",
		"--memory-mib", "512"), ta.errOut.String())

	image, ok := boxTaskConfig(t, ta, "box-dev2")["image"].(map[string]any)
	require.True(t, ok, "the job has an image block")
	require.Equal(t, DraftURLPrefix+"project-2", image["url"], "a draft's url is never fetched")
	require.Equal(t, draft.SHA256, image["sha256"])
	// The disk is the image's: 12 GiB, not the default 8.
	require.Equal(t, 12, int(toInt(boxTaskConfig(t, ta, "box-dev2")["disk_gib"], 0)))
	volume := ta.cluster.volumes["vol-1"]
	require.NotNil(t, volume)
	require.Equal(t, "node-1", volume.NodeID, "a draft is created from on the node that holds it")
	require.Equal(t, int64(12)<<30, volume.CapacityBytes)

	ta.out.Reset()
	ta.errOut.Reset()
	rc := ta.run(t, "create", "dev3", "--image", "project-2", "--disk-gib", "4")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "smaller than the disk inside the image project-2")
	require.Contains(t, ta.errOut.String(), "Lima never shrinks a disk")
}

// TestCreateImageNameIsTheNewestVersion: `--image project` is the newest project-N.
func TestCreateImageNameIsTheNewestVersion(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.withDraft(t, "project-3", "project", "1.0.0", 8)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev4", "--image", "project", "--cpus", "1",
		"--memory-mib", "512", "--disk-gib", "8"), ta.errOut.String())
	require.Equal(t, "project-3", ta.cluster.jobs["box-dev4"].Meta["image"])
}

// TestCreateFromADraftOnAnotherNodeRefused: the draft's image is in one node's
// cache, so the box cannot run elsewhere.
func TestCreateFromADraftOnAnotherNodeRefused(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.addNode("node-2", "node-2", "amd64", 12000, 32768, 150000)
	rc := ta.run(t, "create", "dev5", "--image", "project-2", "--node", "node-2", "--cpus", "1",
		"--memory-mib", "512", "--disk-gib", "8")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "the draft project-2 is on node node-1")
}

// TestCreateChoosesTheRequestedArchitectureFromPublishedAndDraftArtifacts confirms
// an arm64 draft is selected when the same version has a published amd64 artifact.
func TestCreateChoosesTheRequestedArchitectureFromPublishedAndDraftArtifacts(t *testing.T) {
	base := strings.Repeat("1", 64)
	ta := newTestApp(t, platformIndexJSON)
	draft := ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.addNode("node-2", "node-2", "arm64", 12000, 32768, 150000)
	ta.cluster.mu.Lock()
	attrs := ta.cluster.nodes["node-2"].Attributes
	prefix := draftAttributePrefix + "project-2."
	attrs[prefix+"arch"] = "arm64"
	attrs[prefix+"name"] = "project"
	attrs[prefix+"parent"] = "1.0.0"
	attrs[prefix+"sha256"] = strings.Repeat("7f", 32)
	attrs[prefix+"size"] = "1048576"
	attrs[prefix+"virtual_bytes"] = "8589934592"
	attrs[prefix+"platform"] = "mount"
	ta.cluster.mu.Unlock()
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	rc := ta.run(t, "create", "dev1", "--image", "project-2", "--arch", "arm64", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "8")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	job := ta.cluster.jobs["box-dev1"]
	require.Equal(t, "arm64", job.Meta["arch"])
	require.NotNil(t, job)
	var createdVolume *api.HostVolume
	for _, volume := range ta.cluster.volumes {
		if volume.Name == "box-dev1" {
			createdVolume = volume
		}
	}
	require.NotNil(t, createdVolume)
	require.Equal(t, "node-2", createdVolume.NodeID)
	require.NotEqual(t, draft.SHA256, base)
}

func TestImagesPublishRefusesWithoutYes(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	rc := ta.run(t, "images", "publish", "project-2")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "Ask its owner before publishing")
	require.Zero(t, callCount(ta, "RegisterJob box-publish"))
}

// TestImagesPublishRegistersTheJob: the publish job is pinned to the draft's node
// and carries the store as templated files, never as a value.
func TestImagesPublishRegistersTheJob(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	draft := ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{
		"url":        "https://store.example.com",
		"token.tmpl": `{{ with secret "kv/data/box/store" }}{{ .Data.data.token }}{{ end }}`,
	}}
	ta.finishWorkJobs(t, map[string]string{"kind": "publish", "version": "project-2", "arch": "amd64",
		"sha256": draft.SHA256, "index": "https://store.example.com/box-base/index.json"}, "")

	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
	job := ta.workJob
	require.NotNil(t, job)
	require.Equal(t, "publish", job.Meta[boxWorkMarker])
	require.Equal(t, "project-2", job.Meta["image"])
	// Pinned to the node that holds the draft.
	require.Len(t, job.Constraints, 1)
	require.Equal(t, "${node.unique.id}", job.Constraints[0].LTarget)
	require.Equal(t, "node-1", job.Constraints[0].RTarget)
	task, err := boxTask(job)
	require.NoError(t, err)
	publish, ok := task.Config["publish"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, draft.SHA256, publish["sha256"])
	require.Equal(t, "amd64", publish["arch"])

	// The store reaches the task as two files: the url from the Variable, the
	// token through the template. Neither the job nor the variable holds a value.
	templates := job.TaskGroups[0].Tasks[0].Templates
	require.Len(t, templates, 2)
	dests := []string{derefStr(templates[0].DestPath, ""), derefStr(templates[1].DestPath, "")}
	require.Equal(t, []string{"secrets/store/url", "secrets/store/token"}, dests)
	for _, template := range templates {
		require.NotContains(t, derefStr(template.EmbeddedTmpl, ""), "the-store-token")
	}
	require.Contains(t, derefStr(templates[1].EmbeddedTmpl, ""), "secret",
		"the token is a template that reads Vault, not the token itself")
	require.NotNil(t, job.TaskGroups[0].Tasks[0].Vault, "a token template needs a Vault block")

	out := ta.errOut.String()
	require.Contains(t, out, "published project-2 (amd64) to https://store.example.com")
	require.Contains(t, out, "https://store.example.com/box-base/project-2/amd64/box-base-project-2-amd64.qcow2")
	require.Contains(t, out, "every node can create a box from it now")
	require.True(t, callCount(ta, "StopJob box-publish-project-2 purge=true") > 0)
}

// TestImagesPublishRendersTheTokenPlace: the header the token goes in and the
// word in front of it reach a publish as their own files, so the driver sends the
// token where this store reads it -- and an empty scheme is a file with nothing in
// it, which is a bare key, not a missing file.
func TestImagesPublishRendersTheTokenPlace(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	draft := ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{
		"url":    "https://store.example.com",
		"token":  "s3cret-token",
		"header": "X-Api-Key",
		"scheme": "",
	}}
	ta.finishWorkJobs(t, map[string]string{"kind": "publish", "version": "project-2", "arch": "amd64",
		"sha256": draft.SHA256, "index": "https://store.example.com/box-base/index.json"}, "")

	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
	templates := ta.workJob.TaskGroups[0].Tasks[0].Templates
	var dests []string
	for _, template := range templates {
		dests = append(dests, derefStr(template.DestPath, ""))
	}
	require.Equal(t, []string{"secrets/store/url", "secrets/store/token", "secrets/store/header", "secrets/store/scheme"}, dests)
	require.Nil(t, ta.workJob.TaskGroups[0].Tasks[0].Vault, "a plain token needs no Vault block")
}

// TestImagesPublishWithoutAStore: the store is a Variable, and its absence says
// which command writes it.
func TestImagesPublishTwoDraftsSequentially(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.addNode("node-2", "node-2", "arm64", 12000, 32768, 150000)
	ta.cluster.mu.Lock()
	attrs := ta.cluster.nodes["node-2"].Attributes
	prefix := draftAttributePrefix + "project-2."
	for k, v := range map[string]string{"arch": "arm64", "name": "project", "parent": "1.0.0", "sha256": strings.Repeat("8a", 32), "size": "1048576"} {
		attrs[prefix+k] = v
	}
	ta.cluster.mu.Unlock()
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{"url": "https://store.example.com"}}
	seen := []string{}
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		id := derefStr(job.ID, "")
		if !strings.HasPrefix(id, boxPublishJobPrefix) {
			return
		}
		task, err := boxTask(job)
		require.NoError(t, err)
		arch := task.Config["publish"].(map[string]any)["arch"].(string)
		seen = append(seen, arch)
		c.allocSeq++
		event := &api.TaskEvent{Type: "Driver", Time: 1, Details: map[string]string{"kind": "publish", "index": "https://store.example.com/index.json"}}
		state := &api.TaskState{State: "dead", StartedAt: time.Now(), FinishedAt: time.Now(), Events: []*api.TaskEvent{event}}
		c.allocs["alloc-work"] = &api.Allocation{ID: "alloc-work", JobID: id, Job: job, NodeID: "node-1", NodeName: "node-1", ClientStatus: "complete", DesiredStatus: "run", CreateIndex: uint64(c.allocSeq), TaskStates: map[string]*api.TaskState{boxTaskName: state}}
	}
	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
	require.Equal(t, []string{"amd64", "arm64"}, seen)
}

func TestImagesPublishExplicitArm64(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.addNode("node-2", "node-2", "arm64", 12000, 32768, 150000)
	ta.cluster.mu.Lock()
	attrs := ta.cluster.nodes["node-2"].Attributes
	prefix := draftAttributePrefix + "project-2."
	for k, v := range map[string]string{"arch": "arm64", "name": "project", "parent": "1.0.0", "sha256": strings.Repeat("8a", 32), "size": "1048576"} {
		attrs[prefix+k] = v
	}
	ta.cluster.mu.Unlock()
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{"url": "https://store.example.com"}}
	ta.finishWorkJobs(t, map[string]string{"kind": "publish", "version": "project-2", "arch": "arm64", "sha256": strings.Repeat("8a", 32), "index": "https://store.example.com/index.json"}, "")
	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--arch", "arm64", "--yes"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "published project-2 (arm64)")
}

func TestImagesPublishFailureReportsArchitectureStatus(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{"url": "https://store.example.com"}}
	ta.finishWorkJobs(t, nil, "upload failed")
	require.Equal(t, ExitError, ta.run(t, "images", "publish", "project-2", "--yes"))
	require.Contains(t, ta.errOut.String(), "published architectures: ; not published: amd64")
	require.Contains(t, ta.errOut.String(), "upload failed")
}

func TestImagesPublishSkipsAlreadyPublishedAMD64(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	amd := ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.addNode("node-2", "node-2", "arm64", 12000, 32768, 150000)
	ta.cluster.mu.Lock()
	attrs := ta.cluster.nodes["node-2"].Attributes
	prefix := draftAttributePrefix + "project-2."
	for k, v := range map[string]string{"arch": "arm64", "name": "project", "parent": "1.0.0", "sha256": strings.Repeat("8a", 32), "size": "1048576"} {
		attrs[prefix+k] = v
	}
	ta.cluster.mu.Unlock()
	index := `{"schema":1,"images":{"project-2":{"amd64":{"url":"https://store.example.com/x","sha256":"` + amd.SHA256 + `","size":1048576,"kind":"project","name":"project","parent":"1.0.0"}}}}`
	ta.images.body = index
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{"url": "https://store.example.com"}}
	seen := []string{}
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		id := derefStr(job.ID, "")
		if !strings.HasPrefix(id, boxPublishJobPrefix) {
			return
		}
		task, err := boxTask(job)
		require.NoError(t, err)
		seen = append(seen, task.Config["publish"].(map[string]any)["arch"].(string))
		c.allocSeq++
		event := &api.TaskEvent{Type: "Driver", Time: 1, Details: map[string]string{"kind": "publish", "index": "https://store.example.com/index.json"}}
		state := &api.TaskState{State: "dead", StartedAt: time.Now(), FinishedAt: time.Now(), Events: []*api.TaskEvent{event}}
		c.allocs["alloc-work"] = &api.Allocation{ID: "alloc-work", JobID: id, Job: job, NodeID: "node-1", NodeName: "node-1", ClientStatus: "complete", DesiredStatus: "run", CreateIndex: uint64(c.allocSeq), TaskStates: map[string]*api.TaskState{boxTaskName: state}}
	}
	require.Equal(t, ExitOK, ta.run(t, "images", "publish", "project-2", "--yes"), ta.errOut.String())
	require.Equal(t, []string{"arm64"}, seen)
}

func TestImagesPublishSecondArchitectureFailureReportsStatus(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.addNode("node-2", "node-2", "arm64", 12000, 32768, 150000)
	ta.cluster.mu.Lock()
	attrs := ta.cluster.nodes["node-2"].Attributes
	prefix := draftAttributePrefix + "project-2."
	for k, v := range map[string]string{"arch": "arm64", "name": "project", "parent": "1.0.0", "sha256": strings.Repeat("8a", 32), "size": "1048576"} {
		attrs[prefix+k] = v
	}
	ta.cluster.mu.Unlock()
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{"url": "https://store.example.com"}}
	registered := 0
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		id := derefStr(job.ID, "")
		if !strings.HasPrefix(id, boxPublishJobPrefix) {
			return
		}
		registered++
		c.allocSeq++
		var event *api.TaskEvent
		var client string
		if registered == 1 {
			event = &api.TaskEvent{Type: "Driver", Time: 1, Details: map[string]string{"kind": "publish", "index": "https://store.example.com/index.json"}}
			client = "complete"
		} else {
			event = &api.TaskEvent{Type: "Driver", Time: 1, Details: map[string]string{"kind": "exit"}, DisplayMessage: "upload failed"}
			client = "failed"
		}
		state := &api.TaskState{State: "dead", StartedAt: time.Now(), FinishedAt: time.Now(), Failed: client == "failed", Events: []*api.TaskEvent{event}}
		c.allocs["alloc-work"] = &api.Allocation{ID: "alloc-work", JobID: id, Job: job, NodeID: "node-1", NodeName: "node-1", ClientStatus: client, DesiredStatus: "run", CreateIndex: uint64(c.allocSeq), TaskStates: map[string]*api.TaskState{boxTaskName: state}}
	}
	require.Equal(t, ExitError, ta.run(t, "images", "publish", "project-2", "--yes"))
	require.Equal(t, 2, registered)
	require.Contains(t, ta.errOut.String(), "published architectures: amd64; not published: arm64")
	require.Contains(t, ta.errOut.String(), "upload failed")
}

func TestImagesPublishWithoutAStore(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	rc := ta.run(t, "images", "publish", "project-2", "--yes")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "no image store")
	require.Contains(t, ta.errOut.String(), "box store set")
}

// TestStoreSetAndShow: the CLI writes and reads the store's Variable, and never
// prints the token.
func TestStoreSetAndShow(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("a-store-token\n"), 0o600))

	require.Equal(t, ExitOK, ta.run(t, "store", "set", "--url", "https://store.example.com/",
		"--from-file", tokenFile), ta.errOut.String())
	v, err := ta.cluster.VariableRead(context.Background(), boxStoreVariable)
	require.NoError(t, err)
	require.Equal(t, "https://store.example.com", v.Items["url"], "the trailing slash goes")
	require.Equal(t, "a-store-token", v.Items["token"])
	require.Contains(t, ta.errOut.String(), "stored in the Variable")
	require.NotContains(t, ta.errOut.String(), "a-store-token", "the token is never printed")
	require.NotContains(t, ta.out.String(), "a-store-token")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "store", "show"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "https://store.example.com")
	require.Contains(t, ta.errOut.String(), "token")
	require.NotContains(t, ta.errOut.String(), "a-store-token")

	// A template item holds a path, not a value.
	templateFile := filepath.Join(t.TempDir(), "token.tmpl")
	require.NoError(t, os.WriteFile(templateFile,
		[]byte(`{{ with secret "kv/data/box/store" }}{{ .Data.data.token }}{{ end }}`), 0o644))
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "store", "set", "--url", "https://store.example.com",
		"--template", templateFile), ta.errOut.String())
	v, err = ta.cluster.VariableRead(context.Background(), boxStoreVariable)
	require.NoError(t, err)
	_, plain := v.Items["token"]
	require.False(t, plain, "a template replaces the plain item: one token, one way")
	require.Contains(t, v.Items["token.tmpl"], "secret")
	require.Contains(t, ta.errOut.String(), "read from Vault")

	// Two ways to give the token is a usage error, and so is a template that holds
	// no template text.
	ta.errOut.Reset()
	require.Equal(t, ExitUsage, ta.run(t, "store", "set", "--url", "https://s.example.com",
		"--from-file", tokenFile, "--template", templateFile))
	require.Contains(t, ta.errOut.String(), "give one")
	plainFile := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.WriteFile(plainFile, []byte("a-token"), 0o600))
	ta.errOut.Reset()
	require.Equal(t, ExitUsage, ta.run(t, "store", "set", "--url", "https://s.example.com", "--template", plainFile))
	require.Contains(t, ta.errOut.String(), "holds no template text")
}

// TestStoreShowWithoutAStore: a cluster with no store is a state, not a failure.
func TestStoreShowWithoutAStore(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	require.Equal(t, ExitOK, ta.run(t, "store", "show"))
	require.Contains(t, ta.errOut.String(), "no image store is configured")
}

// TestRebuildRefusals: a rebuild needs an image with a recipe, and a base image
// that a box can be baked from.
func TestRebuildRefusals(t *testing.T) {
	t.Run("no such image", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		rc := ta.run(t, "bake", "--rebuild", "nothing")
		require.Equal(t, ExitNotFound, rc)
		require.Contains(t, ta.errOut.String(), "no image is named")
	})
	t.Run("an image without a recipe", func(t *testing.T) {
		ta := newTestApp(t, platformIndexJSON)
		ta.withDraftNoRecipe(t, "project-2", "project", "1.0.0")
		rc := ta.run(t, "bake", "--rebuild", "project-2")
		require.Equal(t, ExitError, rc)
		require.Contains(t, ta.errOut.String(), "carries no recipe")
	})
	t.Run("a base image that cannot be baked", func(t *testing.T) {
		ta := newTestApp(t, indexJSON)
		ta.withDraft(t, "project-2", "project", "1.0.0", 8)
		rc := ta.run(t, "bake", "--rebuild", "project-2")
		require.Equal(t, ExitError, rc)
		require.Contains(t, ta.errOut.String(), "takes no platform bundle")
	})
}

func TestRecipeDifferenceNoteRequiresVersionAndDifferentRecipe(t *testing.T) {
	require.Empty(t, recipeDifferenceNote("", "arm64", "other", "carried"), "automatic versions have no cross-architecture target")
	require.Empty(t, recipeDifferenceNote("project-3", "arm64", "same", "same"), "identical recipes need no note")
	require.Equal(t, "note: target image project-3 on arm64 carries a different recipe from the one this bake will carry",
		recipeDifferenceNote("project-3", "arm64", "other", "carried"))
}

func TestBakeRecipeNoteOnlyForExplicitVersionDifference(t *testing.T) {
	for _, tc := range []struct{ name, version, targetRecipe, expected string }{
		{"explicit version differs", "project-2", encodeRecipe([]byte("other")), "carries a different recipe"},
		{"explicit version same", "project-2", "", ""},
		{"automatic version differs", "", encodeRecipe([]byte("other")), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ta := newTestApp(t, platformIndexJSON)
			ta.addBoxSpec(t, goodProjectSpec())
			ta.withArm64Draft(t, "project-2", "1.0.0")
			ta.cluster.mu.Lock()
			ta.cluster.nodes["node-1"].Attributes[draftAttributePrefix+"project-2.recipe"] = tc.targetRecipe
			ta.cluster.mu.Unlock()
			ta.finishWorkJobs(t, map[string]string{"kind": "bake", "version": "project-2", "arch": "amd64", "name": "project", "parent": "1.0.0", "sha256": strings.Repeat("ab", 32), "size": "1048576"}, "")
			ta.sealMayRun()
			args := []string{"bake", "dev1", "--name", "project", "--no-recipe"}
			if tc.version != "" {
				args = append(args, "--version", tc.version)
			}
			require.Equal(t, ExitOK, ta.run(t, args...), ta.errOut.String())
			if tc.expected == "" {
				require.NotContains(t, ta.errOut.String(), "carries a different recipe")
			} else {
				require.Contains(t, ta.errOut.String(), tc.expected)
			}
		})
	}
}

// TestRebuildCreatesTheBoxAndKeepsItWhenTheRecipeFails: the temporary box is what
// a failure leaves behind, because what it ran is the evidence.
func TestRebuildCreatesTheBoxAndKeepsItWhenTheRecipeFails(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.guest.runExit = 3

	// The newest base image is 2.0.0, which carries its own boxd: a rebuild names
	// the base image it starts from.
	rc := ta.run(t, "bake", "--rebuild", "project-2", "--base", "1.0.0")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the temporary box project-rebuild was kept")
	require.Contains(t, ta.errOut.String(), "box rm project-rebuild --yes")

	// The box was created from the newest base image, and the recipe ran in it.
	job := ta.job(t, "box-project-rebuild")
	require.Equal(t, "1.0.0", job.Meta["image"])
	command := ta.guest.calls[len(ta.guest.calls)-1]
	require.Equal(t, "setsid", command[0])
	require.Equal(t, "bash", command[len(command)-2])
	require.Equal(t, "-s", command[len(command)-1])
	require.Contains(t, strings.Join(ta.guest.stdin, "\n"), "echo recipe")
	require.Zero(t, callCount(ta, "RegisterJob box-bake-project-rebuild"), "a failed recipe bakes nothing")
}

// withDraftNoRecipe publishes a draft that carries no recipe.
func (ta *testApp) withDraftNoRecipe(t *testing.T, version, name, parent string) *DraftImage {
	t.Helper()
	draft := ta.withDraft(t, version, name, parent, 8)
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	node := ta.cluster.nodes["node-1"]
	delete(node.Attributes, draftAttributePrefix+version+".recipe")
	draft.Recipe = ""
	return draft
}

// TestImagesListsAPublishedDraftOnce: a publish leaves the draft in the cache of
// the node that baked it, so the same image and architecture would be listed twice
// -- once as published, once as "draft on node-1". It is one image.
func TestImagesListsAPublishedDraftOnce(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withDraft(t, "project-2", "project", "1.0.0", 12)
	// Publishing writes the index; the node keeps the draft either way.
	draft := ta.drafts(t)[0]
	published := `{
	  "schema": 1,
	  "updated_at": "2026-11-02T12:00:00Z",
	  "images": {
	    "1.0.0": {"amd64": {"url": "https://images.test/box-base/1.0.0/amd64/box.qcow2", "sha256": "` +
		strings.Repeat("5d", 32) + `", "size": 1024, "platform": "mount"}},
	    "project-2": {"amd64": {"url": "https://images.test/box-base/project-2/amd64/box.qcow2", "sha256": "` +
		draft.SHA256 + `", "size": ` + itoa(int(draft.Size)) + `, "kind": "project", "name": "project",
	      "parent": "1.0.0", "platform": "mount"}}
	  },
	  "platforms": {"1.0.0": {"amd64": {"url": "https://images.test/p.tar.gz", "sha256": "` +
		strings.Repeat("4a", 32) + `", "size": 1024, "boxd": "0.5.0"}}}
	}`
	ta.images.body = published
	require.Equal(t, ExitOK, ta.run(t, "images"), ta.errOut.String())

	require.Equal(t, 1, strings.Count(ta.out.String(), "project-2"), "the image is one row: %s", ta.out.String())
	require.Contains(t, ta.out.String(), "published")
	require.NotContains(t, ta.out.String(), "draft on node-1")
	require.NotContains(t, ta.errOut.String(), "box images publish project-2 --yes",
		"a published image has nothing left to publish")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "images", "--json"), ta.errOut.String())
	var doc imagesOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Empty(t, doc.Drafts, "a published image is not a draft any more")
}

// drafts reads the drafts the fake cluster reports.
func (ta *testApp) drafts(t *testing.T) []DraftImage {
	t.Helper()
	drafts, err := ta.nodeDrafts(t.Context())
	require.NoError(t, err)
	return drafts
}

// The tenth bake of a project is newer than the ninth.
func TestProjectVersionsOrderByNumber(t *testing.T) {
	if !versionLess("proj-9", "proj-10") {
		t.Fatal("proj-10 must be newer than proj-9")
	}
	next, err := NextImageVersion("proj", []string{"proj-9", "proj-10"})
	if err != nil || next != "proj-11" {
		t.Fatalf("next = %q, %v; want proj-11", next, err)
	}
}

// What an earlier run left must not answer for this one: Nomad starts nothing for
// an unchanged job, and its failed allocation would be read as this bake's.
func TestWorkJobPurgesTheLastRun(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	job := &api.Job{ID: strPtr("box-bake-a")}
	_, _, err := ta.registerWorkJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	stop, reg := -1, -1
	for i, call := range ta.cluster.calls {
		if strings.HasPrefix(call, "StopJob box-bake-a purge=true") {
			stop = i
		}
		if strings.HasPrefix(call, "RegisterJob box-bake-a") {
			reg = i
		}
	}
	if stop < 0 || stop > reg {
		t.Fatalf("the last job is not purged first: %v", ta.cluster.calls)
	}
}

func TestAllocsSinceDropsAnOlderRun(t *testing.T) {
	old := &api.AllocationListStub{ID: "old", CreateIndex: 10, ClientStatus: "failed"}
	cur := &api.AllocationListStub{ID: "new", CreateIndex: 20, ClientStatus: "running"}
	got := allocsSince([]*api.AllocationListStub{old, cur}, 15)
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("kept %v", got)
	}
	if len(allocsSince([]*api.AllocationListStub{old}, 0)) != 1 {
		t.Fatal("no index keeps everything")
	}
}
