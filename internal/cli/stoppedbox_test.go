package cli

// A box is its volume and its disk, not its job: these tests are the promise
// `box stop` makes -- `box start` brings the box back -- against a cluster whose
// job GC has deleted the job.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// stopAndCollect stops a box and then collects its job, the way `box stop` plus
// Nomad's job GC leave a box: a dead job, a volume, and the stored description.
func (ta *testApp) stopAndCollect(t *testing.T, id string) {
	t.Helper()
	require.Equal(t, ExitOK, ta.run(t, "stop", id), ta.errOut.String())
	ta.cluster.nomadGC()
	ta.cluster.mu.Lock()
	_, jobLeft := ta.cluster.jobs[BoxJobName(id)]
	ta.cluster.mu.Unlock()
	require.False(t, jobLeft, "the job is gone after the GC")
	ta.out.Reset()
	ta.errOut.Reset()
}

// storedSpec decodes the description of a box out of the fake cluster.
func (ta *testApp) storedSpec(t *testing.T, id string) (map[string]any, string) {
	t.Helper()
	ta.cluster.mu.Lock()
	variable, ok := ta.cluster.variables[BoxSpecVariable(id)]
	ta.cluster.mu.Unlock()
	require.True(t, ok, "the variable %s holds the description of box %s", BoxSpecVariable(id), id)
	text := variable.Items[boxSpecItem]
	doc := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(text), &doc), "the description is JSON")
	return doc, text
}

// TestCreateStoresTheDescription: `box create` writes the rendered job where
// Nomad's job GC cannot reach it, and that record holds no credential.
func TestCreateStoresTheDescription(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	// A credential reaches a box through the executors variable, which the job
	// reads at render time: the job itself carries the file name, not the value.
	ta.cluster.setExecutors(map[string]string{"opencode/opencode.json": `{"token":"sk-never-in-a-job"}`})

	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "2", "--memory-mib", "1024",
		"--disk-gib", "8"), ta.errOut.String())

	doc, text := ta.storedSpec(t, "dev1")
	require.Equal(t, float64(boxSpecSchema), doc["schema"])
	job, ok := doc["job"].(map[string]any)
	require.True(t, ok, "the description holds the job")
	require.Equal(t, "box-dev1", job["ID"])
	require.Equal(t, "true", job["Meta"].(map[string]any)["box"], "the description is enough to start the box again")
	require.Equal(t, "1.0.0", job["Meta"].(map[string]any)["image"])

	// Nothing secret: the value of a credential is never in a job.
	require.NotContains(t, text, "sk-never-in-a-job")
	require.Contains(t, text, "opencode/opencode.json")
	require.Less(t, len(text), boxSpecVariableLimit, "the description fits what Nomad stores")

	// The description is a property of the box, not of a start: the box runs.
	require.Equal(t, "running", ta.boxAfter(t, "dev1").State)
}

// boxAfter reads a box record the way `box ls` does.
func (ta *testApp) boxAfter(t *testing.T, id string) BoxRecord {
	t.Helper()
	boxes, _, err := ta.collectBoxes(t.Context())
	require.NoError(t, err)
	for _, box := range boxes {
		if box.ID == id {
			return box
		}
	}
	return BoxRecord{}
}

// TestCreateRemovesTheBoxWhenItsDescriptionCannotBeStored: a create that cannot
// store the description leaves no half box behind. The write comes after the
// registration, so the disk has to go again with the job.
func TestCreateRemovesTheBoxWhenItsDescriptionCannotBeStored(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.cluster.variableWriteErr = &httpError{status: 403, msg: "Permission denied"}

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "4")
	require.NotEqual(t, ExitOK, rc)
	out := ta.errOut.String()
	require.Contains(t, out, "description")
	require.Contains(t, out, "removed again")
	require.Contains(t, out, "nomad/jobs/box-*", "a denied variable write says which rule is missing")

	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Empty(t, ta.cluster.jobs, "the job was deleted again")
	require.Empty(t, ta.cluster.volumes, "and so was the disk")
}

