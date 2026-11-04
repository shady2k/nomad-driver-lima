package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// boxctlPath is the guest binary every guest call goes through
// (docs/boxctl-contract.md).
const boxctlPath = "/usr/local/bin/boxctl"

// isGuestSilent reports whether err is a guest that ran out of its call bound.
func isGuestSilent(err error) bool {
	var silent *guestSilent
	return errors.As(err, &silent)
}

// guestCallTimeout bounds the guest call of `box status`, `box top` and
// `box session ls`: a guest that has stopped answering must not hang them.
//
// A guest out of memory does not answer at all: no reply to boxctl, and a
// command that waited for it showed nothing - not even the facts Nomad had, and
// not even that the guest was the problem. Twenty seconds is longer than a
// healthy guest needs for `boxctl info` (one round trip through `nomad alloc
// exec`) and short enough to read as a status rather than a hang.
//
// The commands that run work in the guest are not bounded this way: `box wait`,
// `box send`, `box run` and `box exec` take as long as the work does, and their
// caller decides (`--timeout`, Ctrl-C).
const guestCallTimeout = 20 * time.Second

// guestSilent is a guest call that ran out of its bound: the guest does not
// answer. It carries the call and the cause for the sentence a person reads.
type guestSilent struct {
	// timeout is how long the call waited.
	timeout time.Duration
	// what names the call, e.g. "`boxctl info`".
	what string
	// err is what the transport reported when the call was cut.
	err error
}

func (g *guestSilent) Error() string {
	msg := fmt.Sprintf("the guest does not answer (no reply in %s): %s", g.timeout, g.what)
	if g.err != nil {
		msg += ": " + g.err.Error()
	}
	return msg
}

func (g *guestSilent) Unwrap() error { return g.err }

// guestTimeout is the bound of a bounded guest call; the tests shorten it.
func (a *App) guestTimeout() time.Duration {
	if a.GuestTimeout > 0 {
		return a.GuestTimeout
	}
	return guestCallTimeout
}

// guestCall runs one guest call with the guest call bound and turns the bound's
// expiry into a guestSilent. what names the call for the message.
//
// The bound is a context, so the abandoned exec ends on the client side too:
// Nomad's exec session returns as soon as its context ends and closes its
// websocket (api.execSession.run), which is what makes the client stop waiting
// for the call instead of leaving a stream and a goroutine behind.
func (a *App) guestCall(ctx context.Context, what string, call func(context.Context) error) error {
	timeout := a.guestTimeout()
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := call(bounded)
	if err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return &guestSilent{timeout: timeout, what: what, err: err}
	}
	return err
}

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
	// MemoryPressure is the guest kernel's own report on memory stalls, absent
	// from a boxd on a kernel without /proc/pressure/memory.
	MemoryPressure *GuestPressure `json:"memory_pressure,omitempty"`
	DiskPath       string         `json:"disk_path"`
	DiskTotalMiB   uint64         `json:"disk_total_mib"`
	DiskFreeMiB    uint64         `json:"disk_free_mib"`
	Top            string         `json:"top,omitempty"`
}

// GuestPressure is how much of the last ten seconds the guest was stalled on
// memory: some of its tasks, or all of them.
type GuestPressure struct {
	SomeAvg10 float64 `json:"some_avg10"`
	FullAvg10 float64 `json:"full_avg10"`
}

