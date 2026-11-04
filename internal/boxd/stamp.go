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
	line = appendPlain(line, p)
	if _, err := s.w.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}

// appendPlain appends p without terminal control sequences: an agent's tool
// output can carry colour codes, and the Nomad UI renders them, so one
// unfinished colour turned every later line of the task log blue. Escape
// sequences go, and so do other control characters but tab and newline.
func appendPlain(dst, p []byte) []byte {
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == 0x1b:
			i = skipEscape(p, i)
		case c == '\n' || c == '\t':
			dst = append(dst, c)
		case c < 0x20 || c == 0x7f:
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// skipEscape returns the index of the last byte of the escape sequence that
// starts at p[i]: CSI (ESC [ ... final byte), OSC (ESC ] ... BEL or ESC \),
// or a two-byte sequence.
func skipEscape(p []byte, i int) int {
	if i+1 >= len(p) {
		return i
	}
	switch p[i+1] {
	case '[':
		for j := i + 2; j < len(p); j++ {
			if p[j] >= 0x40 && p[j] <= 0x7e {
				return j
			}
		}
		return len(p) - 1
	case ']':
		for j := i + 2; j < len(p); j++ {
			if p[j] == 0x07 {
				return j
			}
			if p[j] == 0x1b && j+1 < len(p) && p[j+1] == '\\' {
				return j + 1
			}
		}
		return len(p) - 1
	}
	return i + 1
}
