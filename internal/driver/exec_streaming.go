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
	command := execOptions.Command
	if !execOptions.Tty {
		command = relayedCommand(command)
	}
	cmd := d.boxm.Lima().ShellCmd(ctx, h.boxID, command)
	if quietExec(execOptions.Command) {
		if execOptions.Tty {
			return d.execWithTTY(ctx, cmd, execOptions)
		}
		return d.execPipes(cmd, execOptions)
	}
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

// guestRelay runs a command in the guest with its output going through two cats
// that end with it. ssh keeps a session open while anything holds its stdout or
// stderr, so a daemon a command left in the background would hold the exec until
// it exits, however long after the command itself. The daemon holds a fifo here
// instead. Without a place for the fifos (a full disk is when `df` matters most)
// the command runs as it is.
const guestRelay = `d=$(mktemp -d 2>/dev/null) && mkfifo "$d/o" "$d/e" 2>/dev/null || exec "$@"
cat "$d/o" & o=$!
cat "$d/e" >&2 & e=$!
"$@" >"$d/o" 2>"$d/e"
c=$?
(sleep ` + guestRelayGrace + `; kill $o $e) </dev/null >/dev/null 2>&1 & t=$!
wait $o $e 2>/dev/null
kill $t 2>/dev/null
rm -rf "$d"
exit $c`

// guestRelayGrace is how long the relay reads what is left once the command has
// exited, for a stream a background process keeps open.
const guestRelayGrace = "2"

// relayedCommand wraps a guest command in guestRelay.
func relayedCommand(command []string) []string {
	return append([]string{"sh", "-c", guestRelay, "box-exec"}, command...)
}

// quietExec reports whether a command only reads boxd's state: the CLI's
// boxctl calls that ask how a box, a session or a preparation is doing, which
// it repeats while it waits. They change nothing, and what they report is in
// boxd's own journal lines already, so the task log does not list them.
func quietExec(command []string) bool {
	if len(command) < 2 || command[0] != "/usr/local/bin/boxctl" {
		return false
	}
	switch command[1] {
	case "info", "events", "wait":
		return true
	case "session":
		return len(command) > 2 && (command[2] == "ls" || command[2] == "get")
	case "prepare":
		return len(command) > 2 && (command[2] == "get" || command[2] == "wait")
	}
	return false
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
	// guestRelay is what ends a stream a child in the guest keeps open; this is
	// the bound for the host's own side.
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
