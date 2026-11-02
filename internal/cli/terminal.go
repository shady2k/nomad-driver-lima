package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/hashicorp/nomad/api"
	"golang.org/x/term"
)

// Terminal is the local terminal `box ssh` uses.
//
// It is an interface so a test can run the ssh command path without a real
// terminal, and so the command can tell a terminal from a pipe.
type Terminal interface {
	// IsInteractive reports whether stdin is a terminal.
	IsInteractive() bool
	// MakeRaw puts the terminal into raw mode and returns the restore function.
	MakeRaw() (restore func(), err error)
	// Sizes reports terminal size changes, starting with the current size.
	Sizes() <-chan api.TerminalSize
}

// OSTerminal is the Terminal of the process, wired to the real stdin/stdout.
type OSTerminal struct {
	In  *os.File
	Out *os.File
}

// NewOSTerminal returns a Terminal for the given files, defaulting to
// stdin/stdout when they are nil.
func NewOSTerminal(in, out *os.File) *OSTerminal {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	return &OSTerminal{In: in, Out: out}
}

// IsInteractive reports whether stdin is a terminal.
func (t *OSTerminal) IsInteractive() bool {
	return term.IsTerminal(int(t.In.Fd()))
}

// MakeRaw puts the terminal into raw mode.
func (t *OSTerminal) MakeRaw() (func(), error) {
	fd := int(t.In.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}, err
	}
	return func() { _ = term.Restore(fd, state) }, nil
}

// Sizes reports the terminal size now and on every SIGWINCH.
func (t *OSTerminal) Sizes() <-chan api.TerminalSize {
	ch := make(chan api.TerminalSize, 1)
	send := func() {
		width, height, err := term.GetSize(int(t.Out.Fd()))
		if err != nil || width <= 0 || height <= 0 {
			return
		}
		select {
		case ch <- api.TerminalSize{Width: width, Height: height}:
		default:
		}
	}
	send()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	go func() {
		for range signals {
			send()
		}
	}()
	return ch
}
