// Package boxd implements the box guest daemon (boxd) and its control client
// (boxctl): one executor process per agent session, a session store that
// survives restarts, and the JSON contract the box CLI drives from the host.
package boxd

import (
	"fmt"
	"time"
)

// Session states (contract field "state").
const (
	StateIdle     = "idle"
	StateRunning  = "running"
	StateStarting = "starting"
	StateDead     = "dead"
	StateClosed   = "closed"
)

// Turn states (contract field "state").
const (
	TurnRunning   = "running"
	TurnDone      = "done"
	TurnCancelled = "cancelled"
	TurnError     = "error"
)

// Event kinds (contract field "kind").
const (
	KindTurnStart       = "turn_start"
	KindMessage         = "message"
	KindTool            = "tool"
	KindToolResult      = "tool_result"
	KindTurnEnd         = "turn_end"
	KindError           = "error"
	KindExecutorExit    = "executor_exit"
	KindExecutorRestart = "executor_restart"
)

// Session is the contract Session object: the durable description of one agent
// session inside this box.
type Session struct {
	ID              string    `json:"id"`
	Executor        string    `json:"executor"`
	Model           string    `json:"model"`
	Cwd             string    `json:"cwd"`
	Title           string    `json:"title"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	LastTurn        int       `json:"last_turn"`
	ExecutorSession string    `json:"executor_session"`
	Pid             int       `json:"pid"`
}

// Turn is the contract Turn object: one prompt turn of a session.
type Turn struct {
	Session    string     `json:"session"`
	Turn       int        `json:"turn"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
	Text       string     `json:"text"`
	Error      string     `json:"error,omitempty"`
}

// Event is the contract Event object: one line of the activity feed.
type Event struct {
	Seq     int64          `json:"seq"`
	TS      time.Time      `json:"ts"`
	Session string         `json:"session"`
	Turn    int            `json:"turn"`
	Kind    string         `json:"kind"`
	Summary string         `json:"summary"`
	Data    map[string]any `json:"data,omitempty"`
}

// ExecutorInfo is one entry of the "executors" array returned by "boxctl info".
type ExecutorInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
	// Configured reports whether the executor's credentials and configuration
	// are in the secrets directory. It is nil for an executor whose binary is
	// missing, where availability already says everything.
	Configured *bool  `json:"configured,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// PlatformInfo is the versioned platform bundle mounted at /opt/box: what the
// node downloaded and mounted, as the guest sees it.
type PlatformInfo struct {
	Path    string            `json:"path"`
	Mounted bool              `json:"mounted"`
	Version string            `json:"version,omitempty"`
	Boxd    string            `json:"boxd,omitempty"`
	Tools   map[string]string `json:"tools,omitempty"`
	Reason  string            `json:"reason,omitempty"`
}

// Info is the result of "boxctl info".
type Info struct {
	BoxdVersion string         `json:"boxd_version"`
	Executors   []ExecutorInfo `json:"executors"`
	Sessions    int            `json:"sessions"`
	MemTotalMiB int            `json:"mem_total_mib"`
	MemFreeMiB  int            `json:"mem_free_mib"`
	CPUs        int            `json:"cpus"`
	// Prepare is where the box's preparation is.
	Prepare *PrepareState `json:"prepare,omitempty"`
	// Usage is the guest's load, measured every few seconds.
	Usage *Usage `json:"usage,omitempty"`
	// Platform is the bundle the box mounts at /opt/box; it is always present,
	// with Mounted false and a reason when there is none.
	Platform PlatformInfo `json:"platform"`
}

// ErrorBody is the failure document boxctl prints to stdout and exits 1 with.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse wraps ErrorBody in the wire shape {"error":{...}}.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// Common error codes shared by boxd and boxctl.
const (
	CodeBoxdUnavailable = "boxd_unavailable"
	CodeBusy            = "busy"
	CodeNotFound        = "not_found"
	CodeInvalid         = "invalid_argument"
	CodeClosed          = "session_closed"
	CodeExecutor        = "executor_error"
	CodeInternal        = "internal"
	CodeTimeout         = "timeout"
)

// Error is a coded error carried over the boxd HTTP API.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds a coded error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf reports the code of err, or CodeInternal for untyped errors.
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInternal
}
