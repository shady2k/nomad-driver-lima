package boxd

// `boxctl exec` runs one command the way an executor's sessions run: as boxd's
// user, in the environment those sessions get, and in the box's workspace. What
// an agent makes in a box belongs to root, and the CLI reaches the guest as the
// box's login user, so `box run --executor` calls this through `sudo -n`.
//
// It is the one boxctl command that does not answer with a JSON document: it
// replaces itself with the command (exec), so the command's stdout, stderr, its
// terminal, its signals and its exit code are the caller's, exactly as if it had
// been started without boxctl in between.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// execFailure is a reason `boxctl exec` did not start the command at all, and the
// exit status it answers with: 2 for an invocation it cannot use, as every boxctl
// command answers a malformed one, and 127 for a command that is not on the
// executor's PATH, which is what a shell says.
type execFailure struct {
	code int
	msg  string
}

func (e *execFailure) Error() string { return e.msg }

// execUsagef is a failure of the invocation of `boxctl exec` itself.
func execUsagef(format string, args ...any) *execFailure {
	return &execFailure{code: 2, msg: fmt.Sprintf(format, args...)}
}

// execBrokenf is a failure of the box rather than of the invocation: a home or a
// directory the command needs is not there or cannot be made.
func execBrokenf(format string, args ...any) *execFailure {
	return &execFailure{code: 1, msg: fmt.Sprintf(format, args...)}
}

// execPlan is the process `boxctl exec` starts.
type execPlan struct {
	// path is the program, as it was found on the executor's PATH.
	path string
	// argv is the program with its arguments, as the caller wrote them.
	argv []string
	// env is the environment the executor's sessions get.
	env []string
	// dir is the working directory.
	dir string
}

// cmdExec parses `boxctl exec` and becomes the command.
func cmdExec(args []string, errw io.Writer) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	executor := fs.String("executor", "", "the executor whose sessions the command runs as")
	cwd := fs.String("cwd", "", "the working directory (default: the box's workspace)")
	if err := fs.Parse(args); err != nil {
		return execFail(errw, execUsagef("exec: %v; the command's own flags go after --", err))
	}
	if *executor == "" {
		return execFail(errw, execUsagef("exec needs --executor NAME: the box runs %s", strings.Join(executorNames, ", ")))
	}
	if fs.NArg() == 0 {
		return execFail(errw, execUsagef("exec needs a command: boxctl exec --executor NAME -- cmd [args...]"))
	}
	cfg := boxctlConfig()
	plan, err := execPlanFor(cfg, *executor, *cwd, fs.Args())
	if err != nil {
		return execFail(errw, err)
	}
	// The command replaces this process: nothing of boxctl is left to report
	// anything, and the caller sees the command's exit status and streams.
	if err := os.Chdir(plan.dir); err != nil {
		return execFail(errw, execBrokenf("the working directory %s: %v", plan.dir, err))
	}
	if err := syscall.Exec(plan.path, plan.argv, plan.env); err != nil {
		return execFail(errw, execBrokenf("cannot start %s: %v", plan.path, err))
	}
	return 0
}

// execFail says why the command was not started and answers with the status a
// caller can read it by.
func execFail(errw io.Writer, err error) int {
	var f *execFailure
	if !errors.As(err, &f) {
		f = execBrokenf("%v", err)
	}
	fmt.Fprintf(errw, "boxctl exec: %s\n", f.msg)
	return f.code
}

// boxctlConfig is the daemon configuration boxctl works with: the defaults boxd
// has, and the environment overrides its own flags read, so a command starts in
// the directories the box's daemon uses.
func boxctlConfig() DaemonConfig {
	return DaemonConfig{
		StateDir:   os.Getenv("BOXD_STATE_DIR"),
		RuntimeDir: os.Getenv("BOXD_RUNTIME_DIR"),
		SecretsDir: os.Getenv("BOXD_SECRETS_DIR"),
	}.withDefaults()
}

// execPlanFor builds the process `boxctl exec` starts: the program the command
// names, found on the PATH of the executor's sessions, that environment, and the
// directory the command runs in.
func execPlanFor(cfg DaemonConfig, executor, cwd string, command []string) (execPlan, error) {
	ex, err := buildExecutor(executor, cfg)
	if err != nil {
		return execPlan{}, execUsagef("unknown executor %q: the box runs %s", executor, strings.Join(executorNames, ", "))
	}
	// The executor's HOME and XDG_CONFIG_HOME are what boxd makes when it starts.
	// A command that runs while boxd is down gets them too: without a HOME of its
	// own, a tool writes into the home of the user the command runs as.
	dirs := cfg.dirsOf(executor)
	for _, dir := range []string{dirs.home, dirs.config} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return execPlan{}, execBrokenf("%s for the %s executor: %v", dir, executor, err)
		}
	}
	env, err := ex.Env()
	if err != nil {
		return execPlan{}, execBrokenf("the environment of the %s executor: %v", executor, err)
	}
	env = withoutSudoEnv(env)
	env = appendFiltered(env, "USER", "USER=root")
	env = appendFiltered(env, "LOGNAME", "LOGNAME=root")
	if cwd == "" {
		cwd = cfg.sessionWorkdir(readPrepareState(cfg))
	}
	if !filepath.IsAbs(cwd) {
		return execPlan{}, execUsagef("the working directory %q is not an absolute path in the box", cwd)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return execPlan{}, execBrokenf("the working directory %s: %v", cwd, err)
	}
	path, ok := lookPathIn(env, command[0])
	if !ok {
		return execPlan{}, &execFailure{code: 127, msg: fmt.Sprintf(
			"%s: command not found in the %s executor's PATH (%s)", command[0], executor, pathOf(env))}
	}
	return execPlan{path: path, argv: command, env: env, dir: cwd}, nil
}

// withoutSudoEnv removes the login user's sudo metadata from a command environment.
func withoutSudoEnv(env []string) []string {
	out := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "SUDO_") {
			out = append(out, entry)
		}
	}
	return out
}

// lookPathIn finds a program on the PATH of an environment. exec.LookPath asks
// the PATH of this process, which is not the one the command runs with: that one
// is the bundle's bin and the executor's own home directories in front of what
// boxd inherited.
func lookPathIn(env []string, name string) (string, bool) {
	if strings.ContainsRune(name, '/') {
		// A path is a path: the kernel resolves it, relative to the working
		// directory the command starts in.
		return name, true
	}
	for _, dir := range filepath.SplitList(pathOf(env)) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if isExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// isExecutable reports whether p is a file anyone may run.
func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0
}
