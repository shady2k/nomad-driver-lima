package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// logRestartDelay is how long the driver waits before running guest_log_command
// again after it exited while the VM is still up (for example because the guest
// service does not exist yet).
const logRestartDelay = 5 * time.Second

// startGuestLogs streams the output of guestLogCommand into the task's stdout
// and stderr streams, which is what makes `nomad alloc logs` show guest logs.
// The command is restarted while the VM runs; the stream ends when ctx ends.
func (d *Driver) startGuestLogs(parent context.Context, h *taskHandle) {
	ctx, cancel := context.WithCancel(parent)
	h.mu.Lock()
	h.logCancel = cancel
	h.mu.Unlock()

	go func() {
		defer cancel()
		stdout, stderr, err := openTaskLogWriters(ctx, h.tc)
		if err != nil {
			d.logger.Warn("cannot open task log streams; guest logs are dropped", "task", h.tc.ID, "error", err)
			return
		}
		defer closeWriter(stdout)
		defer closeWriter(stderr)
		if stdout == nil && stderr == nil {
			return
		}
		for {
			if ctx.Err() != nil {
				return
			}
			cmd := d.boxm.Lima().ShellCmd(ctx, h.boxID, h.cfg.GuestLogCommand)
			cmd.Stdout = writerOrDiscard(stdout)
			cmd.Stderr = writerOrDiscard(stderr)
			err := cmd.Run()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				fmt.Fprintf(writerOrDiscard(stderr), "box driver: guest log command %v exited: %v\n", h.cfg.GuestLogCommand, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(logRestartDelay):
			}
		}
	}()
}

func (d *Driver) stopGuestLogs(h *taskHandle) {
	h.mu.Lock()
	cancel := h.logCancel
	h.logCancel = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// openTaskLogWriters opens the FIFOs Nomad's logmon reads. Opening a FIFO for
// writing blocks until the reader is attached, which logmon does before the task
// starts, so this returns promptly in the normal case.
func openTaskLogWriters(ctx context.Context, tc *drivers.TaskConfig) (io.WriteCloser, io.WriteCloser, error) {
	stdout, err := openWriter(ctx, tc.StdoutPath)
	if err != nil {
		return nil, nil, err
	}
	stderr, err := openWriter(ctx, tc.StderrPath)
	if err != nil {
		closeWriter(stdout)
		return nil, nil, err
	}
	return stdout, stderr, nil
}

func openWriter(ctx context.Context, path string) (io.WriteCloser, error) {
	if path == "" {
		return nil, nil
	}
	type result struct {
		f   *os.File
		err error
	}
	done := make(chan result, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		done <- result{f: f, err: err}
	}()
	select {
	case <-ctx.Done():
		// The open may still be blocked; the goroutine returns when a reader
		// appears and closes the file, so nothing leaks for long.
		go func() {
			if r := <-done; r.err == nil && r.f != nil {
				r.f.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			if errors.Is(r.err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, r.err
		}
		return r.f, nil
	}
}

func closeWriter(w io.WriteCloser) {
	if w != nil {
		_ = w.Close()
	}
}

func writerOrDiscard(w io.WriteCloser) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
