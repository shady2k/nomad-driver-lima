package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// boxctlPath is the guest binary every guest call goes through
// (docs/boxctl-contract.md).
const boxctlPath = "/usr/local/bin/boxctl"

// ExecOpts is one command in the box task's environment.
type ExecOpts struct {
	// Command is argv: the binary and its arguments.
	Command []string
	// Stdin, Stdout and Stderr are the streams of the command.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// TTY starts a pseudo-terminal for the command.
	TTY bool
	// TermSize feeds terminal size changes to a TTY command.
	TermSize <-chan api.TerminalSize
}

// ExecTransport runs commands inside one box.
//
// The real implementation is `nomad alloc exec`; the unit tests use a fake
// guest that answers boxctl and tar commands.
type ExecTransport interface {
	Exec(ctx context.Context, opts ExecOpts) (exitCode int, err error)
}

// GuestFactory returns the exec transport of one box allocation.
type GuestFactory func(ctx context.Context, alloc *api.Allocation) (ExecTransport, error)

// nomadExec is the real transport: Nomad's alloc exec API, which the box driver
// serves through `limactl shell`.
type nomadExec struct {
	app   *App
	alloc *api.Allocation
}

func (e *nomadExec) Exec(ctx context.Context, opts ExecOpts) (int, error) {
	// The Nomad exec client reads its stdin unconditionally, so an empty
	// reader stands in for "no input" (a nil reader panics there).
	stdin := opts.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return e.app.Nomad.Exec(ctx, e.alloc, boxTaskName, opts.Command, stdin, stdout, stderr, opts.TTY, opts.TermSize)
}

// guestTransport returns the exec transport for an allocation.
func (a *App) guestTransport(ctx context.Context, alloc *api.Allocation) (ExecTransport, error) {
	if a.Guest != nil {
		return a.Guest(ctx, alloc)
	}
	return &nomadExec{app: a, alloc: alloc}, nil
}

// Boxctl is the guest control plane of one running box.
type Boxctl struct {
	tr ExecTransport
}

// NewBoxctl wraps an exec transport.
func NewBoxctl(tr ExecTransport) *Boxctl { return &Boxctl{tr: tr} }

// BoxctlError is the error document boxctl prints.
type BoxctlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *BoxctlError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the box daemon reported %q", e.Code)
	}
	return e.Message
}

// ExecutorInfo is one agent executor the guest can run.
type ExecutorInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
	// Configured is false when the box has no credentials for the executor;
	// nil when the guest is an older boxd that does not report it.
	Configured *bool  `json:"configured,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// BoxInfo is the result of `boxctl info`.
type BoxInfo struct {
	BoxdVersion string         `json:"boxd_version"`
	Executors   []ExecutorInfo `json:"executors"`
	Sessions    int            `json:"sessions"`
	MemTotalMiB int64          `json:"mem_total_mib"`
	MemFreeMiB  int64          `json:"mem_free_mib"`
	CPUs        int            `json:"cpus"`
	// Prepare is the box's preparation; nil from a boxd that has none.
	Prepare *PrepareState `json:"prepare,omitempty"`
	// Usage is the guest's load as boxd last measured it.
	Usage *GuestUsage `json:"usage,omitempty"`
}

// GuestUsage is what the guest is doing: CPU, disk wait, memory and disk.
type GuestUsage struct {
	At            string  `json:"at"`
	CPUPercent    float64 `json:"cpu_percent"`
	IOWaitPercent float64 `json:"iowait_percent"`
	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	MemTotalMiB   uint64  `json:"mem_total_mib"`
	MemAvailMiB   uint64  `json:"mem_available_mib"`
	DiskPath      string  `json:"disk_path"`
	DiskTotalMiB  uint64  `json:"disk_total_mib"`
	DiskFreeMiB   uint64  `json:"disk_free_mib"`
	Top           string  `json:"top,omitempty"`
}

// Line is the usage in one line, the way box status and box top print it.
func (u *GuestUsage) Line() string {
	used := u.MemTotalMiB - u.MemAvailMiB
	s := fmt.Sprintf("CPU %.0f%% (iowait %.0f%%), load %.1f %.1f, memory %d of %d MiB used (%d available), disk %s %.1f of %.1f GiB used",
		u.CPUPercent, u.IOWaitPercent, u.Load1, u.Load5, used, u.MemTotalMiB, u.MemAvailMiB,
		dash(u.DiskPath), float64(u.DiskTotalMiB-u.DiskFreeMiB)/1024, float64(u.DiskTotalMiB)/1024)
	if u.Top != "" {
		s += "; the most resident: " + u.Top
	}
	return s
}

// Session is one agent session in a box.
type Session struct {
	ID              string `json:"id"`
	Executor        string `json:"executor"`
	Model           string `json:"model"`
	Cwd             string `json:"cwd,omitempty"`
	Title           string `json:"title,omitempty"`
	State           string `json:"state"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastTurn        int    `json:"last_turn"`
	ExecutorSession string `json:"executor_session,omitempty"`
	PID             int    `json:"pid,omitempty"`
}

