package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Nomad's logmon may open its FIFO reader only after RecoverTask returns. Never
// perform a blocking FIFO open/write in that RPC. The guest remains the durable
// spool; monitor advances each stream offset only for bytes actually delivered.
const taskLogWriteBudget = 250 * time.Millisecond

type taskLog struct {
	ctx    context.Context
	path   string
	mu     sync.Mutex
	writes sync.Mutex
	file   *os.File
	closed bool
}

func newTaskLog(ctx context.Context, path string) *taskLog {
	w := &taskLog{ctx: ctx, path: path}
	// Best-effort nonblocking connection preserves an existing startup guard's
	// FIFO writer continuity. Absence of a reader is never an attach failure.
	_ = w.openLocked()
	return w
}
func (w *taskLog) openLocked() error {
	if w.closed {
		return os.ErrClosed
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.file != nil {
		return nil
	}
	st, err := os.Lstat(w.path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() && st.Mode()&os.ModeNamedPipe == 0 && w.path != os.DevNull {
		return fmt.Errorf("unsupported native log endpoint")
	}
	root, err := os.OpenRoot(filepath.Dir(w.path))
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(w.path), os.O_WRONLY|os.O_APPEND|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(st, actual) {
		f.Close()
		return fmt.Errorf("native log endpoint changed")
	}
	w.file = f
	return nil
}
func waitLogRetry(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (w *taskLog) waitOpen(ctx context.Context) error {
	for {
		w.mu.Lock()
		err := w.openLocked()
		w.mu.Unlock()
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.ENXIO) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = waitLogRetry(ctx); err != nil {
			return err
		}
	}
}
func (w *taskLog) Write(data []byte) (int, error) {
	if len(data) == 0 {
		w.mu.Lock()
		err := w.openLocked()
		w.mu.Unlock()
		if errors.Is(err, syscall.ENXIO) || errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	w.writes.Lock()
	defer w.writes.Unlock()
	ctx, cancel := context.WithTimeout(w.ctx, taskLogWriteBudget)
	defer cancel()
	delivered := 0
	for delivered < len(data) {
		if err := ctx.Err(); err != nil {
			return delivered, fmt.Errorf("native log reader/backpressure pending: %w", err)
		}
		w.mu.Lock()
		err := w.openLocked()
		n := 0
		if err == nil {
			// File.Write can enter Go's poller and wait without our deadline. Raw write
			// through SyscallConn retains O_NONBLOCK and allows bounded cancellation.
			var raw syscall.RawConn
			raw, err = w.file.SyscallConn()
			if err == nil {
				var writeErr error
				err = raw.Control(func(fd uintptr) { n, writeErr = syscall.Write(int(fd), data[delivered:]) })
				if err == nil {
					err = writeErr
				}
			}
			if errors.Is(err, syscall.EPIPE) {
				w.file.Close()
				w.file = nil
			}
		}
		closed := w.closed
		w.mu.Unlock()
		if n > 0 {
			delivered += n
		}
		if delivered == len(data) {
			return delivered, nil
		}
		if closed {
			return delivered, os.ErrClosed
		}
		if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) && !errors.Is(err, syscall.ENXIO) && !errors.Is(err, syscall.EPIPE) && !errors.Is(err, os.ErrNotExist) {
			return delivered, fmt.Errorf("native log unavailable: %w", err)
		}
		if err = waitLogRetry(ctx); err != nil {
			return delivered, fmt.Errorf("native log reader/backpressure pending: %w", err)
		}
	}
	return delivered, nil
}
func (w *taskLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

var _ io.WriteCloser = (*taskLog)(nil)
