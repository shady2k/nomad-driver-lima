package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// guestExitCode turns the exit code of a guest command into the command's exit.
func guestExitCode(code int) error {
	if code == 0 {
		return nil
	}
	// No message: the guest already wrote whatever it had to say to stderr.
	return &Error{Code: code}
}

// cmdSSH opens an interactive shell in a box.
func (a *App) cmdSSH(ctx context.Context, args []string) error {
	fs := newFlagSet("ssh")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	if a.Terminal == nil || !a.Terminal.IsInteractive() {
		return usagef("box ssh needs a terminal (stdin is not one); use `box run %s -- cmd` in scripts", id)
	}
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	box, err = a.waitRunning(ctx, box, guestWaitTimeout)
	if err != nil {
		return err
	}
	alloc, err := a.runningAlloc(ctx, box)
	if err != nil {
		return err
	}
	tr, err := a.guestTransport(ctx, alloc)
	if err != nil {
		return err
	}
	restore, err := a.Terminal.MakeRaw()
	if err != nil {
		return wrapf(err, "cannot put the terminal into raw mode")
	}
	defer restore()
	code, err := tr.Exec(ctx, ExecOpts{
		Command:  []string{"bash", "-l"},
		Stdin:    a.In,
		Stdout:   a.Out,
		Stderr:   a.ErrOut,
		TTY:      true,
		TermSize: a.Terminal.Sizes(),
	})
	if err != nil {
		return wrapf(err, "the shell in box %s ended with an error", id)
	}
	if code != 0 {
		a.note("the shell in box %s exited with code %d", id, code)
	}
	return nil
}

// cmdRun runs one command in a box and returns its exit code.
func (a *App) cmdRun(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("box run needs a box: box run <id> -- cmd [args...]")
	}
	id := args[0]
	rest := args[1:]
	var limit time.Duration
	for len(rest) > 0 && rest[0] != "--" && strings.HasPrefix(rest[0], "--timeout") {
		value := strings.TrimPrefix(strings.TrimPrefix(rest[0], "--timeout"), "=")
		rest = rest[1:]
		if value == "" {
			if len(rest) == 0 {
				return usagef("box run %s: --timeout needs a duration, for example --timeout 20m", id)
			}
			value, rest = rest[0], rest[1:]
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return usagef("box run %s: --timeout %q is not a positive duration (for example 20m)", id, value)
		}
		limit = d
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return usagef("box run %s: no command was given; use: box run %s [--timeout D] -- cmd [args...]", id, id)
	}
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	box, err = a.waitRunning(ctx, box, guestWaitTimeout)
	if err != nil {
		return err
	}
	alloc, err := a.runningAlloc(ctx, box)
	if err != nil {
		return err
	}
	tr, err := a.guestTransport(ctx, alloc)
	if err != nil {
		return err
	}
	stdin := a.In
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	// Nomad does not tell the box that the caller has gone while the command
	// prints nothing, so the command would run on with no one to read how it
	// ended. It runs in a session of its own whose leader writes its pid to a
	// file; a run that is interrupted or runs out of --timeout stops that
	// whole session before it returns.
	pidFile := fmt.Sprintf("/tmp/box-run-%d-%d.pid", os.Getpid(), time.Now().UnixNano())
	command := append([]string{"setsid", "-w", "sh", "-c", runWrapper, pidFile}, rest...)
	runCtx := ctx
	if limit > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	code, err := tr.Exec(runCtx, ExecOpts{
		Command: command,
		Stdin:   stdin,
		Stdout:  a.Out,
		Stderr:  a.ErrOut,
	})
	if runCtx.Err() != nil {
		stopped := a.stopGuestRun(tr, pidFile)
		if limit > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return timeoutf("box run %s: %q ran out of its --timeout %s; %s", id, strings.Join(rest, " "), limit, stopped)
		}
		return errorf("box run %s: interrupted; %s", id, stopped)
	}
	if err != nil {
		return wrapf(err, "cannot run %q in box %s", strings.Join(rest, " "), id)
	}
	return guestExitCode(code)
}

// runWrapper runs the command ("$@") as a child of the session leader, which
// notes its pid in "$0" and removes the note when the command ends.
const runWrapper = `echo $$ > "$0"; "$@"; c=$?; rm -f "$0"; exit $c`

// stopGuestRun stops a `box run` command left in the guest: TERM to its whole
// session, then KILL to what is left. It says what happened, in words.
func (a *App) stopGuestRun(tr ExecTransport, pidFile string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := `p=$(cat "$0" 2>/dev/null) || exit 3; kill -TERM -"$p" 2>/dev/null; ` +
		`for i in 1 2 3 4 5; do kill -0 -"$p" 2>/dev/null || break; sleep 1; done; ` +
		`kill -KILL -"$p" 2>/dev/null; rm -f "$0"`
	code, err := tr.Exec(ctx, ExecOpts{Command: []string{"sh", "-c", script, pidFile}})
	switch {
	case err != nil:
		return "the command may still run in the box: it could not be stopped (" + oneLine(err.Error(), 160) + ")"
	case code == 3:
		return "the command had already ended in the box"
	case code != 0:
		return fmt.Sprintf("the command may still run in the box: stopping it exited %d", code)
	}
	return "the command in the box was stopped too"
}

// cmdLogs streams the task log of a box, which is the boxd event stream.
//
// Nomad keeps stdout and stderr of the task apart; both belong to the log the
// coordinator reads, so both are streamed.
func (a *App) cmdLogs(ctx context.Context, args []string) error {
	fs := newFlagSet("logs")
	follow := fs.Bool("f", false, "follow the log")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	if box.AllocationID == "" {
		return errorf("the box %s has no allocation, so it has no logs; `box start %s` runs it again", id, id)
	}
	alloc, err := a.Nomad.Allocation(ctx, box.AllocationID)
	if err != nil {
		if isNotFoundErr(err) {
			return notFoundf("the allocation of box %s is gone; its logs are gone with it", id)
		}
		return wrapf(err, "cannot read allocation %s", box.AllocationID)
	}

	writer := &lockedWriter{w: a.Out}
	streams := []string{"stdout", "stderr"}
	errs := make(chan error, len(streams))
	var wg sync.WaitGroup
	for _, logType := range streams {
		wg.Add(1)
		go func(logType string) {
			defer wg.Done()
			errs <- a.streamLog(ctx, alloc, logType, *follow, writer)
		}(logType)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return wrapf(err, "the log stream of box %s ended with an error", id)
		}
	}
	return nil
}

// streamLog copies one log file of a task to w.
func (a *App) streamLog(ctx context.Context, alloc *api.Allocation, logType string, follow bool, w io.Writer) error {
	frames, errCh := a.Nomad.Logs(ctx, alloc, boxTaskName, logType, follow)
	var streamErr error
	for frames != nil {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				break
			}
			if frame != nil && len(frame.Data) > 0 {
				writeAll(w, frame.Data)
			}
		case err := <-errCh:
			// Keep reading the frames that are already buffered: the stream
			// closes them when it is done.
			if err != nil && !errors.Is(err, io.EOF) {
				streamErr = err
			}
			errCh = nil
		}
	}
	return streamErr
}

// lockedWriter serialises two log streams onto one writer.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
