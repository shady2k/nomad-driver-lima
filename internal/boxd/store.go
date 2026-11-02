package boxd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Layout inside the state directory (default /var/lib/boxd):
//
//	state.json                     all sessions and their recent turns
//	boxd.pid                       pid of the running daemon
//	sessions/<id>/events.jsonl     the session's event feed
//	sessions/<id>/executor.log     executor stderr (truncated)
//	sessions/<id>/prime/           prime's transcript dir (--session-dir)
type Store struct {
	dir string
	mu  sync.Mutex
}

// SessionDir is the per-session state directory.
func (s *Store) SessionDir(id string) string { return filepath.Join(s.dir, "sessions", id) }

// PrimeDir is the prime transcript directory of a session.
func (s *Store) PrimeDir(id string) string { return filepath.Join(s.SessionDir(id), "prime") }

// ExecutorLog is the file that receives the executor's stderr.
func (s *Store) ExecutorLog(id string) string { return filepath.Join(s.SessionDir(id), "executor.log") }

// EventsFile is the session's JSONL event feed.
func (s *Store) EventsFile(id string) string { return filepath.Join(s.SessionDir(id), "events.jsonl") }

// Dir is the state directory root.
func (s *Store) Dir() string { return s.dir }

// NewStore prepares the state directory.
func NewStore(dir string) (*Store, error) {
	// Nothing secret is created here: the mutable executor configuration lives in
	// the runtime directory, on the guest's tmpfs (see prepareExecutorDirs).
	for _, d := range []string{dir, filepath.Join(dir, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

// persistedTurn is a Turn plus nothing extra; kept for symmetry and future fields.
type persistedSession struct {
	Session
	Turns []Turn `json:"turns,omitempty"`
}

type persistedState struct {
	Version  int                `json:"version"`
	Sessions []persistedSession `json:"sessions"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

// Load reads the persisted state. A missing file is an empty state.
func (s *Store) Load() (*persistedState, error) {
	b, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return &persistedState{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state.json: %w", err)
	}
	return &st, nil
}

// Save writes the state atomically.
func (s *Store) Save(st *persistedState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Version = 1
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

// AppendEvent appends one event line to the session's feed.
func (s *Store) AppendEvent(id string, ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.SessionDir(id), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.EventsFile(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadEvents returns up to limit events with seq > since, and the cursor to use
// next (highest returned seq + 1, or since when nothing is new).
func (s *Store) ReadEvents(id string, since int64, limit int) ([]Event, int64, error) {
	f, err := os.Open(s.EventsFile(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, since, nil
	}
	if err != nil {
		return nil, since, err
	}
	defer f.Close()
	if limit <= 0 {
		limit = 100
	}
	var out []Event
	next := since
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Seq <= since {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, ev)
		next = ev.Seq + 1
	}
	if err := sc.Err(); err != nil {
		return out, next, err
	}
	return out, next, nil
}

// LastSeq reports the highest event sequence number stored for a session.
func (s *Store) LastSeq(id string) int64 {
	f, err := os.Open(s.EventsFile(id))
	if err != nil {
		return 0
	}
	defer f.Close()
	var last int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err == nil && ev.Seq > last {
			last = ev.Seq
		}
	}
	return last
}

// SessionIDs lists state directories that hold a session feed, so an orphaned
// directory (state.json lost) can be reported instead of silently reused.
func (s *Store) SessionIDs() []string {
	ents, err := os.ReadDir(filepath.Join(s.dir, "sessions"))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// now is the daemon clock, swappable in tests.
func now() time.Time { return time.Now().UTC() }
