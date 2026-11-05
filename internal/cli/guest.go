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
	// Seal is whether the seal would run in this box now; a boxd that does not
	// say leaves it out.
	Seal *BoxSealStatus `json:"seal,omitempty"`
}

// BoxSealStatus is `boxctl info`'s answer about the seal: OK, or the reason the
// seal would refuse (a running turn, a preparation in progress).
type BoxSealStatus struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
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
	// Processes are the guest's busiest processes by CPU, most first: box top
	// prints them without a flag. A boxd that cannot read the process table
	// leaves them out.
	Processes []GuestProcess `json:"processes,omitempty"`
	// Hogs are the processes that have used a whole core for ten minutes.
	Hogs []GuestHog `json:"hogs,omitempty"`
	// Leftover is the work running with no turn in progress; a boxd that does
	// not report it leaves it out.
	Leftover *Leftover `json:"leftover,omitempty"`
}

// GuestProcess is one of the guest's busiest processes, as box top shows it: the
// share of one core it used since boxd's last sample, the memory it holds and
// what it runs.
type GuestProcess struct {
	Pid        int     `json:"pid"`
	Command    string  `json:"command"`
	CPUPercent float64 `json:"cpu_percent"`
	MemMiB     uint64  `json:"mem_mib"`
}

// Line is one process the way box top prints it.
func (p GuestProcess) Line() string {
	return fmt.Sprintf("pid %d  %.1f%% of a core  %d MiB  %s", p.Pid, p.CPUPercent, p.MemMiB, p.Command)
}

// GuestHog is a process that has used a whole core for ten minutes or more, as
// boxd measured it. It is what a box that lost a core says, and only that: a long
// compile is a whole core held for ten minutes too.
type GuestHog struct {
	Pid        int     `json:"pid"`
	Command    string  `json:"command"`
	CPUPercent float64 `json:"cpu_percent"`
	ForSeconds int     `json:"for_seconds"`
}

// Line says what was seen: who holds a core, and for how long.
func (h GuestHog) Line() string {
	return fmt.Sprintf("%s (pid %d) has used a whole core for %s", h.Command, h.Pid,
		span(time.Duration(h.ForSeconds)*time.Second))
}

// span is a span the way a person reads it: minutes for the minutes, and
// seconds only below a minute.
func span(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// Leftover is what the guest is running with no turn in progress: work an earlier
// turn, an earlier boxd or a person started, and that nothing is waiting for. boxd
// reports it only while no session runs a turn, and leaves it out otherwise.
type Leftover struct {
	// Count is how many work processes are alive, the ones listed and the rest.
	Count int `json:"count"`
	// Processes are the heaviest of them, in the same lines a turn's report uses.
	Processes []StillRunningProcess `json:"processes"`
	// More is how many processes the lines leave out.
	More int `json:"more,omitempty"`
}

// leftoverShown is how many leftover processes the one line of box status and box
// top names: the line says there are more, and the rest is the count.
const leftoverShown = 3

// Summary is the one line box status and box top print when work is running in
// the guest that no turn is waiting for.
func (l *Leftover) Summary() string {
	if l == nil || l.Count == 0 {
		return ""
	}
	shown := l.Processes
	if len(shown) > leftoverShown {
		shown = shown[:leftoverShown]
	}
	parts := make([]string, 0, len(shown))
	for _, p := range shown {
		parts = append(parts, p.Line())
	}
	s := fmt.Sprintf("%d work process(es) are running with no turn in progress", l.Count)
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, "; ")
	}
	if rest := l.Count - len(shown); rest > 0 {
		s += fmt.Sprintf("; and %d more", rest)
	}
	return s
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
	// StillRunning is what the turn left running in the guest; nil, and left out
	// of the JSON, when there was nothing.
	StillRunning *StillRunning `json:"still_running,omitempty"`
}

// StillRunning is what a finished turn left running in the box: one entry per
// process at the top of its own tree or process group, and how many more there
// were than the lines it holds.
type StillRunning struct {
	Processes []StillRunningProcess `json:"processes"`
	More      int                   `json:"more,omitempty"`
}

// StillRunningProcess is one process a finished turn left running.
type StillRunningProcess struct {
	Pid        int     `json:"pid"`
	Command    string  `json:"command"`
	AgeSeconds int     `json:"age_seconds"`
	CPUSeconds float64 `json:"cpu_seconds"`
	Children   int     `json:"children"`
}

// Count is how many processes a turn left running.
func (s *StillRunning) Count() int {
	if s == nil {
		return 0
	}
	return len(s.Processes) + s.More
}

// Line is one process the way a person reads it: what it runs, how long it has
// run, how much CPU it used and how many processes are in its tree.
func (p StillRunningProcess) Line() string {
	parts := []string{
		fmt.Sprintf("pid %d", p.Pid),
		(time.Duration(p.AgeSeconds) * time.Second).String(),
	}
	if p.Children == 1 {
		parts = append(parts, "1 child")
	} else if p.Children > 1 {
		parts = append(parts, fmt.Sprintf("%d children", p.Children))
	}
	parts = append(parts, fmt.Sprintf("%.1fs CPU", p.CPUSeconds))
	return fmt.Sprintf("%s (%s)", p.Command, strings.Join(parts, ", "))
}

// Lines are the lines of a report, ending with "and N more" when it was cut.
func (s *StillRunning) Lines() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Processes)+1)
	for _, p := range s.Processes {
		out = append(out, p.Line())
	}
	if s.More > 0 {
		out = append(out, fmt.Sprintf("and %d more", s.More))
	}
	return out
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

// EventPage is one page of a session's event feed: the events from the cursor,
// where to resume, and whether the feed holds more than this page.
type EventPage struct {
	Events []Event
	Next   int64
	// Total is how many events there are from the cursor, returned and left.
	Total int
	// More says that the feed holds more events than this page. A boxd that does
	// not report it leaves it false, which reads as "nothing more".
	More bool
}

// Events returns the events of a session from a cursor and the next one.
func (b *Boxctl) Events(ctx context.Context, id string, opts EventsOptions) (EventPage, error) {
	args := []string{"events", id}
	if opts.Since > 0 {
		args = append(args, "--since", strconv.FormatInt(opts.Since, 10))
	}
	if opts.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(opts.Limit))
	}
	out, err := b.call(ctx, nil, args...)
	if err != nil {
		return EventPage{}, err
	}
	resp, err := decodeJSON[struct {
		Events []Event `json:"events"`
		Next   int64   `json:"next"`
		Total  int     `json:"total"`
		More   bool    `json:"more"`
	}](out, "boxctl events")
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{Events: resp.Events, Next: resp.Next, Total: resp.Total, More: resp.More}
	if page.Next == 0 {
		for _, event := range page.Events {
			if event.Seq >= page.Next {
				page.Next = event.Seq + 1
			}
		}
	}
	if page.Total < len(page.Events) {
		page.Total = len(page.Events)
	}
	return page, nil
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
