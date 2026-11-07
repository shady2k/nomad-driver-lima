package cli

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// A project is the label that tells an agent which boxes are its own: it is
// shown by `box ls` and `box capacity`, carried in the job's meta so a resize, an
// upgrade and a start keep it, and it decides nothing -- it is not a protection.

// addProjectBox registers a running box that carries a project label.
func (ta *testApp) addProjectBox(t *testing.T, id, project string) {
	t.Helper()
	spec := goodSpec()
	spec.ID = id
	spec.Project = project
	ta.addBoxSpec(t, spec)
}

// withEnv makes the App read the given environment, and nothing else.
func (ta *testApp) withEnv(values map[string]string) {
	ta.Getenv = func(key string) string { return values[key] }
}

// withRepoName makes the App see every directory as the given git repository.
func (ta *testApp) withRepoName(name string) {
	ta.repoName = func(string) string { return name }
}

func TestSanitizeProject(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"my-project", "my-project"},
		{"My Project", "my-project"},
		{"  My--Project_2  ", "my-project-2"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"nomad-driver-lima", "nomad-driver-lima"},
		{"!!!", ""},
		{"", ""},
	} {
		require.Equal(t, tc.want, sanitizeProject(tc.in), "sanitizeProject(%q)", tc.in)
	}
}

// TestProjectLabelSources is where a box's project comes from: --project, then
// BOX_PROJECT, then the name of the git repository of the directory.
func TestProjectLabelSources(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.withEnv(map[string]string{"BOX_PROJECT": "from-env"})
	ta.withRepoName("From Repo")

	label, err := ta.ProjectLabel("from-flag", true)
	require.NoError(t, err)
	require.Equal(t, "from-flag", label, "the flag is the caller's own word")

	label, err = ta.ProjectLabel("", false)
	require.NoError(t, err)
	require.Equal(t, "from-env", label, "the variable comes before the directory")

	ta.withEnv(nil)
	label, err = ta.ProjectLabel("", false)
	require.NoError(t, err)
	require.Equal(t, "from-repo", label, "a directory name is sanitized, never refused")

	ta.withRepoName("")
	label, err = ta.ProjectLabel("", false)
	require.NoError(t, err)
	require.Equal(t, "", label, "no flag, no variable and no repository: no project")
}

// TestProjectLabelRefusedInASentence: a label a person gives is refused when it
// is not usable, because a label that does not say what was meant is worse than
// no label.
func TestProjectLabelRefusedInASentence(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	_, err := ta.ProjectLabel("My_Project", true)
	require.Error(t, err)
	require.Equal(t, ExitUsage, ExitCode(err))
	require.Contains(t, err.Error(), `--project "My_Project" is not usable`)
	require.Contains(t, err.Error(), "lower-case letters, digits and dashes")

	for _, bad := range []string{"My_Project", "-lead", "trail-", "double--dash", "with space"} {
		_, err := ta.ProjectLabel(bad, true)
		require.Error(t, err, "%q is refused", bad)
	}

	ta.withEnv(map[string]string{"BOX_PROJECT": "Bad_Env"})
	_, err = ta.ProjectLabel("", false)
	require.Error(t, err)
	require.Equal(t, ExitUsage, ExitCode(err))
	require.Contains(t, err.Error(), `BOX_PROJECT is "Bad_Env", which is not usable`)
}

// TestGitRepoNameOfAWorktree: a linked worktree belongs to the repository it was
// made from, so both are one project.
func TestGitRepoNameOfAWorktree(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	env := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME=" + root, "GIT_TERMINAL_PROMPT=0"}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = root + "/repo"
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
	}
	require.NoError(t, exec.Command(git, "init", "-q", root+"/repo").Run())
	run("config", "user.email", "t@example.test")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "one")
	run("worktree", "add", "-q", root+"/wt", "-b", "side")

	require.Equal(t, "repo", gitRepoName(root+"/repo"))
	require.Equal(t, "repo", gitRepoName(root+"/wt"), "a worktree is not a second project")
	require.Equal(t, "", gitRepoName(t.TempDir()), "a directory in no repository has no project")
}

// TestCreateCarriesTheProject: --project is the label of the box, in the job's
// meta and so in its stored description.
func TestCreateCarriesTheProject(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--project", "my-project")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	job := ta.cluster.jobs["box-dev1"]
	require.Equal(t, "my-project", job.Meta["project"])
	require.Equal(t, "box-dev1", *job.ID)
	require.Equal(t, "my-project/box-dev1", *job.Name, "the jobs page of Nomad shows the name")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--json"))
	var doc lsOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Boxes, 1)
	require.Equal(t, "my-project", doc.Boxes[0].Project)
}

