package driver

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestLineWriterKeepsLinesWhole: a note written while the guest's journal is
// in the middle of a line goes after that line, not inside it.
func TestLineWriterKeepsLinesWhole(t *testing.T) {
	var out bytes.Buffer
	lw := &lineWriter{w: &out}
	_, _ = io.WriteString(lw, "journal: half")
	_, _ = io.WriteString(lw, "box exec: started: true\n")
	_, _ = io.WriteString(lw, " a line\njournal: next")
	lw.flush()
	want := "journal: halfbox exec: started: true\n a line\njournal: next\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Lines written as whole lines stay whole and in order.
	out.Reset()
	lw = &lineWriter{w: &out}
	_, _ = io.WriteString(lw, "one\n")
	_, _ = io.WriteString(lw, "two\nthree\n")
	if got := strings.Split(strings.TrimSpace(out.String()), "\n"); len(got) != 3 {
		t.Fatalf("lines = %q", got)
	}
}

// TestQuietExecIsOnlyTheReadingBoxctlCalls: the CLI's polling stays out of the
// task log; whatever changes something stays in it.
func TestQuietExecIsOnlyTheReadingBoxctlCalls(t *testing.T) {
	b := "/usr/local/bin/boxctl"
	for _, c := range [][]string{{b, "info"}, {b, "wait", "s-1"}, {b, "events", "s-1"}, {b, "session", "ls"},
		{b, "session", "get", "s-1"}, {b, "prepare", "wait", "--timeout", "30"}, {b, "prepare", "get"}} {
		if !quietExec(c) {
			t.Errorf("%v should stay out of the task log", c)
		}
	}
	for _, c := range [][]string{{b, "session", "new"}, {b, "send", "s-1"}, {b, "cancel", "s-1"}, {b, "prepare", "start"},
		{b, "prepare", "cancel"}, {b, "reload"}, {"git", "clone", "x"}, {"tar", "-xf", "-"}, {"boxctl", "info"}} {
		if quietExec(c) {
			t.Errorf("%v should be in the task log", c)
		}
	}
}