// TestCreateRefusesAJobTooTallForAVariable: the size is checked before the disk
// is created, so a job Nomad cannot store leaves nothing behind.
func TestCreateRefusesAJobTooTallForAVariable(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	// One template per item, each about 250 bytes of template text: 400 of them
	// are four times what a Nomad variable holds.
	items := map[string]string{}
	for i := 0; i < 400; i++ {
		items["opencode/file-"+strings.Repeat("x", 4)+"-"+string(rune('a'+i%26))+itoa(i)+".json.tmpl"] =
			`{{ with secret "kv/data/box/llm" }}{{ .Data.data.key }}{{ end }}` + strings.Repeat(" ", 140)
	}
	ta.cluster.setExecutors(items)

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "4")
	require.NotEqual(t, ExitOK, rc)
	require.Contains(t, ta.errOut.String(), "the box was not created")
	require.Contains(t, ta.errOut.String(), "at most 65536")

	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Empty(t, ta.cluster.volumes, "nothing was created")
	require.Empty(t, ta.cluster.jobs)
}

// itoa is strconv.Itoa without pulling strconv into this file's imports for one
// call.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// TestStartAfterJobGC: the promise of `box stop` -- `box start` brings the box
// back on the same disk -- holds after Nomad has collected the job.
func TestStartAfterJobGC(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "2", "--memory-mib", "1024",
		"--disk-gib", "8"), ta.errOut.String())
	ta.out.Reset()
	ta.errOut.Reset()
	ta.stopAndCollect(t, "dev1")

	// ls: the box is there, stopped, with its disk.
	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "dev1")
	require.Contains(t, ta.out.String(), "stopped")
	require.NotContains(t, ta.out.String(), "orphan")
	require.Contains(t, ta.errOut.String(), "`box start dev1` runs it again")
	ta.out.Reset()
	ta.errOut.Reset()

	// status: what the box is, from the description.
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "state   stopped")
	require.Contains(t, ta.errOut.String(), "2 vCPU, 1024 MiB RAM, 8 GiB disk, image 1.0.0 (amd64)")
	require.Contains(t, ta.errOut.String(), "volume  box-dev1")
	ta.out.Reset()
	ta.errOut.Reset()

	// start: the stored job is registered again, with today's executor items.
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the box runs on its old disk")
	require.Contains(t, ta.errOut.String(), "running")
	job := ta.job(t, "box-dev1")
	require.Equal(t, "true", job.Meta["box"])
	require.Equal(t, 1024, job.TaskGroups[0].Tasks[0].Config["memory_mib"])
	require.Greater(t, callCount(ta, "RegisterJob box-dev1"), 1)
}

// TestOrphanDisk: a disk with no job and no description is an orphan. It is
// listed, it cannot be started (there is nothing to start from), and rm deletes
// it.
func TestOrphanDisk(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.stopAndCollect(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())

	// A disk made before boxes stored a description: a job, a volume, no variable.
	ta.addBox(t, "old")
	ta.stopAndCollect(t, "old")
	ta.out.Reset()
	ta.errOut.Reset()

	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "old")
	require.Contains(t, ta.out.String(), "orphan")
	require.Contains(t, ta.errOut.String(), "`box start old` cannot bring it back")
	ta.out.Reset()
	ta.errOut.Reset()

	require.Equal(t, ExitOK, ta.run(t, "status", "old"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "state   orphan")
	require.Contains(t, ta.errOut.String(), "nothing describes this disk")
	ta.errOut.Reset()

	rc := ta.run(t, "start", "old")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "is an orphan")
	require.Contains(t, ta.errOut.String(), "box rm old --yes")
	require.Empty(t, ta.cluster.jobs, "a start of an orphan registers nothing")
	ta.errOut.Reset()

	// rm works on it: it deletes the disk, and there is no description to delete.
	require.Equal(t, ExitOK, ta.run(t, "rm", "old", "--yes"), ta.errOut.String())
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Empty(t, ta.cluster.volumes)
	_, hasVariable := ta.cluster.variables[BoxSpecVariable("old")]
	require.False(t, hasVariable)
}

// TestRemoveDeletesTheDescription: rm is the end of both records of a box.
func TestRemoveDeletesTheDescription(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512",
		"--disk-gib", "4"), ta.errOut.String())
	ta.stopAndCollect(t, "dev1")

	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "deleted the description "+BoxSpecVariable("dev1"))
	ta.cluster.mu.Lock()
	require.Empty(t, ta.cluster.volumes, "the disk is deleted")
	_, hasVariable := ta.cluster.variables[BoxSpecVariable("dev1")]
	ta.cluster.mu.Unlock()
	require.False(t, hasVariable, "and so is the description")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls"))
	require.NotContains(t, ta.out.String(), "dev1")
}

