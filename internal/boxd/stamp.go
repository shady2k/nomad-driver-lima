package boxd

import (
	"io"
	"sync"
	"time"
)

// TimeLayout is how a journal line of boxd starts: UTC, to the second, the way
// a person reads it. The driver's own lines in the task log use it as well.
const TimeLayout = "2006-01-02 15:04:05 UTC"

// stampedWriter starts every write with the time. boxd writes one line per
// call (the log package, journalLine), so a write is a line.
type stampedWriter struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewStampedWriter returns a writer that puts the UTC time in front of every
// line written to w.
func NewStampedWriter(w io.Writer) io.Writer {
	return &stampedWriter{w: w, now: time.Now}
}

func (s *stampedWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := make([]byte, 0, len(TimeLayout)+1+len(p))
	line = s.now().UTC().AppendFormat(line, TimeLayout)
	line = append(line, ' ')
	line = append(line, p...)
	if _, err := s.w.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}
