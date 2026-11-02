package boxd

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeBinPath is the fake ACP agent used as the "prime" executor in tests.
var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
	repoRoot string
)

// TestMain removes the fake executor binary after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

// buildFake compiles internal/boxd/fakeacp once per test binary.
func buildFake(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		repoRoot, fakeErr = moduleRoot()
		if fakeErr != nil {
			return
		}
		dir, err := os.MkdirTemp("", "boxd-fake-")
		if err != nil {
			fakeErr = err
			return
		}
		fakeDir = dir
		out := filepath.Join(dir, "fakeacp")
		cmd := exec.Command("go", "build", "-o", out, "./internal/boxd/fakeacp")
		cmd.Dir = repoRoot
		if b, err := cmd.CombinedOutput(); err != nil {
			fakeErr = &buildError{msg: string(b), err: err}
			return
		}
		fakePath = out
	})
	if fakeErr != nil {
		t.Fatalf("build fake executor: %v", fakeErr)
	}
	return fakePath
}

type buildError struct {
	msg string
	err error
}

func (b *buildError) Error() string { return b.err.Error() + "\n" + b.msg }

// moduleRoot finds the repository root from the test's working directory.
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", os.ErrNotExist
}

// testConfig is a daemon config pointed at a temp state dir, a temp runtime
// directory (a real box uses /run/boxd, a tmpfs) and the fake agent.
func testConfig(t *testing.T) DaemonConfig {
	t.Helper()
	dir := t.TempDir()
	return DaemonConfig{
		Socket:       filepath.Join(dir, "boxd.sock"),
		StateDir:     filepath.Join(dir, "state"),
		RuntimeDir:   filepath.Join(dir, "run"),
		SecretsDir:   filepath.Join(dir, "secrets"),
		LocalDir:     filepath.Join(dir, "local"),
		PrimeAgent:   buildFake(t),
		Journal:      io.Discard,
		Logf:         func(format string, args ...any) { t.Logf(format, args...) },
		CancelGrace:  400 * time.Millisecond,
		StartTimeout: 30 * time.Second,
	}
}

// newManager starts an in-process manager for tests.
func newManager(t *testing.T, cfg DaemonConfig) *Manager {
	t.Helper()
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

// mustSession creates a session and fails the test on error.
func mustSession(t *testing.T, m *Manager, executor, model string) Session {
	t.Helper()
	s, err := m.NewSession(executor, model, "", "test")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

// waitTurn waits for a turn and fails on error.
func waitTurn(t *testing.T, m *Manager, id string, turn int) Turn {
	t.Helper()
	tr, err := m.Wait(id, turn, 30*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return tr
}

// allEvents drains the event feed of a session.
func allEvents(t *testing.T, m *Manager, id string) []Event {
	t.Helper()
	var out []Event
	var since int64
	for {
		evs, next, err := m.Events(id, since, 200)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		out = append(out, evs...)
		if len(evs) == 0 {
			return out
		}
		since = next
	}
}

// mkdirAll is a test helper.
func mkdirAll(p string) error { return os.MkdirAll(p, 0o700) }

func kinds(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func hasKind(evs []Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
