package boxd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecPlanIsTheSessionEnvironment: `boxctl exec` starts a command in the
// environment an executor's session gets -- its HOME, its XDG_CONFIG_HOME, the
// bundle's bin and the directories its installer writes to in front of PATH --
// and in the box's workspace, which is where sessions start.
func TestExecPlanIsTheSessionEnvironment(t *testing.T) {
	cfg := testConfig(t)
	if _, err := NewManager(cfg); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	dirs := cfg.dirsOf("prime")
	for _, rel := range []string{".bun/bin", ".cargo/bin", ".deno/bin"} {
		if err := os.MkdirAll(filepath.Join(dirs.home, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "bun"))
	t.Setenv("PATH", bin+":/usr/bin:/bin")

	plan, err := execPlanFor(cfg, "prime", "", []string{"bun", "--version"})
	if err != nil {
		t.Fatalf("execPlanFor: %v", err)
	}
	if plan.path != filepath.Join(bin, "bun") || strings.Join(plan.argv, " ") != "bun --version" {
		t.Fatalf("the command is %q %v, want the bun on the executor's PATH", plan.path, plan.argv)
	}
	if got, want := plan.dir, filepath.Join(cfg.StateDir, "work"); got != want {
		t.Fatalf("working directory = %q, want the box's work directory %q", got, want)
	}
	sep := string(filepath.ListSeparator)
	wantPath := strings.Join([]string{
		cfg.platformBinDir(), bin, "/usr/bin", "/bin",
		filepath.Join(dirs.home, "go", "bin"),
		filepath.Join(dirs.home, ".local", "bin"),
		filepath.Join(dirs.home, ".bun", "bin"),
		filepath.Join(dirs.home, ".cargo", "bin"),
		filepath.Join(dirs.home, ".deno", "bin"),
	}, sep)
	if got := envValue(plan.env, "PATH"); got != wantPath {
		t.Fatalf("PATH = %q, want %q", got, wantPath)
	}
	if got := envValue(plan.env, "HOME"); got != dirs.home {
		t.Fatalf("HOME = %q, want the executor's own home %q", got, dirs.home)
	}
	if got := envValue(plan.env, "XDG_CONFIG_HOME"); got != dirs.config {
		t.Fatalf("XDG_CONFIG_HOME = %q, want %q", got, dirs.config)
	}

	// A session of the same executor runs with that PATH too: the two must not
	// drift apart, or a tool `box run --executor` finds is an executor's
	// "command not found".
	ex, err := buildExecutor("prime", cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, sessionEnv, err := ex.Argv(ExecSpec{SessionID: "s-1", Model: "p/m", Cwd: plan.dir, SessionDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := envValue(sessionEnv, "PATH"), envValue(plan.env, "PATH"); got != want {
		t.Fatalf("a session PATH = %q, the command's = %q: they must be the same", got, want)
	}
}

// TestExecPlanStartsInThePreparedSources: the workspace is the directory the
// box's preparation left, and a command in it runs there without boxd being
// asked.
func TestExecPlanStartsInThePreparedSources(t *testing.T) {
	cfg := testConfig(t)
	dest := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepareStatePath(cfg),
		[]byte(`{"state":"ready","dest":"`+dest+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := execPlanFor(cfg, "prime", "", []string{"true"})
	if err != nil {
		t.Fatalf("execPlanFor: %v", err)
	}
	if plan.dir != dest {
		t.Fatalf("working directory = %q, want the prepared sources %q", plan.dir, dest)
	}
	// The executor's home directories are made for the command, so it never
	// writes into the home of the user it happens to run as.
	dirs := cfg.dirsOf("prime")
	if got := envValue(plan.env, "HOME"); got != dirs.home {
		t.Fatalf("HOME = %q, want the executor's %q", got, dirs.home)
	}
	if got := envValue(plan.env, "XDG_CONFIG_HOME"); got != dirs.config {
		t.Fatalf("XDG_CONFIG_HOME = %q, want %q", got, dirs.config)
	}
	for _, dir := range []string{dirs.home, dirs.config} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Fatalf("%s was not made for the command: %v", dir, err)
		}
	}
	// --cwd is what the caller said, and it is made when it is not there.
	wanted := filepath.Join(t.TempDir(), "in", "deep")
	plan, err = execPlanFor(cfg, "prime", wanted, []string{"true"})
	if err != nil {
		t.Fatalf("execPlanFor: %v", err)
	}
	if plan.dir != wanted {
		t.Fatalf("working directory = %q, want --cwd %q", plan.dir, wanted)
	}
	if _, err := os.Stat(wanted); err != nil {
		t.Fatalf("--cwd was not created: %v", err)
	}
}

// TestExecPlanRefusesWhatItCannotRun: a name that is not an executor, a
// directory that is not absolute and a command that is not on PATH are answered
// as the invocation's own failure, with the exit status a caller reads them by.
func TestExecPlanRefusesWhatItCannotRun(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("PATH", "/usr/bin:/bin")

	_, err := execPlanFor(cfg, "opancde", "", []string{"true"})
	var f *execFailure
	if !errors.As(err, &f) || f.code != 2 {
		t.Fatalf("an unknown executor = %v, want exit 2", err)
	}
	if !strings.Contains(f.msg, "prime, opencode, claude") {
		t.Fatalf("the message names no executor: %q", f.msg)
	}

	_, err = execPlanFor(cfg, "prime", "relative/dir", []string{"true"})
	if !errors.As(err, &f) || f.code != 2 || !strings.Contains(f.msg, "not an absolute path") {
		t.Fatalf("a relative --cwd = %v, want exit 2", err)
	}

	_, err = execPlanFor(cfg, "prime", "", []string{"no-such-program"})
	if !errors.As(err, &f) || f.code != 127 {
		t.Fatalf("a command that is not there = %v, want exit 127", err)
	}
	if !strings.Contains(f.msg, "command not found in the prime executor's PATH") {
		t.Fatalf("the message does not say where it looked: %q", f.msg)
	}
}

// TestBoxctlExecNeedsAnExecutorAndACommand: `boxctl exec` is the one command
// that is not a JSON answer, so a bad invocation is a sentence on stderr and an
// exit status, and nothing is started.
func TestBoxctlExecNeedsAnExecutorAndACommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no executor", []string{"exec", "--", "true"}, "--executor"},
		{"no command", []string{"exec", "--executor", "prime"}, "needs a command"},
		{"unknown executor", []string{"exec", "--executor", "nope", "--", "true"}, "the box runs prime, opencode, claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			if code := RunBoxctl(tc.args, strings.NewReader(""), &out, &errw); code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, errw.String())
			}
			if !strings.Contains(errw.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q", errw.String(), tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q: the command's output is its own, boxctl prints nothing", out.String())
			}
		})
	}
}

// writeExecutable puts a program of one line into dir.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