// TestStoppedBoxCommandsSayHowToStartIt: every command that needs a job says
// that the box is stopped, and how to bring it back.
func TestStoppedBoxCommandsSayHowToStartIt(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512",
		"--disk-gib", "4"), ta.errOut.String())
	ta.stopAndCollect(t, "dev1")

	require.NotEqual(t, ExitOK, ta.run(t, "resize", "dev1", "--cpus", "2"))
	require.Contains(t, ta.errOut.String(), "the box dev1 is stopped")
	require.Contains(t, ta.errOut.String(), "`box start dev1` runs it again")
	ta.errOut.Reset()

	require.NotEqual(t, ExitOK, ta.run(t, "upgrade", "dev1"))
	require.Contains(t, ta.errOut.String(), "the box dev1 is stopped")
	ta.errOut.Reset()

	require.NotEqual(t, ExitOK, ta.run(t, "run", "dev1", "--", "true"))
	require.Contains(t, ta.errOut.String(), "the box dev1 is stopped")
	require.Contains(t, ta.errOut.String(), "box start dev1")
	ta.errOut.Reset()

	// A stop of a stopped box says so and changes nothing.
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "already stopped")
}

// TestStartWritesTheDescriptionOfARunningBox: a box made before descriptions were
// stored gets one from a start, which is the only command that is safe on a
// running box.
func TestStartWritesTheDescriptionOfARunningBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.mu.Lock()
	_, before := ta.cluster.variables[BoxSpecVariable("dev1")]
	ta.cluster.mu.Unlock()
	require.False(t, before)

	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "already running")
	_, text := ta.storedSpec(t, "dev1")
	require.Contains(t, text, "box-dev1")
}

// TestStartOfAStoppedBoxRefreshesTheExecutorItems: a start is the moment to pick
// up credentials staged since the box was created, and the refreshed job is what
// is stored.
func TestStartOfAStoppedBoxRefreshesTheExecutorItems(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512",
		"--disk-gib", "4"), ta.errOut.String())
	ta.stopAndCollect(t, "dev1")

	ta.cluster.setExecutors(map[string]string{"opencode/opencode.json": `{"permission":"never"}`})
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	job := ta.job(t, "box-dev1")
	require.Len(t, job.TaskGroups[0].Tasks[0].Templates, 1, "the new credential reaches the job")
	_, text := ta.storedSpec(t, "dev1")
	require.Contains(t, text, "opencode/opencode.json", "and the stored description has it too")
}

// TestCapacityCountsAStoppedBox: a listed disk is a counted box, and the count
// says which boxes are stopped.
func TestCapacityCountsAStoppedBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512",
		"--disk-gib", "4"), ta.errOut.String())
	// The driver reports the disk of the box on its node.
	ta.cluster.mu.Lock()
	ta.cluster.addNodeDiskLocked("node-1", "dev1", 300<<20, int64(4)<<30)
	ta.cluster.mu.Unlock()
	ta.stopAndCollect(t, "dev1")

	require.Equal(t, ExitOK, ta.run(t, "capacity"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "1 (1 stopped)", "the node counts the box whose disk it holds")
	require.Contains(t, ta.errOut.String(), "dev1 is stopped", "and the disk line says the same")
}

// TestCapacityAfterRemove: nothing of a deleted box is left to count.
func TestCapacityAfterRemove(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512",
		"--disk-gib", "4"), ta.errOut.String())
	require.Equal(t, ExitOK, ta.run(t, "rm", "dev1", "--yes"), ta.errOut.String())
	ta.out.Reset()
	ta.errOut.Reset()

	// Nomad's node keeps the driver's attributes key by key, so a deleted box's
	// keys stay in node.Attributes; the driver's own fingerprint is what counts.
	ta.cluster.mu.Lock()
	node := ta.cluster.nodes["node-1"]
	live := map[string]string{}
	for key, value := range node.Attributes {
		if strings.HasPrefix(key, boxAttrPrefix) && !strings.Contains(key, ".dev1.") {
			live[key] = value
		}
	}
	live[boxDiskAttrPrefix+"used_bytes"] = "0"
	live[boxDiskAttrPrefix+"virtual_bytes"] = "0"
	node.Drivers[boxDriverName].Attributes = live
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "capacity"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "dev1")
	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	require.NotContains(t, ta.out.String(), "dev1")
}
