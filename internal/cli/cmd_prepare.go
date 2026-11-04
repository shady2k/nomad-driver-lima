package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A box is prepared before agents work in it: boxd puts the sources there (a
// git clone, or files from this machine) and runs the project's recipe,
// .box/prepare.sh in them. Sessions wait for it; docs/cli.md has the states.

// defaultPrepareTimeout is the time limit of a preparation, and how long the
// CLI waits for it.
const defaultPrepareTimeout = time.Hour

// workspaceRoot is where sources go in the box when --dest names no place.
const workspaceRoot = "/work"

// prepareFlags are the source flags of `box create` and `box prepare`.
type prepareFlags struct {
	repo, ref, from, dest *string
}

func addPrepareFlags(fs *flag.FlagSet) prepareFlags {
	return prepareFlags{
		repo: fs.String("repo", "", "git repository the box clones as its sources"),
		ref:  fs.String("ref", "", "branch, tag or commit of --repo to check out"),
		from: fs.String("from", "", "a directory or file on this machine to send as the sources, without git"),
		dest: fs.String("dest", "", "where the sources go in the box (default "+workspaceRoot+"/<name>)"),
	}
}

func (f prepareFlags) given() bool { return *f.repo != "" || *f.from != "" }

// check refuses flags that do not go together, before anything is created.
func (f prepareFlags) check() error {
	switch {
	case *f.repo != "" && *f.from != "":
		return usagef("--repo and --from are two sources; give one")
	case *f.ref != "" && *f.repo == "":
		return usagef("--ref belongs to --repo")
	case *f.dest != "" && !f.given():
		return usagef("--dest needs --repo or --from")
	case *f.dest != "" && !path.IsAbs(*f.dest):
		return usagef("--dest %q must be an absolute path in the box", *f.dest)
	}
	if *f.from != "" {
		if _, err := os.Stat(*f.from); err != nil {
			return usagef("--from %s: %v", *f.from, err)
		}
	}
	return nil
}

// options turns the flags into a preparation; the files stream is opened
// here and closed by the transfer.
func (f prepareFlags) options(limit time.Duration) (PrepareOptions, error) {
	opts := PrepareOptions{Repo: *f.repo, Ref: *f.ref, Dest: *f.dest, Limit: limit}
	if err := f.check(); err != nil {
		return opts, err
	}
	if *f.from != "" {
		info, err := os.Stat(*f.from)
		if err != nil {
			return opts, usagef("--from %s: %v", *f.from, err)
		}
		name := "."
		if !info.IsDir() {
			name = filepath.Base(*f.from)
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(PackTar(pw, *f.from, name)) }()
		opts.Files = pr
		if opts.Dest == "" {
			abs, _ := filepath.Abs(*f.from)
			base := filepath.Base(abs)
			if !info.IsDir() {
				base = strings.TrimSuffix(base, filepath.Ext(base))
			}
			opts.Dest = path.Join(workspaceRoot, safeName(base))
		}
	}
	if opts.Repo != "" && opts.Dest == "" {
		opts.Dest = path.Join(workspaceRoot, safeName(repoName(opts.Repo)))
	}
	return opts, nil
}

// repoName is the last path element of a repository URL, without .git.
func repoName(repo string) string {
	s := strings.TrimRight(repo, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".git")
}

// safeName keeps a directory name to letters, digits, dot, dash and underscore.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), ".-")
	if name == "" {
		return "src"
	}
	return name
}

// prepareOutput is the JSON document of `box prepare` and the preparation part
// of `box create`.
type prepareOutput struct {
	Box     string        `json:"box"`
	Prepare *PrepareState `json:"prepare"`
}

// cmdPrepare starts, waits for, cancels or skips the preparation of a box.
func (a *App) cmdPrepare(ctx context.Context, args []string) error {
	fs := newFlagSet("prepare")
	src := addPrepareFlags(fs)
	timeout := fs.Duration("timeout", defaultPrepareTimeout, "time limit of the preparation, and how long to wait for it")
	again := fs.Bool("again", false, "run the recipe again on the sources the box has")
	cancel := fs.Bool("cancel", false, "stop the running preparation")
	skip := fs.Bool("skip", false, "let sessions start without a preparation")
	noWait := fs.Bool("no-wait", false, "start it and return")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	actions := 0
	for _, b := range []bool{src.given(), *again, *cancel, *skip} {
		if b {
			actions++
		}
	}
	if actions > 1 {
		return usagef("box prepare %s: give one of --repo/--from, --again, --cancel or --skip", id)
	}
	if *timeout <= 0 {
		return usagef("--timeout must be positive, got %s", *timeout)
	}
	opts, err := src.options(*timeout)
	if err != nil {
		return err
	}
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case *cancel:
		st, err := client.PrepareCancel(ctx)
		if err != nil {
			return wrapf(err, "box %s", id)
		}
		return a.printPrepare(id, st, "the preparation of box %s was cancelled; `box prepare %s` runs it again, "+
			"`box prepare %s --skip` lets sessions start without it", id, id, id)
	case *skip:
		st, err := client.PrepareSkip(ctx)
		if err != nil {
			return wrapf(err, "box %s", id)
		}
		return a.printPrepare(id, st, "sessions in box %s start without a preparation", id)
	case src.given(), *again:
		return a.prepareBox(ctx, client, id, opts, *timeout, *noWait)
	}
	// No action: wait for a running preparation, run a failed one again, or
	// say where it is.
	st, err := client.PrepareGet(ctx)
	if err != nil {
		return wrapf(err, "box %s", id)
	}
	switch st.State {
	case "preparing":
		if *noWait {
			return a.printPrepare(id, st, "box %s is being prepared (%s)", id, prepareDoing(st))
		}
		return a.finishPrepare(ctx, client, id, *timeout)
	case "failed":
		return a.prepareBox(ctx, client, id, opts, *timeout, *noWait)
	case "none":
		return usagef("box %s has never been prepared: name its sources with --repo URL or --from PATH", id)
	}
	return a.printPrepare(id, st, "box %s is %s%s; `box prepare %s --again` runs the recipe again", id, st.State, prepareWhere(st), id)
}

