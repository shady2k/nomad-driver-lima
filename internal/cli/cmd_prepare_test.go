package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

const (
	prepReady   = `{"state":"ready","step":"recipe","source":"git","dest":"/work/nocx","recipe":".box/prepare.sh"}`
	prepRunning = `{"state":"preparing","step":"clone","dest":"/work/nocx","last_line":"Receiving objects:  40%"}`
	prepFailed  = `{"state":"failed","step":"clone","dest":"/work/nocx","error":"git clone https://example.com/org/nocx.git: exit 128: fatal: early EOF","tail":["Receiving objects:  40%","fatal: early EOF"]}`
)

// TestCreateWithARepoWaitsUntilTheBoxIsReady: create clones the repository
// through boxd and returns when the preparation has ended.
func TestCreateWithARepoWaitsUntilTheBoxIsReady(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.guest.responses["prepare start --dest /work/nocx --repo https://example.com/org/nocx.git --ref v1 --limit 3600"] = prepRunning
	ta.guest.responses["prepare wait --timeout 30"] = prepReady

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--repo", "https://example.com/org/nocx.git", "--ref", "v1")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "preparing box dev1: a clone of https://example.com/org/nocx.git in /work/nocx")
	require.Contains(t, ta.errOut.String(), "box dev1 is ready: sources in /work/nocx, .box/prepare.sh ran")
}

// TestPrepareFailureSaysWhyAndTheWaysOut: a failed preparation is exit 1 with
// its reason, its last lines and the commands that get out of it.
func TestPrepareFailureSaysWhyAndTheWaysOut(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["prepare start --dest /work/nocx --repo https://example.com/org/nocx.git --limit 3600"] = prepRunning
	ta.guest.responses["prepare wait --timeout 30"] = prepFailed

	rc := ta.run(t, "prepare", "dev1", "--repo", "https://example.com/org/nocx.git")
	require.Equal(t, ExitError, rc)
	msg := ta.errOut.String()
	require.Contains(t, msg, "failed in the clone step")
	require.Contains(t, msg, "fatal: early EOF")
	require.Contains(t, msg, "`box prepare dev1` runs it again")
	require.Contains(t, msg, "`box prepare dev1 --skip`")
}

// TestPrepareWithoutArgumentsTakesTheWayOutOfEachState: waiting, running a
// failed one again, and plain words for a box never prepared.
func TestPrepareWithoutArgumentsTakesTheWayOutOfEachState(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["prepare get"] = prepRunning
	ta.guest.responses["prepare wait --timeout 30"] = prepReady
	require.Equal(t, ExitOK, ta.run(t, "prepare", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "box dev1 is ready")

	ta.errOut.Reset()
	ta.guest.responses["prepare get"] = prepFailed
	ta.guest.responses["prepare start --limit 3600"] = prepRunning
	require.Equal(t, ExitOK, ta.run(t, "prepare", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the recipe again")

	ta.errOut.Reset()
	ta.guest.responses["prepare get"] = `{"state":"none"}`
	require.Equal(t, ExitUsage, ta.run(t, "prepare", "dev1"))
	require.Contains(t, ta.errOut.String(), "--repo URL or --from PATH")

	ta.errOut.Reset()
	ta.guest.responses["prepare cancel"] = `{"state":"failed","error":"cancelled"}`
	require.Equal(t, ExitOK, ta.run(t, "prepare", "dev1", "--cancel"))
	require.Contains(t, ta.errOut.String(), "was cancelled")

	ta.errOut.Reset()
	ta.guest.responses["prepare skip"] = `{"state":"skipped"}`
	require.Equal(t, ExitOK, ta.run(t, "prepare", "dev1", "--skip"))
	require.Contains(t, ta.errOut.String(), "without a preparation")
}

// TestPrepareFromFilesSendsATar: --from sends the directory's contents as a
// tar stream, no git needed.
func TestPrepareFromFilesSendsATar(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	src := filepath.Join(t.TempDir(), "my proj")
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".box"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644))
	ta.guest.responses["prepare start --dest /work/my-proj --files --limit 3600"] = prepRunning
	ta.guest.responses["prepare wait --timeout 30"] = prepReady

	require.Equal(t, ExitOK, ta.run(t, "prepare", "dev1", "--from", src), ta.errOut.String())
	sent := ta.guest.stdin[len(ta.guest.stdin)-2]
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader([]byte(sent)))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names[h.Name] = true
	}
	require.True(t, names["./main.go"], "names = %v", names)
	require.True(t, names["./.box/"], "names = %v", names)
}

// TestPrepareFlagsThatDoNotGoTogether are refused before anything happens.
func TestPrepareFlagsThatDoNotGoTogether(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	for _, args := range [][]string{
		{"prepare", "dev1", "--repo", "x", "--from", "."},
		{"prepare", "dev1", "--ref", "main"},
		{"prepare", "dev1", "--repo", "x", "--dest", "relative"},
		{"prepare", "dev1", "--cancel", "--skip"},
		{"create", "dev1", "--image", "1.0.0", "--ref", "main"},
	} {
		ta.errOut.Reset()
		require.Equal(t, ExitUsage, ta.run(t, args...), "%v: %s", args, ta.errOut.String())
	}
	require.Empty(t, ta.cluster.jobs)
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/shady2k/nocx":      "nocx",
		"https://github.com/shady2k/nocx.git/": "nocx",
		"git@github.com:shady2k/nocx.git":      "nocx",
		"file:///srv/repos/my repo.git":        "my repo",
	} {
		require.Equal(t, want, repoName(in), in)
	}
	require.Equal(t, "my-repo", safeName("my repo"))
	require.Equal(t, "src", safeName(".."))
}
