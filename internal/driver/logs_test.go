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
