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
		// A command cannot be sent twice: a stream that was lost may have run it
		// to the end, or not at all, and there is no way to ask. Saying it failed
		// would make an agent run it again, and saying it passed would be a lie.
		if lostContact(err) && ctx.Err() == nil {
			if gone := a.boxGone(ctx, id); gone != nil {
				return gone
			}
			message := fmt.Sprintf("lost contact with the box %s while %q ran: what the command did is not known, and it may still be running in the box. "+
				"It is not run again. `box run %s -- ps -ef` shows what runs when the box answers, `box logs %s -f` what it printed",
				id, strings.Join(rest, " "), id, id)
			return unknownf("%s", withVMNote(message, a.vmNote(ctx, box)))
		}
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
// coordinator reads, so both are streamed. Each stream resumes from the byte it
// left off at, so a client that restarts mid-read costs neither a repeated line
// nor a lost one, and only a box that is really gone ends the stream.
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
	alloc, err := a.logsAlloc(ctx, box)
	if err != nil {
		return err
	}

	writer := &lockedWriter{w: a.Out}
	contact := a.newContact(id)
	streams := []string{"stdout", "stderr"}
	errs := make(chan error, len(streams))
	var wg sync.WaitGroup
	for _, logType := range streams {
		wg.Add(1)
		go func(logType string) {
			defer wg.Done()
			errs <- a.streamLog(ctx, id, alloc, logType, *follow, writer, contact)
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

// logsAlloc returns the allocation whose task log is the box's journal.
func (a *App) logsAlloc(ctx context.Context, box BoxRecord) (*api.Allocation, error) {
	alloc, err := a.Nomad.Allocation(ctx, box.AllocationID)
	if err != nil {
		if isNotFoundErr(err) {
			return nil, notFoundf("the allocation of box %s is gone; its logs are gone with it", box.ID)
		}
		return nil, wrapf(err, "cannot read allocation %s", box.AllocationID)
	}
	return alloc, nil
}

// streamLog copies one log file of a task to w, and picks it up again where it
// stopped when the connection to the box is lost. It returns nil when the file
// ended (without --f), when the caller interrupted it, or when the box is gone.
func (a *App) streamLog(ctx context.Context, boxID string, alloc *api.Allocation, logType string, follow bool, w io.Writer, contact *contactOfBox) error {
	var offset int64
	attempts := 0
	// Contact is back when a line arrives, not when the request succeeds: a
	// stream that opens and stays silent proves nothing.
	onFrame := func() { contact.back(logType) }
	for {
		err := a.streamLogOnce(ctx, alloc, logType, follow, w, &offset, onFrame)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		if !lostContact(err) {
			return err
		}
		contact.lost(logType, err)
		attempts++
		if err := contact.waitForContact(ctx, boxID, attempts); err != nil {
			return err
		}
		// The allocation may have been replaced while the client was away; the
		// log belongs to the box, so it is read from whichever one runs it now.
		if box, _, err := a.findBox(ctx, boxID); err == nil && box.AllocationID != "" && box.AllocationID != alloc.ID {
			if fresh, err := a.logsAlloc(ctx, box); err == nil {
				alloc = fresh
				offset = 0
			}
		}
	}
}

// streamLogOnce copies one log file of a task to w from offset until the file
// ends or the stream fails, and advances offset by what it read.
//
// The stream ends one of two ways: the frames channel closes, which is the end of
// the file, or the error channel reports a failure -- a client that restarted
// closes neither, it drops the connection, and Nomad then sends the error. The
// bytes read are what the next attempt asks from, so a frame that arrived with
// the failure and was not written is read again rather than lost.
func (a *App) streamLogOnce(ctx context.Context, alloc *api.Allocation, logType string, follow bool, w io.Writer, offset *int64, onFrame func()) error {
	frames, errCh := a.Nomad.Logs(ctx, alloc, boxTaskName, logType, follow, *offset)
	write := func(frame *api.StreamFrame) {
		if frame == nil || len(frame.Data) == 0 {
			return
		}
		if onFrame != nil {
			onFrame()
		}
		writeAll(w, frame.Data)
		*offset += int64(len(frame.Data))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			write(frame)
		case err := <-errCh:
			if err == nil || errors.Is(err, io.EOF) {
				// Nothing more will be said on the error side; the frames still
				// say when the file ends.
				errCh = nil
				continue
			}
			// The stream was lost. What it had already buffered is the log, so
			// it goes out before the failure is reported.
			for {
				select {
				case frame, ok := <-frames:
					if !ok {
						return err
					}
					write(frame)
					continue
				default:
				}
				return err
			}
		}
	}
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
