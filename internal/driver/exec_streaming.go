package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/creack/pty"

	"github.com/hashicorp/nomad/plugins/drivers"
)

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

	if execOptions.Tty {
		return d.execWithTTY(ctx, cmd, execOptions)
	}

	cmd.Stdin = execOptions.Stdin
	cmd.Stdout = execOptions.Stdout
	cmd.Stderr = execOptions.Stderr
	err := cmd.Run()
	res := &drivers.ExitResult{}
	if err != nil {
		if code := exitCode(err); code >= 0 {
			res.ExitCode = code
		} else {
			res.ExitCode = -1
			res.Err = err
		}
	}
	closeIfNotNil(execOptions.Stdout)
	closeIfNotNil(execOptions.Stderr)
	return res, nil
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
