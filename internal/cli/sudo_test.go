package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunExecutorRunsAsTheExecutorsSessions is `box run --executor`: the command
// goes in as boxd's user -- through `sudo -n` into `boxctl exec`, which builds
// the environment the executor's sessions get -- and the exit code is the
// command's, exactly as a plain box run's is.
func TestRunExecutorRunsAsTheExecutorsSessions(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")

	require.Equal(t, ExitOK, ta.run(t, "run", "dev1", "--executor", "prime", "--", "git", "-C", "/work/proj", "status"),
		ta.errOut.String())
	require.Equal(t, []string{"sudo", "-n", "true"}, ta.guest.calls[0], "root is asked for once, before the command")
	last := ta.guest.calls[len(ta.guest.calls)-1]
	require.Equal(t, []string{"setsid", "-w", "sh", "-c", runWrapper}, last[:5],
		"the command still runs in a session of its own")
	require.Equal(t, []string{"sudo", "-n", boxctlPath, "exec", "--executor", "prime", "--", "git", "-C", "/work/proj", "status"},
		last[6:])

	ta.guest.runExit = 42
	require.Equal(t, 42, ta.run(t, "run", "dev1", "--executor", "prime", "--", "false"),
		"the command's exit code is the run's")
}

// TestRunExecutorWithACwd: --cwd is the directory the command starts in, and it
// is only a run's own flag when the command runs as an executor's sessions do.
func TestRunExecutorWithACwd(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")

	require.Equal(t, ExitOK, ta.run(t, "run", "dev1", "--executor=prime", "--cwd", "/work/other", "--", "make"),
		ta.errOut.String())
	last := ta.guest.calls[len(ta.guest.calls)-1]
	require.Equal(t, []string{"sudo", "-n", boxctlPath, "exec", "--executor", "prime", "--cwd", "/work/other", "--", "make"},
		last[6:])

	require.Equal(t, ExitUsage, ta.run(t, "run", "dev1", "--cwd", "/work/other", "--", "make"))
	require.Contains(t, ta.errOut.String(), "--cwd needs --executor")

	ta.errOut.Reset()
	require.Equal(t, ExitUsage, ta.run(t, "run", "dev1", "--executor", "prime", "--cwd", "work", "--", "make"))
	require.Contains(t, ta.errOut.String(), "not an absolute path")
}

// TestRunUnknownExecutorIsAUsageError: the name is checked before the box is
// asked anything, and the message lists what the box runs.
func TestRunUnknownExecutorIsAUsageError(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")

	require.Equal(t, ExitUsage, ta.run(t, "run", "dev1", "--executor", "opancde", "--", "true"))
	require.Contains(t, ta.errOut.String(), `unknown executor "opancde"`)
	require.Contains(t, ta.errOut.String(), "prime, opencode, claude")
	require.Empty(t, ta.guest.calls, "an unknown executor costs no guest call")
}

// TestRunExecutorWithoutSudo: a box whose login user has no passwordless sudo
// says so instead of reporting a command that never ran.
func TestRunExecutorWithoutSudo(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.sudoOff = true

	rc := ta.run(t, "run", "dev1", "--executor", "prime", "--", "git", "status")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "no passwordless sudo")
	require.Contains(t, ta.errOut.String(), "a box run without --executor")
	require.Len(t, ta.guest.calls, 1, "the command is not run")
}

// TestSSHExecutorRunsTheShellAsTheExecutor: `box ssh --executor prime` is the
// same interactive shell, with the environment that executor's sessions get.
func TestSSHExecutorRunsTheShellAsTheExecutor(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.term.interactive = true

	require.Equal(t, ExitOK, ta.run(t, "ssh", "dev1", "--executor", "prime"), ta.errOut.String())
	last := ta.guest.calls[len(ta.guest.calls)-1]
	require.Equal(t, []string{"sudo", "-n", boxctlPath, "exec", "--executor", "prime", "--", "bash", "-l"}, last)

	ta.errOut.Reset()
	require.Equal(t, ExitUsage, ta.run(t, "ssh", "dev1", "--executor", "opancde"))
	require.Contains(t, ta.errOut.String(), `unknown executor "opancde"`)
	require.Equal(t, ExitUsage, ta.run(t, "ssh", "dev1", "--cwd", "/work"))
	require.Contains(t, ta.errOut.String(), "--cwd needs --executor")
}