// TestCreateProjectFromTheEnvironmentAndTheDirectory: without the flag the
// variable decides, and without either the repository of the directory does.
func TestCreateProjectFromTheEnvironmentAndTheDirectory(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		ta := newTestApp(t, indexJSON)
		ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
		ta.withEnv(map[string]string{"BOX_PROJECT": "env-project"})
		ta.withRepoName("from-repo")
		require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
		require.Equal(t, "env-project", ta.cluster.jobs["box-dev1"].Meta["project"],
			"BOX_PROJECT outranks the repository name")
	})

	t.Run("directory", func(t *testing.T) {
		ta := newTestApp(t, indexJSON)
		ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
		ta.withRepoName("Nomad Driver Lima")
		require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
		require.Equal(t, "nomad-driver-lima", ta.cluster.jobs["box-dev1"].Meta["project"],
			"a directory name is sanitized into a label")
	})

	t.Run("none", func(t *testing.T) {
		ta := newTestApp(t, indexJSON)
		ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
		require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
		_, labelled := ta.cluster.jobs["box-dev1"].Meta["project"]
		require.False(t, labelled, "a box with no project carries no label at all")

		require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
		fields := strings.Fields(strings.Split(strings.TrimSpace(ta.out.String()), "\n")[1])
		require.Equal(t, "-", fields[1], "the project column of a box with no label: %q", ta.out.String())
	})
}

// TestCreateRefusesAnUnusableProject: a project is refused before anything is
// created, and the sentence says what a usable one looks like.
func TestCreateRefusesAnUnusableProject(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--project", "My_Project")
	require.Equal(t, ExitUsage, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "is not usable")
	require.Empty(t, ta.cluster.jobs, "nothing was created")

	ta = newTestApp(t, indexJSON)
	ta.withEnv(map[string]string{"BOX_PROJECT": "Bad_Project"})
	rc = ta.run(t, "create", "dev1", "--image", "1.0.0")
	require.Equal(t, ExitUsage, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "BOX_PROJECT is \"Bad_Project\"")
	require.Contains(t, ta.errOut.String(), "or unset it")
	require.Empty(t, ta.cluster.jobs)
}

// TestProjectSurvivesResizeStartAndUpgrade: the label is in the job's meta, and
// every command that registers the job again writes it back.
func TestProjectSurvivesResizeStartAndUpgrade(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":0}`
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0", "--project", "my-project",
		"--platform", "1.9.0"), ta.errOut.String())
	require.Equal(t, "my-project", ta.cluster.jobs["box-dev1"].Meta["project"])

	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--cpus", "4"), ta.errOut.String())
	require.Equal(t, "my-project", ta.cluster.jobs["box-dev1"].Meta["project"], "a resize keeps the project")

	require.Equal(t, ExitOK, ta.run(t, "upgrade", "dev1"), ta.errOut.String())
	require.Equal(t, "my-project", ta.cluster.jobs["box-dev1"].Meta["project"], "an upgrade keeps the project")

	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"), ta.errOut.String())
	ta.cluster.nomadGC()
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	require.Equal(t, "my-project", ta.cluster.jobs["box-dev1"].Meta["project"], "a start keeps the project")
}

// TestListShowsProjectsAndFiltersThem: the PROJECT column, --project and --mine.
func TestListShowsProjectsAndFiltersThem(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addProjectBox(t, "mine1", "my-project")
	ta.addProjectBox(t, "other1", "other-project")
	ta.addBox(t, "plain") // no label at all

	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	table := ta.out.String()
	require.Contains(t, table, "PROJECT")
	row := func(id string) string {
		for _, line := range strings.Split(table, "\n") {
			if strings.HasPrefix(line, id+" ") {
				return line
			}
		}
		return ""
	}
	require.Contains(t, row("mine1"), "my-project")
	require.Contains(t, row("other1"), "other-project")
	require.Contains(t, row("plain"), "-")
	require.Len(t, strings.Split(strings.TrimSpace(table), "\n"), 4, "the header and three boxes")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--project", "my-project"), ta.errOut.String())
	table = ta.out.String()
	require.Contains(t, table, "mine1")
	require.NotContains(t, table, "other1", "another project's boxes are not this project's")
	require.NotContains(t, table, "plain", "a box with no label belongs to no project")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--json", "--project", "other-project"), ta.errOut.String())
	var doc lsOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Boxes, 1)
	require.Equal(t, "other1", doc.Boxes[0].ID)
	require.Equal(t, "other-project", doc.Boxes[0].Project)

	// --mine is the project this directory resolves to.
	ta.withEnv(map[string]string{"BOX_PROJECT": "my-project"})
	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--mine"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "mine1")
	require.NotContains(t, ta.out.String(), "other1")

	// A project with no box is an empty answer, not a failure.
	ta.JSON = false
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--project", "nothing-here"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no boxes of the project nothing-here")
}

// TestListMineWithoutAProject: --mine needs a project to resolve, and says so.
func TestListMineWithoutAProject(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitUsage, ta.run(t, "ls", "--mine"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "this directory belongs to no project")
	require.Contains(t, ta.errOut.String(), "--project NAME")

	require.Equal(t, ExitUsage, ta.run(t, "ls", "--mine", "--project", "x"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "give one of them")
}

// TestCapacityShowsTheProjectOfEachBox: the nodes are shared, so whose box holds
// a disk is part of the picture.
func TestCapacityShowsTheProjectOfEachBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addProjectBox(t, "dev1", "my-project")

	require.Equal(t, ExitOK, ta.run(t, "capacity"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "node-1/dev1: 300 MiB used of 8.0 GiB (dev1 is running, project my-project)")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "--json", "capacity"))
	var doc capacityOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Nodes[0].Disks, 1)
	require.Equal(t, "my-project", doc.Nodes[0].Disks[0].Project)
}

