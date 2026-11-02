package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

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
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return usagef("box run %s: no command was given; use: box run %s -- cmd [args...]", id, id)
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
	code, err := tr.Exec(ctx, ExecOpts{
		Command: rest,
		Stdin:   stdin,
		Stdout:  a.Out,
		Stderr:  a.ErrOut,
	})
	if err != nil {
		return wrapf(err, "cannot run %q in box %s", strings.Join(rest, " "), id)
	}
	return guestExitCode(code)
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