// Turn is one prompt and the agent's answer to it.
type Turn struct {
	Session    string `json:"session"`
	Turn       int    `json:"turn"`
	State      string `json:"state"`
	StartedAt  string `json:"started_at,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Text       string `json:"text,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Event is one line of a session's activity stream.
type Event struct {
	Seq     int64          `json:"seq"`
	TS      string         `json:"ts,omitempty"`
	Session string         `json:"session,omitempty"`
	Turn    int            `json:"turn"`
	Kind    string         `json:"kind"`
	Summary string         `json:"summary"`
	Data    map[string]any `json:"data,omitempty"`
}

// call runs boxctl with args and returns its stdout.
func (b *Boxctl) call(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	command := append([]string{boxctlPath}, args...)
	var stdout, stderr bytes.Buffer
	code, err := b.tr.Exec(ctx, ExecOpts{Command: command, Stdin: stdin, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return nil, wrapf(err, "cannot run boxctl in the box")
	}
	out := stdout.Bytes()
	if code != 0 {
		var envelope struct {
			Error *BoxctlError `json:"error"`
		}
		if json.Unmarshal(out, &envelope) == nil && envelope.Error != nil {
			// Keep the code and the message: the callers switch on the code
			// (busy, boxd_unavailable) and the message is already plain words.
			return nil, &Error{Code: ExitError, Err: envelope.Error}
		}
		detail := oneLine(stderr.String(), 300)
		if detail == "" {
			detail = oneLine(string(out), 300)
		}
		if detail == "" {
			detail = "no output"
		}
		return nil, errorf("boxctl exited with code %d: %s", code, detail)
	}
	return out, nil
}

// decodeJSON decodes one JSON document.
func decodeJSON[T any](out []byte, what string) (T, error) {
	var v T
	if len(bytes.TrimSpace(out)) == 0 {
		return v, errorf("%s printed nothing", what)
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return v, wrapf(err, "%s printed output that is not JSON", what)
	}
	return v, nil
}

// Info runs `boxctl info`.
func (b *Boxctl) Info(ctx context.Context) (*BoxInfo, error) {
	out, err := b.call(ctx, nil, "info")
	if err != nil {
		return nil, err
	}
	info, err := decodeJSON[BoxInfo](out, "boxctl info")
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// SessionNewOptions are the arguments of `boxctl session new`.
type SessionNewOptions struct {
	Executor string
	Model    string
	Cwd      string
	Title    string
}

// SessionNew starts a new agent session.
func (b *Boxctl) SessionNew(ctx context.Context, opts SessionNewOptions) (string, error) {
	args := []string{"session", "new", "--executor", opts.Executor, "--model", opts.Model}
	if opts.Cwd != "" {
		args = append(args, "--cwd", opts.Cwd)
	}
	if opts.Title != "" {
		args = append(args, "--title", opts.Title)
	}
	out, err := b.call(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	resp, err := decodeJSON[struct {
		Session string `json:"session"`
	}](out, "boxctl session new")
	if err != nil {
		return "", err
	}
	if resp.Session == "" {
		return "", errorf("boxctl session new returned no session id")
	}
	return resp.Session, nil
}

// SessionList runs `boxctl session ls`.
func (b *Boxctl) SessionList(ctx context.Context) ([]Session, error) {
	out, err := b.call(ctx, nil, "session", "ls")
	if err != nil {
		return nil, err
	}
	resp, err := decodeJSON[struct {
		Sessions []Session `json:"sessions"`
	}](out, "boxctl session ls")
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// SessionGet runs `boxctl session get <id>`.
func (b *Boxctl) SessionGet(ctx context.Context, id string) (*Session, error) {
	out, err := b.call(ctx, nil, "session", "get", id)
	if err != nil {
		return nil, err
	}
	session, err := decodeJSON[Session](out, "boxctl session get")
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// SessionClose runs `boxctl session close <id>`.
func (b *Boxctl) SessionClose(ctx context.Context, id string) error {
	_, err := b.call(ctx, nil, "session", "close", id)
	return err
}

// Send passes prompt text to a session on stdin and returns the turn number.
func (b *Boxctl) Send(ctx context.Context, id, prompt string) (int, error) {
	out, err := b.call(ctx, bytes.NewBufferString(prompt), "send", id)
	if err != nil {
		return 0, err
	}
	resp, err := decodeJSON[struct {
		Session string `json:"session"`
		Turn    int    `json:"turn"`
	}](out, "boxctl send")
	if err != nil {
		return 0, err
	}
	return resp.Turn, nil
}

// WaitOptions are the arguments of `boxctl wait`.
type WaitOptions struct {
	// Turn waits for one specific turn; zero means the running one.
	Turn int
	// Timeout is handed to boxctl as --timeout seconds; zero means none.
	Timeout int
}

// Wait waits for a turn to finish. The returned turn has state "running" when
// boxctl's own timeout expired.
func (b *Boxctl) Wait(ctx context.Context, id string, opts WaitOptions) (*Turn, error) {
	args := []string{"wait", id}
	if opts.Turn > 0 {
		args = append(args, "--turn", strconv.Itoa(opts.Turn))
	}
	if opts.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(opts.Timeout))
	}
	out, err := b.call(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	turn, err := decodeJSON[Turn](out, "boxctl wait")
	if err != nil {
		return nil, err
	}
	return &turn, nil
}

// EventsOptions are the arguments of `boxctl events`.
type EventsOptions struct {
	// Since is the sequence number to resume from.
	Since int64
	// Limit bounds the number of events.
	Limit int
}

// Events returns the events of a session and the next sequence number.
func (b *Boxctl) Events(ctx context.Context, id string, opts EventsOptions) ([]Event, int64, error) {
	args := []string{"events", id}
	if opts.Since > 0 {
		args = append(args, "--since", strconv.FormatInt(opts.Since, 10))
	}
	if opts.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(opts.Limit))
	}
	out, err := b.call(ctx, nil, args...)
	if err != nil {
		return nil, 0, err
	}
	resp, err := decodeJSON[struct {
		Events []Event `json:"events"`
		Next   int64   `json:"next"`
	}](out, "boxctl events")
	if err != nil {
		return nil, 0, err
	}
	next := resp.Next
	if next == 0 {
		for _, event := range resp.Events {
			if event.Seq >= next {
				next = event.Seq + 1
			}
		}
	}
	return resp.Events, next, nil
}

// Cancel cancels the running turn of a session.
func (b *Boxctl) Cancel(ctx context.Context, id string) (*Turn, error) {
	out, err := b.call(ctx, nil, "cancel", id)
	if err != nil {
		return nil, err
	}
	turn, err := decodeJSON[Turn](out, "boxctl cancel")
	if err != nil {
		return nil, err
	}
	return &turn, nil
}

// PrepareState is where a box's preparation is (boxctl prepare get).
type PrepareState struct {
	State      string   `json:"state"`
	Step       string   `json:"step,omitempty"`
	Source     string   `json:"source,omitempty"`
	Repo       string   `json:"repo,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Dest       string   `json:"dest,omitempty"`
	Recipe     string   `json:"recipe,omitempty"`
	LimitSec   int      `json:"limit_sec,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Error      string   `json:"error,omitempty"`
	LastLine   string   `json:"last_line,omitempty"`
	Tail       []string `json:"tail,omitempty"`
}

// PrepareOptions starts a preparation: a repository, files (a tar stream), or
// neither, which runs the recipe again on the sources there are.
type PrepareOptions struct {
	Repo, Ref, Dest string
	Files           io.Reader
	Limit           time.Duration
}

// PrepareStart runs `boxctl prepare start`.
func (b *Boxctl) PrepareStart(ctx context.Context, opts PrepareOptions) (*PrepareState, error) {
	args := []string{"prepare", "start"}
	if opts.Dest != "" {
		args = append(args, "--dest", opts.Dest)
	}
	if opts.Repo != "" {
		args = append(args, "--repo", opts.Repo)
	}
	if opts.Ref != "" {
		args = append(args, "--ref", opts.Ref)
	}
	if opts.Files != nil {
		args = append(args, "--files")
	}
	if opts.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(int(opts.Limit.Seconds())))
	}
	return b.prepareCall(ctx, opts.Files, args...)
}

// PrepareGet runs `boxctl prepare get`.
func (b *Boxctl) PrepareGet(ctx context.Context) (*PrepareState, error) {
	return b.prepareCall(ctx, nil, "prepare", "get")
}

// PrepareWait runs `boxctl prepare wait`: it returns when the preparation is
// not running, or after timeout.
func (b *Boxctl) PrepareWait(ctx context.Context, timeout time.Duration) (*PrepareState, error) {
	return b.prepareCall(ctx, nil, "prepare", "wait", "--timeout", strconv.Itoa(int(timeout.Seconds())))
}

// PrepareCancel runs `boxctl prepare cancel`.
func (b *Boxctl) PrepareCancel(ctx context.Context) (*PrepareState, error) {
	return b.prepareCall(ctx, nil, "prepare", "cancel")
}

// PrepareSkip runs `boxctl prepare skip`.
func (b *Boxctl) PrepareSkip(ctx context.Context) (*PrepareState, error) {
	return b.prepareCall(ctx, nil, "prepare", "skip")
}

func (b *Boxctl) prepareCall(ctx context.Context, stdin io.Reader, args ...string) (*PrepareState, error) {
	out, err := b.call(ctx, stdin, args...)
	if err != nil {
		return nil, err
	}
	st, err := decodeJSON[PrepareState](out, "boxctl "+strings.Join(args[:2], " "))
	if err != nil {
		return nil, err
	}
	return &st, nil
}