// TestCreateSaysABoxCanBeStopped: the line after a box is created, in the moment
// its owner decides how long to keep it awake.
func TestCreateSaysABoxCanBeStopped(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "a stopped box holds no CPU and no memory")
	require.Contains(t, out, "`box stop dev1` gives them back")
	require.Contains(t, out, "`box start dev1` brings it back with its disk and its sessions")
}

// TestWaitSaysTheBoxCanBeStopped: after a turn that finished, with no other turn
// running in the box, the wait says what stopping the box costs -- nothing.
func TestWaitSaysTheBoxCanBeStopped(t *testing.T) {
	line := "box stop dev1 if you do not need this box for a while: its disk and sessions are kept"

	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"ok"}`
	ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":1,"activity":{"busy":false,"idle_seconds":90}}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), line)
	require.Equal(t, 1, strings.Count(ta.errOut.String(), "box stop dev1"), "one line: %q", ta.errOut.String())

	// A turn still running in another session of the box: it is at work.
	ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":2,"activity":{"busy":true,"idle_seconds":0}}`
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), line)

	// A boxd too old to report its activity says nothing about it.
	ta.guest.responses["info"] = `{"boxd_version":"0.1.17","sessions":1}`
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), line)

	// A guest that does not answer is not a box that is idle either.
	ta.guest.silent["info"] = true
	ta.GuestTimeout = 50 * time.Millisecond
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), line)
	// The hint changes no exit code, not even of a turn that ended in an error.
	ta.guest.silent["info"] = false
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"error","error":"boom"}`
	require.Equal(t, ExitError, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
}

// TestCreateRefusalNamesTheIdleBoxes: a refusal for lack of room names the idle
// boxes of the caller's own project, which it may stop, and the others, which it
// may not.
func TestCreateRefusalNamesTheIdleBoxes(t *testing.T) {
	newApp := func(t *testing.T) *testApp {
		ta := newTestApp(t, indexJSON)
		ta.addProjectBox(t, "mine", "my-project")
		ta.addProjectBox(t, "theirs", "other-project")
		ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":0,"activity":{"busy":false,"idle_seconds":7200}}`
		ta.hogAllocation(20000) // node-1 has 32768 MiB, so 16384 MiB of RAM no longer fit
		return ta
	}

	ta := newApp(t)
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384", "--project", "my-project")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "box: no node can take this box")
	require.Contains(t, ta.errOut.String(), "your idle boxes: mine 2h (box stop <id>)")
	require.Contains(t, ta.errOut.String(), "other idle boxes: theirs 2h (not yours to stop)")
	_, created := ta.cluster.jobs["box-dev1"]
	require.False(t, created, "nothing was created")

	// With no project of its own, nothing is known to be the caller's to stop.
	ta = newApp(t)
	rc = ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "idle boxes: mine 2h, theirs 2h")
	require.NotContains(t, ta.errOut.String(), "not yours to stop")

	// A box with a turn running is not idle, and a guest that did not answer is
	// not named either.
	ta = newApp(t)
	ta.guest.responses["info"] = `{"boxd_version":"0.5.0","sessions":1,"activity":{"busy":true,"idle_seconds":0}}`
	rc = ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384", "--project", "my-project")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "idle boxes")

	// The wait says it too, at the refusal that starts it.
	ta = newApp(t)
	ta.Sleep = func(ctx context.Context, d time.Duration) error {
		ta.clock.advance(d)
		return nil
	}
	rc = ta.run(t, "create", "dev1", "--image", "1.0.0", "--memory-mib", "16384", "--project", "my-project",
		"--wait", "1m")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no room for box dev1 yet")
	require.Contains(t, ta.errOut.String(), "your idle boxes: mine 2h (box stop <id>)")
}

// TestRebuildBoxCarriesTheProject: the temporary box of a rebuild is a box
// created here, so it carries the project the command resolves. A rebuild that
// fails keeps that box, and it is then the caller's own to find.
func TestRebuildBoxCarriesTheProject(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.withEnv(map[string]string{"BOX_PROJECT": "my-project"})
	ta.withDraft(t, "project-2", "project", "1.0.0", 8)
	ta.finishWorkJobs(t, map[string]string{"kind": "bake", "version": "project-3", "arch": "amd64",
		"name": "project", "parent": "1.0.0", "sha256": strings.Repeat("ef", 32), "size": "2048"}, "")
	// The temporary box is deleted when the rebuild ends, so its label is read
	// when it is registered.
	var label string
	inner := ta.cluster.onRegister
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		if derefStr(job.ID, "") == "box-project-rebuild" {
			label = job.Meta["project"]
		}
		inner(c, job)
	}

	require.Equal(t, ExitOK, ta.run(t, "bake", "--rebuild", "project-2", "--base", "1.0.0"), ta.errOut.String())
	require.Equal(t, "my-project", label)
}
