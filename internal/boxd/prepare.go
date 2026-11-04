package boxd

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A box is prepared before its sessions work in it: its sources arrive (a git
// clone, or files sent from the caller's machine) and the project's own recipe
// runs, .box/prepare.sh in the sources. boxd does it, not an agent's shell, so
// a preparation survives the caller going away and says how it ended.
//
// The states and every way out of them:
//
//	none      -> preparing        (prepare)
//	preparing -> ready            (the recipe exited 0)
//	preparing -> failed           (a step failed, the time limit, cancel,
//	                               or boxd restarted in the middle)
//	failed    -> preparing        (prepare again: new sources, or the recipe
//	                               again on the sources there are)
//	failed    -> skipped          (skip: sessions start without it)
//	ready, skipped -> preparing   (prepare again)
//
// Sessions start in none, ready and skipped. preparing always ends: it has a
// time limit, and cancel ends it at once.

// Preparation states.
const (
	PrepareNone      = "none"
	PrepareRunning   = "preparing"
	PrepareReady     = "ready"
	PrepareFailed    = "failed"
	PrepareSkipped   = "skipped"
	prepareRecipe    = ".box/prepare.sh"
	prepareTailLines = 30
	// prepareJournalLines bounds what one preparation writes to the journal
	// line by line; after that only progress lines go there.
	prepareJournalLines = 5000
	// DefaultPrepareLimit is the time limit of a preparation that names none.
	DefaultPrepareLimit = time.Hour
	// prepareKillGrace is how long a stopped step has between SIGTERM and
	// SIGKILL.
	prepareKillGrace = 10 * time.Second
)

// PrepareState is where the box's preparation is.
type PrepareState struct {
	State  string `json:"state"`
	Step   string `json:"step,omitempty"`   // receive, clone, checkout, recipe, owner
	Source string `json:"source,omitempty"` // git or files
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	// Dest is where the sources are in the guest; sessions start there.
	Dest string `json:"dest,omitempty"`
	// Recipe is the recipe that ran, relative to Dest; "" when there is none.
	Recipe     string     `json:"recipe,omitempty"`
	Owner      string     `json:"owner,omitempty"` // uid:gid the sources belong to
	LimitSec   int        `json:"limit_sec,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	// LastLine is the last line a step wrote; Tail the last lines, kept for a
	// failure.
	LastLine string   `json:"last_line,omitempty"`
	Tail     []string `json:"tail,omitempty"`
}

// PrepareRequest starts a preparation. Without Repo it is either files (the
// tar stream of PrepareFiles) or, with no stream either, the recipe again on
// the sources of the last preparation.
type PrepareRequest struct {
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Dest     string `json:"dest,omitempty"`
	Owner    string `json:"owner,omitempty"`
	LimitSec int    `json:"limit_sec,omitempty"`
}

// preparer runs the box's one preparation at a time.
type preparer struct {
	cfg  DaemonConfig
	path string

	mu      sync.Mutex
	st      PrepareState
	cancel  context.CancelFunc
	changed chan struct{} // closed and replaced on every state change
	lines   int
	saidAt  time.Time
}

func newPreparer(cfg DaemonConfig) *preparer {
	p := &preparer{cfg: cfg, path: filepath.Join(cfg.StateDir, "prepare.json"), changed: make(chan struct{})}
	p.st = PrepareState{State: PrepareNone}
	if b, err := os.ReadFile(p.path); err == nil {
		var st PrepareState
		if json.Unmarshal(b, &st) == nil && st.State != "" {
			p.st = st
		}
	}
	// A preparation boxd did not see end has no process left to end it.
	if p.st.State == PrepareRunning {
		p.st.State = PrepareFailed
		p.st.Error = fmt.Sprintf("boxd restarted during the %s step, so the preparation stopped there; "+
			"`box prepare` runs it again", p.st.Step)
		now := cfg.now()
		p.st.FinishedAt = &now
		p.persistLocked()
		cfg.logf("prepare: %s", p.st.Error)
	}
	return p
}

// State returns the current state.
func (p *preparer) State() PrepareState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.copyLocked()
}

func (p *preparer) copyLocked() PrepareState {
	st := p.st
	st.Tail = append([]string(nil), p.st.Tail...)
	return st
}

// Wait returns once the preparation is not running, or when timeout passes.
func (p *preparer) Wait(ctx context.Context, timeout time.Duration) PrepareState {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		st := p.copyLocked()
		ch := p.changed
		p.mu.Unlock()
		if st.State != PrepareRunning {
			return st
		}
		select {
		case <-ch:
		case <-deadline.C:
			return st
		case <-ctx.Done():
			return st
		}
	}
}

// sessionGate says why a session cannot start now, or nil.
func (p *preparer) sessionGate() error {
	st := p.State()
	switch st.State {
	case PrepareRunning:
		what := st.Step
		if st.LastLine != "" {
			what += ": " + st.LastLine
		}
		return Errorf(CodeBusy, "the box is still being prepared (%s); `box prepare <box>` waits for it, "+
			"`box prepare <box> --cancel` stops it", what)
	case PrepareFailed:
		return Errorf(CodeInvalid, "the box's preparation failed: %s; `box prepare <box>` runs it again, "+
			"`box prepare <box> --skip` lets sessions start without it", st.Error)
	}
	return nil
}

// workdir is where a session starts when it names no directory.
func (p *preparer) workdir() string {
	st := p.State()
	if st.State == PrepareReady && st.Dest != "" && dirExists(st.Dest) {
		return st.Dest
	}
	return ""
}

// Start begins a preparation from git, or runs the recipe again on the last
// sources. Files go through StartFiles.
func (p *preparer) Start(req PrepareRequest) (PrepareState, error) {
	if req.Repo == "" {
		return p.startAgain(req)
	}
	dest, err := p.claim(req, "git")
	if err != nil {
		return PrepareState{}, err
	}
	ctx := p.runContext()
	go p.run(ctx, func(ctx context.Context) error { return p.clone(ctx, req, dest) }, dest)
	return p.State(), nil
}

// StartFiles unpacks a tar stream as the sources and then runs the recipe. The
// stream is read before it returns: the caller's connection carries it.
func (p *preparer) StartFiles(req PrepareRequest, body io.Reader) (PrepareState, error) {
	dest, err := p.claim(req, "files")
	if err != nil {
		return PrepareState{}, err
	}
	ctx := p.runContext()
	p.step("receive")
	tmp := dest + ".partial"
	_ = os.RemoveAll(tmp)
	if err := unpackInto(ctx, body, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		p.finish(fmt.Errorf("receive the files: %w", err))
		return p.State(), nil
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		p.finish(fmt.Errorf("move the files to %s: %w", dest, err))
		return p.State(), nil
	}
	p.line(fmt.Sprintf("received the files into %s", dest))
	go p.run(ctx, func(context.Context) error { return nil }, dest)
	return p.State(), nil
}

// startAgain runs the recipe once more on the sources the last preparation
// left: the way out of a failed recipe after its cause is fixed.
func (p *preparer) startAgain(req PrepareRequest) (PrepareState, error) {
	p.mu.Lock()
	if p.st.State == PrepareRunning {
		p.mu.Unlock()
		return PrepareState{}, p.busyError()
	}
	dest := p.st.Dest
	if dest == "" || !dirExists(dest) {
		p.mu.Unlock()
		return PrepareState{}, Errorf(CodeInvalid, "there are no sources to prepare again: name them with --repo or --from")
	}
	prev := p.st
	p.st = PrepareState{State: PrepareRunning, Source: prev.Source, Repo: prev.Repo, Ref: prev.Ref, Dest: dest,
		Owner: firstNonEmpty(req.Owner, prev.Owner), LimitSec: limitOf(req.LimitSec)}
	p.startedLocked()
	p.mu.Unlock()
	p.cfg.logf("prepare: running the recipe again in %s", dest)
	ctx := p.runContext()
	go p.run(ctx, func(context.Context) error { return nil }, dest)
	return p.State(), nil
}

// claim moves the state to preparing for new sources at their destination.
func (p *preparer) claim(req PrepareRequest, source string) (string, error) {
	dest := req.Dest
	if dest == "" {
		return "", Errorf(CodeInvalid, "a preparation needs a destination directory")
	}
	if !filepath.IsAbs(dest) {
		return "", Errorf(CodeInvalid, "the destination %q is not an absolute path", dest)
	}
	dest = filepath.Clean(dest)
	if _, err := parseOwner(req.Owner); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st.State == PrepareRunning {
		return "", p.busyError()
	}
	if _, err := os.Lstat(dest); err == nil {
		return "", Errorf(CodeInvalid, "%s already exists in the box: name another --dest, or remove it "+
			"(`box run <box> -- sudo rm -rf %s`) to put new sources there", dest, dest)
	}
	p.st = PrepareState{State: PrepareRunning, Source: source, Repo: req.Repo, Ref: req.Ref, Dest: dest,
		Owner: req.Owner, LimitSec: limitOf(req.LimitSec)}
	p.startedLocked()
	return dest, nil
}

func (p *preparer) busyError() error {
	return Errorf(CodeBusy, "the box is already being prepared (%s); `box prepare <box>` waits for it, "+
		"`box prepare <box> --cancel` stops it", p.st.Step)
}

func (p *preparer) startedLocked() {
	now := p.cfg.now()
	p.st.StartedAt = &now
	p.lines = 0
	p.saidAt = time.Time{}
	p.persistLocked()
	p.notifyLocked()
}

// runContext is the context of one preparation: its time limit, and the
// cancel that Cancel calls.
func (p *preparer) runContext() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.st.LimitSec)*time.Second)
	p.cancel = cancel
	return ctx
}

// Cancel stops a running preparation; it ends as failed, cancelled.
func (p *preparer) Cancel() (PrepareState, error) {
	p.mu.Lock()
	if p.st.State != PrepareRunning || p.cancel == nil {
		st := p.copyLocked()
		p.mu.Unlock()
		return st, Errorf(CodeInvalid, "the box is not being prepared (it is %s)", st.State)
	}
	p.st.Error = "cancelled"
	cancel := p.cancel
	p.mu.Unlock()
	p.cfg.logf("prepare: cancelling the %s step", p.State().Step)
	cancel()
	return p.Wait(context.Background(), prepareKillGrace+5*time.Second), nil
}

// Skip lets sessions start without a preparation.
func (p *preparer) Skip() (PrepareState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st.State == PrepareRunning {
		return p.copyLocked(), Errorf(CodeBusy, "the box is being prepared; `box prepare <box> --cancel` stops it first")
	}
	p.st.State = PrepareSkipped
	p.persistLocked()
	p.notifyLocked()
	p.cfg.logf("prepare: skipped; sessions start without a preparation")
	return p.copyLocked(), nil
}

// run does the source step, then the recipe, then hands the sources to their
// owner, and records how it ended.
func (p *preparer) run(ctx context.Context, sources func(context.Context) error, dest string) {
	err := sources(ctx)
	if err == nil {
		err = p.recipe(ctx, dest)
	}
	if err == nil {
		err = p.chown(dest)
	}
	if err == nil {
		allowGitEverywhere(p.cfg)
	}
	if ctx.Err() != nil && err != nil {
		p.mu.Lock()
		cancelled := p.st.Error == "cancelled"
		limit := p.st.LimitSec
		p.mu.Unlock()
		if cancelled {
			err = errors.New("cancelled")
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("the preparation ran out of its time limit (%s) in the %s step; "+
				"`box prepare <box> --timeout D` gives it longer", time.Duration(limit)*time.Second, p.State().Step)
		}
	}
	p.finish(err)
}

func (p *preparer) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.now()
	p.st.FinishedAt = &now
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	took := ""
	if p.st.StartedAt != nil {
		took = " after " + now.Sub(*p.st.StartedAt).Round(time.Second).String()
	}
	if err != nil {
		p.st.State = PrepareFailed
		p.st.Error = err.Error()
		p.cfg.logf("prepare: failed%s: %s", took, err)
	} else {
		p.st.State = PrepareReady
		p.st.Error = ""
		p.cfg.logf("prepare: ready%s: %s", took, p.st.Dest)
	}
	p.persistLocked()
	p.notifyLocked()
}

// clone clones the repository next to its destination and moves it there when
// it is complete, so a failed clone leaves no half a checkout behind.
func (p *preparer) clone(ctx context.Context, req PrepareRequest, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".partial"
	_ = os.RemoveAll(tmp)
	p.step("clone")
	p.line(fmt.Sprintf("cloning %s into %s", req.Repo, dest))
	if err := p.command(ctx, "", nil, "git", "clone", "--progress", "--", req.Repo, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("git clone %s: %w", req.Repo, err)
	}
	if req.Ref != "" {
		p.step("checkout")
		if err := p.command(ctx, tmp, nil, "git", "checkout", "--quiet", req.Ref); err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("git checkout %s: %w", req.Ref, err)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// recipe runs .box/prepare.sh of the sources, when they have one.
func (p *preparer) recipe(ctx context.Context, dest string) error {
	script := filepath.Join(dest, prepareRecipe)
	if !fileExists(script) {
		p.line(fmt.Sprintf("no %s in the sources, nothing to run", prepareRecipe))
		return nil
	}
	p.mu.Lock()
	p.st.Recipe = prepareRecipe
	p.mu.Unlock()
	p.step("recipe")
	p.line("running " + prepareRecipe)
	env := append(os.Environ(), "BOX_WORKSPACE="+dest, "DEBIAN_FRONTEND=noninteractive")
	if err := p.command(ctx, dest, env, "bash", "-e", prepareRecipe); err != nil {
		return fmt.Errorf("%s: %w", prepareRecipe, err)
	}
	return nil
}

// chown hands the sources to the user that asked for them, so `box run` and
// `box cp` work in them as that user.
func (p *preparer) chown(dest string) error {
	p.mu.Lock()
	owner := p.st.Owner
	p.mu.Unlock()
	ids, err := parseOwner(owner)
	if err != nil || ids == nil {
		return err
	}
	p.step("owner")
	return filepath.WalkDir(dest, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, ids[0], ids[1]); err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		return nil
	})
}

// command runs one step's process in its own process group, so a cancel or
// the time limit stops everything it started, and turns its output into
// journal lines.
func (p *preparer) command(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = prepareKillGrace
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.Stdin = nil
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.readLines(pr)
	}()
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	_ = pw.Close()
	<-done
	if cmd.Process != nil {
		// Whatever the step left running in its group goes with it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			msg := fmt.Sprintf("exit %d", exit.ExitCode())
			if last := p.State().LastLine; last != "" {
				msg += ": " + last
			}
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// readLines splits a step's output into lines (git's progress uses carriage
// returns) and records each one.
func (p *preparer) readLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, b := range data {
			if b == '\n' || b == '\r' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			p.line(s)
		}
	}
	_, _ = io.Copy(io.Discard, r)
}

// progressLine reports whether a line only says how far a step got ("Receiving
// objects:  45% ..."), which the journal gets at most every progressGap.
func progressLine(s string) bool { return strings.Contains(s, "%") }

const progressGap = 15 * time.Second

// line records one line of the current step: in the state, and in the journal.
func (p *preparer) line(s string) {
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	p.mu.Lock()
	p.st.LastLine = s
	p.st.Tail = append(p.st.Tail, s)
	if len(p.st.Tail) > prepareTailLines {
		p.st.Tail = p.st.Tail[len(p.st.Tail)-prepareTailLines:]
	}
	now := p.cfg.now()
	say := false
	switch {
	case progressLine(s) || p.lines >= prepareJournalLines:
		say = now.Sub(p.saidAt) >= progressGap
	default:
		say = true
	}
	if say {
		p.lines++
		p.saidAt = now
	}
	step := p.st.Step
	p.notifyLocked()
	p.mu.Unlock()
	if say {
		p.cfg.logf("prepare %s: %s", step, s)
	}
}

func (p *preparer) step(name string) {
	p.mu.Lock()
	p.st.Step = name
	p.persistLocked()
	p.notifyLocked()
	p.mu.Unlock()
}

func (p *preparer) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *preparer) persistLocked() {
	b, err := json.MarshalIndent(p.st, "", "  ")
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
		_ = os.Rename(tmp, p.path)
	}
}

// unpackInto extracts a tar stream into dir. Names that leave dir are refused.
func unpackInto(ctx context.Context, r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("the archive names %q, which is outside the destination", h.Name)
		}
		if clean == "." {
			continue
		}
		target := filepath.Join(dir, clean)
		mode := fs.FileMode(h.Mode) & 0o7777
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the archive holds %q of a kind boxd does not unpack", h.Name)
		}
	}
}

// allowGitEverywhere lets git work in sources that belong to another user: the
// executors run as root, the sources belong to the box's user.
func allowGitEverywhere(cfg DaemonConfig) {
	out, _ := exec.Command("git", "config", "--system", "--get-all", "safe.directory").Output()
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == "*" {
			return
		}
	}
	if err := exec.Command("git", "config", "--system", "--add", "safe.directory", "*").Run(); err != nil {
		cfg.logf("prepare: git may refuse the sources as another user's: cannot set safe.directory: %v", err)
	}
}

// parseOwner reads "uid:gid"; "" is no owner.
func parseOwner(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return nil, Errorf(CodeInvalid, "owner %q is not uid:gid", s)
	}
	uid, err1 := strconv.Atoi(parts[0])
	gid, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return nil, Errorf(CodeInvalid, "owner %q is not uid:gid", s)
	}
	return []int{uid, gid}, nil
}

func limitOf(sec int) int {
	if sec <= 0 {
		return int(DefaultPrepareLimit / time.Second)
	}
	return sec
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