// Line is the usage in one line, the way box status and box top print it.
func (u *GuestUsage) Line() string {
	used := u.MemTotalMiB - u.MemAvailMiB
	parts := []string{
		fmt.Sprintf("CPU %.0f%% (iowait %.0f%%)", u.CPUPercent, u.IOWaitPercent),
		fmt.Sprintf("load %.1f %.1f", u.Load1, u.Load5),
		fmt.Sprintf("memory %d of %d MiB used (%d available)", used, u.MemTotalMiB, u.MemAvailMiB),
	}
	if p := u.MemoryPressure; p != nil {
		parts = append(parts, fmt.Sprintf("memory pressure some %.1f%% full %.1f%% (avg10)", p.SomeAvg10, p.FullAvg10))
	}
	parts = append(parts, fmt.Sprintf("disk %s %.1f of %.1f GiB used",
		dash(u.DiskPath), float64(u.DiskTotalMiB-u.DiskFreeMiB)/1024, float64(u.DiskTotalMiB)/1024))
	s := strings.Join(parts, ", ")
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

// SealImage is the image record a seal wrote: the box is an image now.
type SealImage struct {
	Schema   int    `json:"schema"`
	Version  string `json:"version,omitempty"`
	Name     string `json:"name"`
	Arch     string `json:"arch,omitempty"`
	Parent   string `json:"parent,omitempty"`
	Platform string `json:"platform"`
	Recipe   string `json:"recipe,omitempty"`
	SealedAt string `json:"sealed_at"`
}

// SealResult is what `boxctl seal` reported: the image, and what the seal did.
type SealResult struct {
	Image  SealImage `json:"image"`
	Steps  []string  `json:"steps"`
	Files  int       `json:"files"`
	Values int       `json:"values"`
}

// SealOptions are the arguments of `boxctl seal`.
type SealOptions struct {
	// Name, Version, Parent and Arch name the image the box becomes.
	Name    string
	Version string
	Parent  string
	Arch    string
	// Recipe is the script that makes this environment from the parent image. It
	// goes in on stdin, because it is a file on this host and a script in the
	// guest.
	Recipe []byte
	// Say receives every progress line as it happens.
	Say func(line string)
}

// Seal seals the box into an image, reporting the steps as they happen.
//
// A seal takes minutes: it deletes the work, the sessions and the credentials, and
// then searches the disk for the credential values themselves. boxctl streams one
// JSON document per line for that reason, and this reads them as they arrive.
func (b *Boxctl) Seal(ctx context.Context, opts SealOptions) (*SealResult, error) {
	args := []string{"seal", "--name", opts.Name}
	if opts.Version != "" {
		args = append(args, "--version", opts.Version)
	}
	if opts.Parent != "" {
		args = append(args, "--parent", opts.Parent)
	}
	if opts.Arch != "" {
		args = append(args, "--arch", opts.Arch)
	}
	var stdin io.Reader = strings.NewReader("")
	if len(opts.Recipe) > 0 {
		args = append(args, "--recipe", "-")
		stdin = bytes.NewReader(opts.Recipe)
	}
	stream := &sealStream{say: opts.Say}
	var stderr bytes.Buffer
	code, err := b.tr.Exec(ctx, ExecOpts{Command: append([]string{boxctlPath}, args...),
		Stdin: stdin, Stdout: stream, Stderr: &stderr})
	if err != nil {
		return nil, wrapf(err, "cannot run boxctl seal in the box")
	}
	if msg := stream.errorMessage(); msg != nil {
		return nil, &Error{Code: exitCodeOf(code), Err: msg}
	}
	if code != 0 {
		detail := oneLine(strings.TrimSpace(stderr.String()), 300)
		if detail == "" {
			detail = "no output"
		}
		return nil, errorf("boxctl seal exited with code %d: %s", code, detail)
	}
	if stream.result == nil {
		return nil, errorf("boxctl seal printed no result: the seal is in the box's journal (`box logs`)")
	}
	return stream.result, nil
}

// sealStream reads boxctl seal's line-delimited JSON and reports the steps.
type sealStream struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	say    func(line string)
	result *SealResult
	errMsg *BoxctlError
}

func (s *sealStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// A partial line waits for the rest.
			s.buf.WriteString(line)
			return len(p), nil
		}
		s.handleLine(line)
	}
}

// handleLine reads one document of the stream.
func (s *sealStream) handleLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var doc struct {
		Line  string      `json:"line"`
		Seal  *SealResult `json:"seal"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return
	}
	switch {
	case doc.Error != nil && doc.Error.Message != "":
		s.errMsg = &BoxctlError{Code: doc.Error.Code, Message: doc.Error.Message}
	case doc.Seal != nil:
		s.result = doc.Seal
	case doc.Line != "" && s.say != nil:
		s.say(doc.Line)
	}
}

// errorMessage is the error the seal reported, if it did.
func (s *sealStream) errorMessage() *BoxctlError {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errMsg
}

// exitCodeOf maps boxctl's exit code onto the command's own code.
func exitCodeOf(code int) int {
	if code == 0 {
		return ExitError
	}
	return code
}
