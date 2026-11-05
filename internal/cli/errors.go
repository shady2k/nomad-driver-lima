// Package cli implements the coordinator-side `box` command.
//
// The command speaks only to the Nomad API (and to the image index over HTTP),
// so Nomad is the single source of truth: no local state is kept. Every guest
// call goes through `nomad alloc exec` of the guest's /usr/local/bin/boxctl, as
// described in docs/boxctl-contract.md.
package cli

import (
	"errors"
	"fmt"
	"strings"
)

// Exit codes of the command, as specified in docs/cli.md.
const (
	// ExitOK is a successful run.
	ExitOK = 0
	// ExitError is any failure the user can act on.
	ExitError = 1
	// ExitUsage is a malformed command line or a missing argument.
	ExitUsage = 2
	// ExitNotFound is a box, session or file that does not exist.
	ExitNotFound = 3
	// ExitTimeout is a wait that ran out while the work is still going on.
	ExitTimeout = 4
	// ExitUnknown is a command that lost contact with the box for good: the turn
	// or the command it was following may still run, so its result is not known.
	// It is neither a failure nor a timeout, because neither is true.
	ExitUnknown = 5
)

// Error is a failure with an exit code and a message meant for the user.
//
// Messages are written in plain words and, where it helps, say what to do
// next: the coordinator agent reads them and acts on them.
type Error struct {
	// Code is the process exit code.
	Code int
	// Msg is the plain-words message.
	Msg string
	// Err is the underlying failure, if any. It is included in the message so
	// a single line always explains what happened.
	Err error
}

func (e *Error) Error() string {
	switch {
	case e.Msg == "" && e.Err == nil:
		return fmt.Sprintf("box: failed (exit %d)", e.Code)
	case e.Msg == "":
		return e.Err.Error()
	case e.Err == nil:
		return e.Msg
	default:
		return e.Msg + ": " + e.Err.Error()
	}
}

func (e *Error) Unwrap() error { return e.Err }

// errorf returns a plain failure (exit 1).
func errorf(format string, args ...any) *Error {
	return &Error{Code: ExitError, Msg: fmt.Sprintf(format, args...)}
}

// wrapf returns a plain failure that carries a cause.
func wrapf(err error, format string, args ...any) *Error {
	return &Error{Code: ExitError, Msg: fmt.Sprintf(format, args...), Err: err}
}

// wrapKeep wraps an error and keeps its exit code, so a timeout stays a
// timeout when the command adds a sentence to it.
func wrapKeep(err error, format string, args ...any) *Error {
	return &Error{Code: ExitCode(err), Msg: fmt.Sprintf(format, args...), Err: err}
}

// usagef returns a usage failure (exit 2).
func usagef(format string, args ...any) *Error {
	return &Error{Code: ExitUsage, Msg: fmt.Sprintf(format, args...)}
}

// notFoundf returns a not-found failure (exit 3).
func notFoundf(format string, args ...any) *Error {
	return &Error{Code: ExitNotFound, Msg: fmt.Sprintf(format, args...)}
}

// timeoutf returns a timeout failure (exit 4).
func timeoutf(format string, args ...any) *Error {
	return &Error{Code: ExitTimeout, Msg: fmt.Sprintf(format, args...)}
}

// partialf returns a failure that did part of its work (exit 1, as a failure):
// the sentence says what happened and what is left to do.
func partialf(format string, args ...any) *Error {
	return &Error{Code: ExitError, Msg: fmt.Sprintf(format, args...)}
}

// unknownf returns a lost-contact failure (exit 5): the work may still go on.
func unknownf(format string, args ...any) *Error {
	return &Error{Code: ExitUnknown, Msg: fmt.Sprintf(format, args...)}
}

// ExitCode reports the process exit code for err: one of the Exit* constants.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ExitError
}

// errOr renders an error for a sentence that carries it, or nothing at all.
func errOr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// joinNotes joins the sentences of a note, skipping the empty ones.
func joinNotes(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// oneLine collapses whitespace and truncates s, so an error message that came
// from a remote command stays readable on a single line.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if limit > 0 && len(s) > limit {
		s = s[:limit-1] + "\u2026"
	}
	return s
}