// prepareBox starts a preparation and, unless noWait, waits for its end.
func (a *App) prepareBox(ctx context.Context, client *Boxctl, id string, opts PrepareOptions, timeout time.Duration, noWait bool) error {
	st, err := a.startPrepare(ctx, client, opts)
	if err != nil {
		return wrapf(err, "box %s could not start its preparation", id)
	}
	if st.State == "failed" {
		return prepareFailure(id, st)
	}
	what := "the recipe again"
	switch {
	case opts.Repo != "":
		what = "a clone of " + opts.Repo
	case opts.Files != nil:
		what = "the files"
	}
	a.note("preparing box %s: %s in %s", id, what, dash(st.Dest))
	if noWait {
		return a.printPrepare(id, st, "box %s is being prepared; `box prepare %s` waits for it", id, id)
	}
	return a.finishPrepare(ctx, client, id, timeout)
}

// startPrepare starts it, asking again while a box that has just started has
// no boxd to answer yet.
func (a *App) startPrepare(ctx context.Context, client *Boxctl, opts PrepareOptions) (*PrepareState, error) {
	deadline := time.Now().Add(guestWaitTimeout)
	for {
		st, err := client.PrepareStart(ctx, opts)
		if err == nil || opts.Files != nil || !isBoxctlCode(err, "boxd_unavailable") || time.Now().After(deadline) {
			return st, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// finishPrepare waits for the preparation to end and says how it did, with
// what it is doing while it runs.
func (a *App) finishPrepare(ctx context.Context, client *Boxctl, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var said string
	var saidAt time.Time
	for {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		chunk := 30 * time.Second
		if left < chunk {
			chunk = left
		}
		st, err := client.PrepareWait(ctx, chunk)
		if err != nil {
			return wrapf(err, "box %s did not say how its preparation goes; `box prepare %s` asks again", id, id)
		}
		if st.State != "preparing" {
			if st.State == "failed" {
				return prepareFailure(id, st)
			}
			return a.printPrepare(id, st, "box %s is ready%s", id, prepareWhere(st))
		}
		if doing := prepareDoing(st); doing != said && time.Since(saidAt) >= 15*time.Second {
			a.note("  %s", doing)
			said, saidAt = doing, time.Now()
		}
	}
	return timeoutf("box %s is still being prepared after %s; `box prepare %s` keeps waiting, "+
		"`box prepare %s --cancel` stops it", id, timeout, id, id)
}

// prepareFailure is a failed preparation as an error: the reason, the last
// lines it wrote, and the ways out.
func prepareFailure(id string, st *PrepareState) error {
	msg := fmt.Sprintf("the preparation of box %s failed", id)
	if st.Step != "" {
		msg += " in the " + st.Step + " step"
	}
	msg += ": " + st.Error
	if tail := lastLines(st.Tail, 10); tail != "" {
		msg += "\nits last lines:\n" + tail
	}
	msg += fmt.Sprintf("\n`box prepare %s` runs it again (with --repo or --from for other sources), "+
		"`box prepare %s --skip` lets sessions start without it", id, id)
	return errorf("%s", msg)
}

func lastLines(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func prepareDoing(st *PrepareState) string {
	s := dash(st.Step)
	if st.LastLine != "" {
		s += ": " + st.LastLine
	}
	return s
}

func prepareWhere(st *PrepareState) string {
	if st.Dest == "" {
		return ""
	}
	s := ": sources in " + st.Dest
	if st.Recipe != "" {
		s += ", " + st.Recipe + " ran"
	}
	return s
}

func (a *App) printPrepare(id string, st *PrepareState, format string, args ...any) error {
	if a.JSON {
		return a.printJSON(prepareOutput{Box: id, Prepare: st})
	}
	a.note(format, args...)
	return nil
}

// isBoxctlCode reports whether err is boxctl's error with that code.
func isBoxctlCode(err error, code string) bool {
	var be *BoxctlError
	return errors.As(err, &be) && be.Code == code
}
