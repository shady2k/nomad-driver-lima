package boxd

import (
	"bytes"
	"testing"
	"time"
)

// TestStampedWriterDropsTerminalCodes: colour codes from an agent's output do
// not reach the task log, where the Nomad UI would render them.
func TestStampedWriterDropsTerminalCodes(t *testing.T) {
	var b bytes.Buffer
	w := &stampedWriter{w: &b, now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}
	_, _ = w.Write([]byte("tool finished: \x1b[34mblue\x1b[0m \x1b]0;title\x07done\r\x00\tok\n"))
	if got, want := b.String(), "2026-01-02 03:04:05 UTC tool finished: blue done\tok\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
