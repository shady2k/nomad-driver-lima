package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/creack/pty"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// execWaitDelay is how long an exec waits for its streams once the command in
// the guest has exited.
const execWaitDelay = 5 * time.Second

// ExecTaskStreaming runs a command inside the guest and connects its streams to
// the caller, allocating a pseudo terminal when Tty is set. Everything goes
// through `limactl shell <box> -- <command>`, so the guest sees a normal process
// and the host user's Lima SSH key.
func (d *Driver) ExecTaskStreaming(ctx context.Context, taskID string, execOptions *drivers.ExecOptions) (*drivers.ExitResult, error) {
	h := d.tasks.Get(taskID)
	if h == nil {
		return nil, fmt.Errorf("task %q is not running", taskID)
	}
	if execOptions == nil || len(execOptions.Command) == 0 {
		return nil, fmt.Errorf("exec needs a command")
	}
	cmd := d.boxm.Lima().ShellCmd(ctx, h.boxID, execOptions.Command)
	label := execLabel(execOptions.Command)
	started := time.Now()
	h.noteExec("started: %s", label)
	running := make(chan struct{})
	defer close(running)
	go h.noteExecProgress(label, started, running)

	if execOptions.Tty {
		res, err := d.execWithTTY(ctx, cmd, execOptions)
		h.noteExecEnd(label, started, res, err)
		return res, err
	}
	res, err := d.execPipes(cmd, execOptions)
	h.noteExecEnd(label, started, res, err)
	return res, err
}

// noteExecEnd notes how a guest command ended.
func (h *taskHandle) noteExecEnd(label string, started time.Time, res *drivers.ExitResult, err error) {
	took := time.Since(started).Round(100 * time.Millisecond)
	switch {
	case err != nil:
		h.noteExec("failed after %s: %s: %v", took, label, err)
	case res != nil && res.Err != nil:
		h.noteExec("failed after %s: %s: %v", took, label, res.Err)
	case res != nil:
		h.noteExec("exit %d after %s: %s", res.ExitCode, took, label)
	}
}

// execPipes runs a guest command on plain pipes.
func (d *Driver) execPipes(cmd *exec.Cmd, execOptions *drivers.ExecOptions) (*drivers.ExitResult, error) {
	cmd.Stdout = execOptions.Stdout
	cmd.Stderr = execOptions.Stderr
	// Nomad's stdin is a stream, not a file. Left to os/exec, it is copied in a
	// goroutine that Wait waits for until the stream ends, so a caller whose
	// stdin stays open without sending anything (an agent's shell) would wait
	// forever for a command that has already exited. The copy is ours instead:
	// Wait closes the pipe when the command exits and does not wait for it.
	if execOptions.Stdin != nil {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("stdin of the guest command: %w", err)
		}
		go func() {
			_, _ = io.Copy(in, execOptions.Stdin)
			_ = in.Close()
		}()
	}
	// A child in the guest that keeps stdout open after the command exits must
	// not hold the exec either.
	cmd.WaitDelay = execWaitDelay
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command exited, only a stream was still open: its own exit
		// code is the answer.
		err = nil
		if code := cmd.ProcessState.ExitCode(); code != 0 {
			return &drivers.ExitResult{ExitCode: code}, closeStreams(execOptions)
		}
	}
	res := &drivers.ExitResult{}
	if err != nil {
		if code := exitCode(err); code >= 0 {
			res.ExitCode = code
		} else {
			res.ExitCode = -1
			res.Err = err
		}
	}
	return res, closeStreams(execOptions)
}

// closeStreams closes the caller's output streams once the command is done.
func closeStreams(opts *drivers.ExecOptions) error {
	closeIfNotNil(opts.Stdout)
	closeIfNotNil(opts.Stderr)
	return nil
}

// execWithTTY runs the command on a pseudo terminal so interactive sessions
// (nomad alloc exec without -t uses plain pipes, with -t uses this path) work.
func (d *Driver) execWithTTY(ctx context.Context, cmd *exec.Cmd, opts *drivers.ExecOptions) (*drivers.ExitResult, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, fmt.Errorf("start command with a tty: %w", err)
	}
	defer f.Close()

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		if opts.Stdout != nil {
			_, _ = io.Copy(opts.Stdout, f)
		} else {
			_, _ = io.Copy(io.Discard, f)
		}
	}()

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		if opts.Stdin != nil {
			_, _ = io.Copy(f, opts.Stdin)
		}
		// Do not close the pty here: the command may still be writing.
	}()

	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		if opts.ResizeCh == nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case size, ok := <-opts.ResizeCh:
				if !ok {
					return
				}
				_ = pty.Setsize(f, &pty.Winsize{Rows: uint16(size.Height), Cols: uint16(size.Width)})
			}
		}
	}()

	waitErr := cmd.Wait()
	_ = f.Close()
	<-stdoutDone
	<-resizeDone

	res := &drivers.ExitResult{}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = waitErr
		}
	}
	closeIfNotNil(opts.Stdout)
	closeIfNotNil(opts.Stderr)
	return res, nil
}

func closeIfNotNil(w io.WriteCloser) {
	if w != nil {
		_ = w.Close()
	}
}
