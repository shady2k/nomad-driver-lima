package boxd

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitRepo makes a local repository with the files given, committed.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		require(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require(t, os.WriteFile(p, []byte(body), 0o755))
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// prepConfig is a test config whose journal lines are kept.
func prepConfig(t *testing.T) (DaemonConfig, func() string) {
	cfg := testConfig(t)
	var mu sync.Mutex
	var lines []string
	cfg.Logf = func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	require(t, os.MkdirAll(cfg.StateDir, 0o700))
	return cfg.withDefaults(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func waitDone(t *testing.T, p *preparer) PrepareState {
	t.Helper()
	st := p.Wait(context.Background(), 30*time.Second)
	if st.State == PrepareRunning {
		t.Fatalf("the preparation did not end: %+v", st)
	}
	return st
}

// TestPrepareClonesAndRunsTheRecipe: the sources are cloned, the recipe runs
// in them, its output is in the journal, and sessions start there.
func TestPrepareClonesAndRunsTheRecipe(t *testing.T) {
	cfg, journal := prepConfig(t)
	repo := gitRepo(t, map[string]string{"README": "hi\n", ".box/prepare.sh": "echo toolchain ready in $BOX_WORKSPACE\n"})
	p := newPreparer(cfg)
	dest := filepath.Join(t.TempDir(), "work", "nocx")
	if _, err := p.Start(PrepareRequest{Repo: repo, Dest: dest}); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, p)
	if st.State != PrepareReady || st.Recipe != prepareRecipe {
		t.Fatalf("state = %+v", st)
	}
	if !fileExists(filepath.Join(dest, "README")) {
		t.Fatal("the clone is not at the destination")
	}
	if !strings.Contains(journal(), "prepare recipe: toolchain ready in "+dest) || !strings.Contains(journal(), "prepare: ready") {
		t.Fatalf("journal = %s", journal())
	}
	if p.workdir() != dest || p.sessionGate() != nil {
		t.Fatalf("workdir = %q, gate = %v", p.workdir(), p.sessionGate())
	}
}

// TestPrepareFailureBlocksSessionsUntilRetriedOrSkipped: a failed recipe says
// why, sessions refuse to start with the ways out, and both ways work.
func TestPrepareFailureBlocksSessionsUntilRetriedOrSkipped(t *testing.T) {
	cfg, _ := prepConfig(t)
	repo := gitRepo(t, map[string]string{".box/prepare.sh": "echo installing go\nexit 3\n"})
	m := newManager(t, cfg)
	dest := filepath.Join(t.TempDir(), "src")
	if _, err := m.Prepare().Start(PrepareRequest{Repo: repo, Dest: dest}); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, m.Prepare())
	if st.State != PrepareFailed || !strings.Contains(st.Error, "exit 3: installing go") {
		t.Fatalf("state = %+v", st)
	}
	_, err := m.NewSession("prime", "fake/model-1", "", "t")
	if err == nil || !strings.Contains(err.Error(), "preparation failed") || !strings.Contains(err.Error(), "--skip") {
		t.Fatalf("session new = %v", err)
	}

	// The recipe is fixed in place and runs again on the same sources.
	require(t, os.WriteFile(filepath.Join(dest, prepareRecipe), []byte("echo fixed\n"), 0o755))
	if _, err := m.Prepare().Start(PrepareRequest{}); err != nil {
		t.Fatal(err)
	}
	if st := waitDone(t, m.Prepare()); st.State != PrepareReady {
		t.Fatalf("again = %+v", st)
	}

	// Skip is the other way out.
	require(t, os.WriteFile(filepath.Join(dest, prepareRecipe), []byte("exit 1\n"), 0o755))
	_, _ = m.Prepare().Start(PrepareRequest{})
	waitDone(t, m.Prepare())
	if _, err := m.Prepare().Skip(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NewSession("prime", "fake/model-1", "", "t"); err != nil {
		t.Fatalf("after skip: %v", err)
	}
}

// TestPrepareCancelAndTimeLimitEndIt: a running preparation always ends, by
// cancel at once and by its time limit, and what it started goes with it.
func TestPrepareCancelAndTimeLimitEndIt(t *testing.T) {
	cfg, _ := prepConfig(t)
	marker := filepath.Join(t.TempDir(), "still-running")
	repo := gitRepo(t, map[string]string{".box/prepare.sh": "(sleep 3; touch " + marker + ") &\nsleep 60\n"})
	p := newPreparer(cfg)

	if _, err := p.Start(PrepareRequest{Repo: repo, Dest: filepath.Join(t.TempDir(), "a")}); err != nil {
		t.Fatal(err)
	}
	for p.State().Step != "recipe" {
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if st, _ := p.Cancel(); st.State != PrepareFailed || st.Error != "cancelled" {
		t.Fatalf("cancel = %+v", st)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancel took %s", time.Since(start))
	}
	if _, err := p.Cancel(); err == nil {
		t.Fatal("a second cancel has nothing to stop and says so")
	}

	if _, err := p.Start(PrepareRequest{Repo: repo, Dest: filepath.Join(t.TempDir(), "b"), LimitSec: 1}); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, p)
	if st.State != PrepareFailed || !strings.Contains(st.Error, "time limit") {
		t.Fatalf("limit = %+v", st)
	}
	time.Sleep(3500 * time.Millisecond)
	if fileExists(marker) {
		t.Fatal("a process the recipe started outlived the preparation")
	}
}

// TestPrepareThatBoxdDidNotSeeEndIsFailed: after a restart nothing is left to
// end a preparation, so it is failed, with the way out.
func TestPrepareThatBoxdDidNotSeeEndIsFailed(t *testing.T) {
	cfg, _ := prepConfig(t)
	require(t, os.WriteFile(filepath.Join(cfg.StateDir, "prepare.json"), []byte(`{"state":"preparing","step":"clone"}`), 0o600))
	st := newPreparer(cfg).State()
	if st.State != PrepareFailed || !strings.Contains(st.Error, "restarted during the clone step") {
		t.Fatalf("state = %+v", st)
	}
}

// TestPrepareFromFiles: sources without git arrive as a tar stream; a name
// that leaves the destination refuses the stream.
func TestPrepareFromFiles(t *testing.T) {
	cfg, _ := prepConfig(t)
	p := newPreparer(cfg)
	dest := filepath.Join(t.TempDir(), "proj")
	st, err := p.StartFiles(PrepareRequest{Dest: dest}, tarOf(t, map[string]string{"main.go": "package main\n", ".box/prepare.sh": "test -f main.go\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if st = waitDone(t, p); st.State != PrepareReady || st.Source != "files" {
		t.Fatalf("state = %+v", st)
	}

	bad := filepath.Join(t.TempDir(), "bad")
	if _, err := p.StartFiles(PrepareRequest{Dest: bad}, tarOf(t, map[string]string{"../escape": "x"})); err != nil {
		t.Fatal(err)
	}
	if st := waitDone(t, p); st.State != PrepareFailed || !strings.Contains(st.Error, "outside the destination") {
		t.Fatalf("traversal = %+v", st)
	}
	if dirExists(bad) {
		t.Fatal("a refused stream left its files behind")
	}
}

// TestPrepareKeepsExistingSources: a destination that exists is never written
// over, and a second preparation while one runs is refused.
func TestPrepareKeepsExistingSources(t *testing.T) {
	cfg, _ := prepConfig(t)
	p := newPreparer(cfg)
	dest := t.TempDir()
	if _, err := p.Start(PrepareRequest{Repo: "https://example.com/x.git", Dest: dest}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing dest = %v", err)
	}
	if _, err := p.Start(PrepareRequest{Repo: "x", Dest: "relative/path"}); err == nil {
		t.Fatal("a relative destination is refused")
	}
	repo := gitRepo(t, map[string]string{".box/prepare.sh": "sleep 60\n"})
	if _, err := p.Start(PrepareRequest{Repo: repo, Dest: filepath.Join(t.TempDir(), "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(PrepareRequest{Repo: repo, Dest: filepath.Join(t.TempDir(), "b")}); CodeOf(err) != CodeBusy {
		t.Fatalf("second = %v", err)
	}
	if err := p.sessionGate(); CodeOf(err) != CodeBusy || !strings.Contains(err.Error(), "--cancel") {
		t.Fatalf("gate = %v", err)
	}
	_, _ = p.Cancel()
}

// TestPrepareCloneFailureSaysWhy: a clone that fails leaves no half checkout
// and names git's own reason.
func TestPrepareCloneFailureSaysWhy(t *testing.T) {
	cfg, _ := prepConfig(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	p := newPreparer(cfg)
	dest := filepath.Join(t.TempDir(), "x")
	if _, err := p.Start(PrepareRequest{Repo: filepath.Join(t.TempDir(), "no-such-repo"), Dest: dest}); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, p)
	if st.State != PrepareFailed || !strings.Contains(st.Error, "git clone") || !strings.Contains(st.Error, "exit 128") {
		t.Fatalf("state = %+v", st)
	}
	if dirExists(dest) || dirExists(dest+".partial") {
		t.Fatal("a failed clone left files")
	}
}

func tarOf(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range files {
		require(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require(t, err)
	}
	require(t, tw.Close())
	return &b
}
