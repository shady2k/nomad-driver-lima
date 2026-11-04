package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
//
// A journalctl command is run in JSON so the driver keeps the cursor of the
// last line it wrote: a restarted stream goes on after it (--after-cursor)
// instead of writing the last lines again. A box that has just booted is
// streamed from the start of its boot; one the driver re-attached to after a
// restart of its own, from now.
func (d *Driver) startGuestLogs(parent context.Context, h *taskHandle, fromBoot bool) {
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
		// The guest's journal and the driver's own lines share stdout, the
		// stream `nomad alloc logs` and the UI show first; a lineWriter keeps
		// one from landing in the middle of the other's line.
		out := io.Writer(io.Discard)
		if stdout != nil {
			lw := &lineWriter{w: stdout}
			defer lw.flush()
			out = lw
			h.mu.Lock()
			h.logOut = lw
			h.mu.Unlock()
			defer func() {
				h.mu.Lock()
				h.logOut = nil
				h.mu.Unlock()
			}()
		}
		journal := journalStream(h.cfg.GuestLogCommand)
		cursor := ""
		for {
			if ctx.Err() != nil {
				return
			}
			argv := h.cfg.GuestLogCommand
			var jw *journalWriter
			if journal {
				argv = journalArgv(argv, cursor, fromBoot)
				jw = &journalWriter{out: out}
			}
			cmd := d.boxm.Lima().ShellCmd(ctx, h.boxID, argv)
			cmd.Stdout = out
			if jw != nil {
				cmd.Stdout = jw
			}
			cmd.Stderr = writerOrDiscard(stderr)
			err := cmd.Run()
			if jw != nil {
				jw.flush()
				if jw.cursor != "" {
					cursor = jw.cursor
				}
				if cursor == "" {
					// Nothing was shown yet: from now on, not the boot again.
					fromBoot = false
				}
			}
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

// logTimeLayout is the time a line of the task log starts with; boxd's journal
// lines use the same (boxd.TimeLayout), so the two read as one log.
const logTimeLayout = "2006-01-02 15:04:05 UTC"

// noteExec writes one line about a command run in the guest to the task's
// stdout, next to boxd's journal, so `nomad alloc logs` and the UI show what
// was done in the box besides its sessions. The command is cut short: the
// line says what ran, not what it carried.
func (h *taskHandle) noteExec(format string, args ...any) {
	h.mu.RLock()
	w := h.logOut
	h.mu.RUnlock()
	if w == nil {
		return
	}
	line := fmt.Sprintf("%s box exec: %s\n", time.Now().UTC().Format(logTimeLayout), fmt.Sprintf(format, args...))
	_, _ = io.WriteString(w, line)
}

// execLabel is a command line as noteExec shows it. A `box run` command comes
// wrapped (setsid -w sh -c <wrapper> <pid file> cmd...); the label is cmd.
func execLabel(command []string) string {
	if len(command) > 6 && command[0] == "setsid" && command[1] == "-w" && command[2] == "sh" && command[3] == "-c" &&
		strings.HasPrefix(command[5], "/tmp/box-run-") {
		command = command[6:]
	}
	s := strings.Join(command, " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// execProgressEvery is how often a guest command that is still running is
// noted, so a long build or test run is not silence in the task log.
const execProgressEvery = 2 * time.Minute

// noteExecProgress notes a running guest command every execProgressEvery until
// done is closed.
func (h *taskHandle) noteExecProgress(label string, started time.Time, done <-chan struct{}) {
	ticker := time.NewTicker(execProgressEvery)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			h.noteExec("still running after %s: %s", time.Since(started).Round(time.Second), label)
		}
	}
}

// lineWriter writes whole lines to w: a write that ends mid-line keeps the
// rest until its newline comes, and whole lines from different writers never
// interleave.
type lineWriter struct {
	mu      sync.Mutex
	w       io.Writer
	partial []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	if i := bytes.LastIndexByte(l.partial, '\n'); i >= 0 {
		if _, err := l.w.Write(l.partial[:i+1]); err != nil {
			l.partial = l.partial[i+1:]
			return len(p), err
		}
		l.partial = append(l.partial[:0], l.partial[i+1:]...)
	}
	return len(p), nil
}

// flush writes what is left of an unfinished line.
func (l *lineWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		_, _ = l.w.Write(append(l.partial, '\n'))
		l.partial = nil
	}
}

// journalStream reports whether the guest log command is journalctl, which
// the driver then runs in JSON to keep a cursor.
func journalStream(argv []string) bool {
	for _, a := range argv {
		if filepath.Base(a) == "journalctl" {
			return true
		}
	}
	return false
}

// journalArgv is the journalctl command in JSON, from after cursor when there
// is one, else from the start of the boot or from now.
func journalArgv(argv []string, cursor string, fromBoot bool) []string {
	out := make([]string, 0, len(argv)+4)
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case (a == "-o" || a == "--output") && i+1 < len(argv):
			i++
			continue
		case strings.HasPrefix(a, "--output="), strings.HasPrefix(a, "-o") && len(a) > 2:
			continue
		}
		out = append(out, a)
	}
	out = append(out, "-o", "json")
	switch {
	case cursor != "":
		out = append(out, "--after-cursor="+cursor)
	case fromBoot:
		out = append(out, "-b", "-n", "all")
	default:
		out = append(out, "-n", "0")
	}
	return out
}

// journalWriter turns journalctl's JSON lines into the messages they carry,
// and keeps the cursor of the last one.
type journalWriter struct {
	out     io.Writer
	partial []byte
	cursor  string
}

func (j *journalWriter) Write(p []byte) (int, error) {
	j.partial = append(j.partial, p...)
	for {
		i := bytes.IndexByte(j.partial, '\n')
		if i < 0 {
			break
		}
		j.line(j.partial[:i])
		j.partial = j.partial[i+1:]
	}
	return len(p), nil
}

func (j *journalWriter) flush() {
	if len(j.partial) > 0 {
		j.line(j.partial)
		j.partial = nil
	}
}

func (j *journalWriter) line(b []byte) {
	var e struct {
		Cursor  string          `json:"__CURSOR"`
		Message json.RawMessage `json:"MESSAGE"`
	}
	if json.Unmarshal(b, &e) != nil {
		return
	}
	msg := ""
	if json.Unmarshal(e.Message, &msg) != nil {
		// journald gives a message that is not UTF-8 as an array of bytes.
		var raw []byte
		var nums []int
		if json.Unmarshal(e.Message, &nums) == nil {
			for _, n := range nums {
				raw = append(raw, byte(n))
			}
			msg = strings.ToValidUTF8(string(raw), "?")
		}
	}
	if e.Cursor != "" {
		j.cursor = e.Cursor
	}
	_, _ = io.WriteString(j.out, strings.TrimRight(msg, "\n")+"\n")
}