// TestCopyIntoARootOwnedTarget: a target the box's login user cannot write --
// /work, which belongs to boxd's user -- is made and written as root, and what is
// written is given to the owner of the directory that was already there, not to
// the uid of the machine the copy started from.
func TestCopyIntoARootOwnedTarget(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.makeDir(t, "/work")
	ta.guest.owners["/work"] = "0:0"
	local := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(local, "a.txt"), []byte("alpha"), 0o644))

	require.Equal(t, ExitOK, ta.run(t, "cp", filepath.Join(local, "a.txt"), "dev1:/work/in/a.txt"), ta.errOut.String())
	require.Equal(t, "alpha", ta.guest.read(t, "/work/in/a.txt"))

	require.Equal(t, []string{"mkdir", "-p", "--", "/work/in"}, ta.guest.sudos[1], "the directory is made as root")
	require.Equal(t, []string{"tar", "-xf", "-", "--no-same-owner", "-C", "/work/in"}, ta.guest.sudos[2])
	require.Equal(t, []string{"chown", "-R", "0:0", "--", "/work/in", "/work/in/a.txt"}, ta.guest.sudos[3],
		"what was written belongs to the owner of the nearest directory that was there")
	require.Equal(t, "0:0", ta.guest.ownerOf("/work/in/a.txt"))
	require.Equal(t, []string{"true"}, ta.guest.sudos[0], "root is asked for once, first")
}

// TestCopyIntoTheAgentsOwnWork: what a copy writes below a directory the agent
// owns stays the agent's, and the copy is the login user's: no sudo is asked for.
func TestCopyIntoTheAgentsOwnWork(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.makeDir(t, "/work/proj")
	ta.guest.owners["/work"] = "0:0"
	ta.guest.owners["/work/proj"] = "1000:1000"
	local := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(local, "a.txt"), []byte("alpha"), 0o644))

	require.Equal(t, ExitOK, ta.run(t, "cp", filepath.Join(local, "a.txt"), "dev1:/work/proj/a.txt"), ta.errOut.String())
	require.Equal(t, "alpha", ta.guest.read(t, "/work/proj/a.txt"))
	// A directory that is not there yet is made by the same user: the nearest
	// directory that is there is the agent's.
	require.Equal(t, ExitOK, ta.run(t, "cp", filepath.Join(local, "a.txt"), "dev1:/work/proj/out/a.txt"), ta.errOut.String())
	require.Equal(t, "alpha", ta.guest.read(t, "/work/proj/out/a.txt"))
	require.Empty(t, ta.guest.sudos, "nothing was written as root")
}

// TestCopyOutOfAnUnreadableSource: a result the agent wrote below a root-owned
// directory is read through sudo -n, and the copy lands locally as before.
func TestCopyOutOfAnUnreadableSource(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.write(t, "/work/out/a.txt", "alpha")
	ta.guest.unreadable = []string{"/work/out"}
	local := t.TempDir()

	require.Equal(t, ExitOK, ta.run(t, "cp", "dev1:/work/out", local), ta.errOut.String())
	data, err := os.ReadFile(filepath.Join(local, "out", "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha", string(data))
	require.Equal(t, []string{"tar", "-cf", "-", "-C", "/work", "out"}, ta.guest.sudos[1],
		"the tree is read as root")
}

// TestCopyWithoutSudo: a copy that needs root in a box that has none says what is
// missing and what to do, and writes nothing.
func TestCopyWithoutSudo(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.makeDir(t, "/work")
	ta.guest.owners["/work"] = "0:0"
	ta.guest.sudoOff = true
	local := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(local, "a.txt"), []byte("alpha"), 0o644))

	rc := ta.run(t, "cp", filepath.Join(local, "a.txt"), "dev1:/work/in/a.txt")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "no passwordless sudo")
	require.Contains(t, ta.errOut.String(), "a directory it owns for box cp")
	require.NoFileExists(t, ta.guest.path("/work/in/a.txt"))
}
