package boxd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/boxd/acp"
)

// DaemonConfig is everything boxd needs to run.
type DaemonConfig struct {
	Socket     string // unix socket path (default /run/boxd/boxd.sock)
	StateDir   string // durable state: sessions and transcripts (default /var/lib/boxd)
	RuntimeDir string // executor configuration for this boot (default /run/boxd)
	// SecretsDir is where the Nomad driver puts the executor credentials. The
	// driver copies them onto the guest's tmpfs, never onto its disk.
	SecretsDir string    // (default /run/box/secrets)
	LocalDir   string    // files the driver copies in (default /etc/box/local)
	PrimeAgent string    // prime-agent binary (default: PATH)
	OpenCode   string    // opencode binary (default: PATH)
	ClaudeACP  string    // claude-code-acp binary (default: PATH)
	Journal    io.Writer // event feed destination (stdout in the guest)
	// ProgressEvery is how long a running turn may be silent in the journal
	// before it writes a progress line (default two minutes).
	ProgressEvery time.Duration
	Logf          func(string, ...any)
	Now           func() time.Time
	CancelGrace   time.Duration // how long a cancel waits for the executor to agree
	StartTimeout  time.Duration // initialize + session/new budget
	TurnLimit     time.Duration // hard ceiling on one turn (0 = none)
}

func (c DaemonConfig) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c DaemonConfig) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return now()
}

// primeConfigDir is the prime agent directory boxd hands the executor through
// PRIME_AGENT_CODING_AGENT_DIR: the mutable copy of its secrets, in the runtime
// directory, which is a tmpfs. It is never on the box disk.
func (c DaemonConfig) primeConfigDir() string {
	d := filepath.Join(c.RuntimeDir, "executors", "prime")
	if dirExists(d) && !isEmptyDir(d) {
		return d
	}
	return ""
}

// executorsDir is where boxd keeps the mutable copy of every executor's secrets
// and each executor's XDG_CONFIG_HOME, on the tmpfs. HOMEs are on the disk
// (executorDirs).
func (c DaemonConfig) executorsDir() string { return filepath.Join(c.RuntimeDir, "executors") }

// Defaults fills in every unset field.
func (c DaemonConfig) withDefaults() DaemonConfig {
	if c.Socket == "" {
		c.Socket = "/run/boxd/boxd.sock"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/boxd"
	}
	if c.RuntimeDir == "" {
		c.RuntimeDir = "/run/boxd"
	}
	if c.SecretsDir == "" {
		c.SecretsDir = "/run/box/secrets"
	}
	if c.LocalDir == "" {
		c.LocalDir = "/etc/box/local"
	}
	if c.PrimeAgent == "" {
		c.PrimeAgent = "prime-agent"
	}
	if c.OpenCode == "" {
		c.OpenCode = "opencode"
	}
	if c.ClaudeACP == "" {
		c.ClaudeACP = "claude-code-acp"
	}
	if c.CancelGrace == 0 {
		c.CancelGrace = 5 * time.Second
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = 180 * time.Second
	}
	return c
}

// Manager owns every session of the box.
type Manager struct {
	cfg   DaemonConfig
	store *Store

	mu       sync.Mutex
	sessions map[string]*session
	order    []string
}

// NewManager loads the persisted sessions and prepares the state directory.
func NewManager(cfg DaemonConfig) (*Manager, error) {
	cfg = cfg.withDefaults()
	if err := prepareExecutorDirs(cfg); err != nil {
		cfg.logf("executor secrets: %v", err)
	}
	store, err := NewStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, store: store, sessions: map[string]*session{}}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) load() error {
	st, err := m.store.Load()
	if err != nil {
		return err
	}
	for _, ps := range st.Sessions {
		s := &session{
			m:    m,
			meta: ps.Session,
			log: &eventLog{
				store:   m.store,
				journal: m.cfg.Journal,
				session: ps.ID,
				seq:     m.store.LastSeq(ps.ID),
				now:     m.cfg.now,
			},
		}
		for _, t := range ps.Turns {
			ts := &turnState{Turn: t, done: make(chan struct{})}
			if t.State != TurnRunning {
				close(ts.done)
			}
			s.turns = append(s.turns, ts)
		}
		// No executor survives a boxd restart: every session is resumable but idle.
		switch s.meta.State {
		case StateClosed:
			s.closed = true
		default:
			s.meta.State = StateIdle
		}
		s.meta.Pid = 0
		for _, ts := range s.turns {
			if ts.State == TurnRunning {
				ts.State = TurnError
				ts.Error = "interrupted by boxd restart"
				ended := m.cfg.now()
				ts.EndedAt = &ended
				if s.meta.ExecutorSession != "" {
					s.meta.State = StateIdle
				}
				s.log.emit(KindTurnEnd, ts.Turn.Turn, "turn interrupted by boxd restart", map[string]any{
					"state": TurnError,
				})
				close(ts.done)
			}
		}
		m.sessions[s.meta.ID] = s
		m.order = append(m.order, s.meta.ID)
	}
	if len(st.Sessions) > 0 {
		m.cfg.logf("loaded %d session(s) from %s", len(st.Sessions), m.cfg.StateDir)
	}
	return nil
}

// persist writes every session's metadata. Callers hold no lock: the snapshot is
// taken under the manager lock and each session's own lock.
func (m *Manager) persist(_ *session) {
	m.mu.Lock()
	st := &persistedState{}
	for _, id := range m.order {
		sess := m.sessions[id]
		sess.mu.Lock()
		ps := persistedSession{Session: sess.meta}
		turns := make([]Turn, 0, len(sess.turns))
		for _, t := range sess.turns {
			turns = append(turns, t.Turn)
		}
		sess.mu.Unlock()
		if len(turns) > maxPersistedTurns {
			turns = turns[len(turns)-maxPersistedTurns:]
		}
		ps.Turns = turns
		st.Sessions = append(st.Sessions, ps)
	}
	m.mu.Unlock()
	if err := m.store.Save(st); err != nil {
		m.cfg.logf("save state: %v", err)
	}
}

const maxPersistedTurns = 50

// ----------------------------------------------------------------- session ---

type session struct {
	m     *Manager
	log   *eventLog
	meta  Session // guarded by mu
	turns []*turnState

	mu     sync.Mutex
	proc   *proc
	busy   bool
	closed bool
	spawnM sync.Mutex
	cur    *turnState
	// reloadPending is set when new executor credentials arrived while a turn was
	// running; the executor is restarted when the turn ends (see finishTurn).
	reloadPending bool
}

type turnState struct {
	Turn

	mu        sync.Mutex
	done      chan struct{}
	finishOne sync.Once
	cancelled bool
	acc       strings.Builder
	flushed   int
}

// takeTail returns the assistant text not yet reported as a message event.
func (t *turnState) takeTail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	all := t.acc.String()
	if t.flushed >= len(all) {
		return ""
	}
	tail := strings.TrimSpace(all[t.flushed:])
	t.flushed = len(all)
	return tail
}

func (t *turnState) appendText(s string) {
	t.mu.Lock()
	t.acc.WriteString(s)
	t.mu.Unlock()
}

func (t *turnState) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acc.String()
}

func (t *turnState) markCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := t.cancelled
	t.cancelled = true
	return !was
}

func (t *turnState) isCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

// proc is one live executor process.
type proc struct {
	name    string
	client  *acp.Client
	pid     int
	logFile *os.File
	session string // ACP session id
	rootID  string // executor transcript id ("" when the adapter keeps none)
	// watched is closed when the exit handler has recorded the exit, so a
	// shutdown can wait for the last state write.
	watched chan struct{}
}

// ------------------------------------------------------------------ events ---

type eventLog struct {
	mu      sync.Mutex
	store   *Store
	journal io.Writer
	session string
	seq     int64
	now     func() time.Time
	// last is the latest event, which the progress line of a running turn
	// names; saidAt is when the journal last heard of the session.
	last   Event
	saidAt time.Time
}

func (l *eventLog) emit(kind string, turn int, summary string, data map[string]any) {
	l.mu.Lock()
	l.seq++
	ev := Event{
		Seq:     l.seq,
		TS:      l.now(),
		Session: l.session,
		Turn:    turn,
		Kind:    kind,
		Summary: oneLine(summary, 300),
		Data:    data,
	}
	l.last = ev
	l.saidAt = ev.TS
	l.mu.Unlock()
	// The feed file is best effort: an event that cannot be appended must not
	// fail the call, and the journal line below still goes out.
	if l.store != nil {
		_ = l.store.AppendEvent(l.session, ev)
	}
	l.journalLine(turn, kindLabel(kind), ev.Summary)
}

// journalLine writes one line for a person to the journal, which is what the
// box's task log in Nomad shows: the session, the turn and what happened. The
// feed file keeps the structured event; the journal is for reading, best effort
// like the feed: a line that cannot be written is dropped.
func (l *eventLog) journalLine(turn int, what, summary string) {
	if l.journal == nil {
		return
	}
	line := fmt.Sprintf("session %s", l.session)
	if turn > 0 {
		line += fmt.Sprintf(" turn %d", turn)
	}
	// A summary that names the turn already ("turn 3 started: ...") needs no
	// label in front of it.
	switch {
	case summary == "":
		line += ": " + what
	case strings.HasPrefix(summary, "turn "):
		line += ": " + strings.TrimPrefix(summary, fmt.Sprintf("turn %d ", turn))
	default:
		line += ": " + what + ": " + summary
	}
	_, _ = io.WriteString(l.journal, line+"\n")
}

// progress writes the line that says a turn is still running, how long it has
// run and what it did last, when the session has been silent for quiet: a turn
// whose agent is busy calling tools says enough by itself.
func (l *eventLog) progress(turn int, started time.Time, quiet time.Duration) {
	l.mu.Lock()
	last := l.last
	said := l.saidAt
	l.mu.Unlock()
	now := l.now()
	if now.Sub(said) < quiet {
		return
	}
	l.mu.Lock()
	l.saidAt = now
	l.mu.Unlock()
	summary := fmt.Sprintf("running for %s", now.Sub(started).Round(time.Second))
	if last.Turn == turn && last.Kind != KindTurnStart && last.Summary != "" {
		summary += fmt.Sprintf("; last, %s ago, %s: %s", now.Sub(last.TS).Round(time.Second),
			kindLabel(last.Kind), oneLine(last.Summary, 160))
	} else {
		summary += "; waiting for the model's first answer"
	}
	l.journalLine(turn, "still working", summary)
}

// kindLabel is an event kind in the words of a journal line.
func kindLabel(kind string) string {
	switch kind {
	case KindTurnStart:
		return "started"
	case KindMessage:
		return "the agent says"
	case KindTool:
		return "tool"
	case KindToolResult:
		return "tool finished"
	case KindTurnEnd:
		return "finished"
	case KindExecutorExit:
		return "executor exited"
	case KindExecutorRestart:
		return "executor restarted"
	default:
		return kind
	}
}

// oneLine collapses whitespace and truncates, so an event summary can never
// carry a prompt or a tool body in full.
func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		cut := max
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}

// utf8Start reports whether b starts a UTF-8 sequence, so truncation never
// splits a rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// -------------------------------------------------------------------- API ----

// NewSession records a session. The executor process starts on the first send,
// so an idle session costs nothing.
func (m *Manager) NewSession(executor, model, cwd, title string) (Session, error) {
	// The driver copies the task files (and with them /run/box/secrets) after
	// boxd has started, so the executor directories are checked again here: a
	// session is the first moment boxd can see them.
	if err := prepareExecutorDirs(m.cfg); err != nil {
		m.cfg.logf("executor secrets: %v", err)
	}
	if _, err := buildExecutor(executor, m.cfg); err != nil {
		return Session{}, err
	}
	if model == "" {
		return Session{}, Errorf(CodeInvalid, "a model is required (--model <provider>/<model>)")
	}
	if cwd == "" {
		cwd = filepath.Join(m.cfg.StateDir, "work")
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return Session{}, Errorf(CodeInvalid, "cwd %q: %v", cwd, err)
	}
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}
	now := m.cfg.now()
	s := &session{
		m:   m,
		log: &eventLog{store: m.store, journal: m.cfg.Journal, session: id, now: m.cfg.now},
		meta: Session{
			ID:        id,
			Executor:  executor,
			Model:     model,
			Cwd:       cwd,
			Title:     title,
			State:     StateIdle,
			CreatedAt: now,
		},
	}
	if err := os.MkdirAll(m.store.PrimeDir(id), 0o700); err != nil {
		return Session{}, err
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.order = append(m.order, id)
	m.mu.Unlock()
	m.persist(s)
	return s.meta, nil
}

func newSessionID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "s-" + hex.EncodeToString(b[:]), nil
}

// List returns every session in creation order.
func (m *Manager) List() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.order))
	for _, id := range m.order {
		s := m.sessions[id]
		s.mu.Lock()
		out = append(out, s.meta)
		s.mu.Unlock()
	}
	return out
}

// Reload re-reads the executor credentials and refreshes the runtime directory.
//
// This is how a running box gets new executor credentials without being
// recreated: the operator updates the Nomad Variable, Nomad re-renders the
// task's secrets, the box driver copies them onto the guest's tmpfs again and
// calls this. Idle executors are stopped so the next turn starts with the new
// configuration; a session with a turn running is left alone and its executor
// restarts after the turn, so a reload never kills work in flight.
func (m *Manager) Reload() (ReloadResult, error) {
	if err := prepareExecutorDirs(m.cfg); err != nil {
		return ReloadResult{}, Errorf(CodeInternal, "reload the executor credentials: %v", err)
	}
	res := ReloadResult{Configured: []string{}}
	for _, s := range m.List() {
		if s.State == StateClosed {
			continue
		}
		prep := m.lookupSession(s.ID)
		if prep == nil {
			continue
		}
		stopped, pending := prep.reloadExecutor()
		if stopped {
			res.Restarted = append(res.Restarted, s.ID)
		}
		if pending {
			res.Deferred = append(res.Deferred, s.ID)
		}
	}
	for _, name := range []string{"prime", "opencode", "claude"} {
		ex, err := buildExecutor(name, m.cfg)
		if err != nil {
			continue
		}
		if ex.Configured() {
			res.Configured = append(res.Configured, name)
		}
	}
	return res, nil
}

// ReloadResult reports what a reload did.
type ReloadResult struct {
	// Configured lists the executors that have credentials now.
	Configured []string `json:"configured"`
	// Restarted lists the sessions whose idle executor was stopped, so the next
	// turn starts it with the new configuration.
	Restarted []string `json:"restarted"`
	// Deferred lists the sessions that are running a turn: their executor
	// restarts after the turn.
	Deferred []string `json:"deferred"`
}

// lookupSession returns a session by id, or nil.
func (m *Manager) lookupSession(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Count is the number of sessions on this box, including closed ones (so the
// number matches "session ls").
func (m *Manager) Count() int { return len(m.List()) }

func (m *Manager) lookup(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, Errorf(CodeNotFound, "no session %q", id)
	}
	return s, nil
}

// Get returns one session.
func (m *Manager) Get(id string) (Session, error) {
	s, err := m.lookup(id)
	if err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta, nil
}

// Send starts one turn and returns immediately.
func (m *Manager) Send(id, text string) (Turn, error) {
	s, err := m.lookup(id)
	if err != nil {
		return Turn{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Turn{}, Errorf(CodeInvalid, "empty prompt")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Turn{}, Errorf(CodeClosed, "session %s is closed", id)
	}
	if s.busy {
		s.mu.Unlock()
		return Turn{}, Errorf(CodeBusy, "session %s already has a turn running", id)
	}
	n := s.meta.LastTurn + 1
	ts := &turnState{Turn: Turn{Session: id, Turn: n, State: TurnRunning, StartedAt: m.cfg.now()}, done: make(chan struct{})}
	s.turns = append(s.turns, ts)
	s.cur = ts
	s.busy = true
	s.meta.LastTurn = n
	s.meta.State = StateRunning
	turn := ts.Turn
	s.mu.Unlock()

	m.persist(s)
	go s.runTurn(ts, text)
	return turn, nil
}

// runTurn is the body of one turn.
func (s *session) runTurn(ts *turnState, prompt string) {
	m := s.m
	s.log.emit(KindTurnStart, ts.Turn.Turn, fmt.Sprintf("turn %d started: %s", ts.Turn.Turn, oneLine(prompt, 80)), map[string]any{
		"prompt_chars": len(prompt),
	})
	p, resumed, err := s.ensureExecutor()
	if err != nil {
		if ts.isCancelled() {
			s.finishTurn(ts, TurnCancelled, "cancelled", "", "cancelled before the executor started")
			return
		}
		s.finishTurn(ts, TurnError, "", "", "executor: "+err.Error())
		return
	}
	if resumed {
		s.log.emit(KindExecutorRestart, ts.Turn.Turn, "resumed executor session "+p.rootID, map[string]any{
			"executor_session": p.rootID,
			"pid":              p.pid,
		})
	}
	if ts.isCancelled() {
		_ = p.client.Cancel(p.session)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.reportProgress(ctx, ts.Turn.Turn)
	if m.cfg.TurnLimit > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, m.cfg.TurnLimit)
		defer stop()
	}
	res, err := p.client.Prompt(ctx, p.session, prompt)

	text := ts.text()
	if err != nil {
		if ts.isCancelled() {
			s.finishTurn(ts, TurnCancelled, "cancelled", text, "cancelled")
			return
		}
		s.finishTurn(ts, TurnError, "", text, promptErrorSentence(err, p))
		return
	}
	stop := res.StopReason
	state := TurnDone
	switch {
	case stop == "cancelled" || ts.isCancelled():
		state = TurnCancelled
	}
	s.finishTurn(ts, state, stop, text, "")
}

// reportProgress writes a progress line whenever the session has been silent
// for ProgressEvery, until ctx ends, so the task log says a long turn is alive
// and what it last did.
func (s *session) reportProgress(ctx context.Context, turn int) {
	every := s.m.cfg.ProgressEvery
	if every <= 0 {
		every = 2 * time.Minute
	}
	started := s.m.cfg.now()
	ticker := time.NewTicker(every / 4)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.log.progress(turn, started, every)
		}
	}
}

func promptErrorSentence(err error, p *proc) string {
	var rpc *acp.RPCError
	if errors.As(err, &rpc) {
		return fmt.Sprintf("executor error: %s", oneLine(rpc.Error(), 200))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "turn timed out"
	}
	return fmt.Sprintf("executor stopped: %s (pid %d)", oneLine(err.Error(), 160), p.pid)
}

// finishTurn closes a turn exactly once and publishes its result.
func (s *session) finishTurn(ts *turnState, state, stopReason, text, errMsg string) {
	ts.finishOne.Do(func() {
		text = strings.TrimSpace(text)
		if tail := ts.takeTail(); tail != "" {
			s.log.emit(KindMessage, ts.Turn.Turn, oneLine(tail, 240), map[string]any{
				"chars": len(tail),
			})
		}
		if state == TurnError && errMsg != "" {
			s.log.emit(KindError, ts.Turn.Turn, errMsg, nil)
		}
		if state == TurnCancelled && ts.isCancelled() {
			stopReason = "cancelled"
		}
		ended := s.m.cfg.now()
		s.mu.Lock()
		ts.State = state
		ts.StopReason = stopReason
		ts.Text = text
		ts.Error = errMsg
		ts.EndedAt = &ended
		s.busy = false
		if s.cur == ts {
			s.cur = nil
		}
		if !s.closed {
			if s.proc != nil {
				s.meta.State = StateIdle
			} else {
				s.meta.State = StateDead
			}
		}
		cur := ts.Turn
		s.mu.Unlock()

		summary := fmt.Sprintf("turn %d %s", cur.Turn, state)
		switch {
		case state == TurnDone && strings.TrimSpace(text) != "":
			summary = oneLine(text, 160)
		case state == TurnError && errMsg != "":
			summary = fmt.Sprintf("turn %d error: %s", cur.Turn, oneLine(errMsg, 160))
		case state == TurnCancelled:
			summary = fmt.Sprintf("turn %d cancelled", cur.Turn)
		}
		s.log.emit(KindTurnEnd, cur.Turn, summary, map[string]any{
			"state":       state,
			"stop_reason": stopReason,
		})
		s.m.persist(s)
		close(ts.done)
	})
	// A reload that arrived while this turn was running restarts the executor
	// now, so the next turn uses the new credentials.
	if s.takeReloadPending() {
		s.stopIdleExecutor("reload")
	}
}

// Wait blocks until the requested turn (0 = the latest) finishes or the timeout
// expires. A timed-out wait returns the running turn, not an error.
func (m *Manager) Wait(id string, turn int, timeout time.Duration) (Turn, error) {
	s, err := m.lookup(id)
	if err != nil {
		return Turn{}, err
	}
	s.mu.Lock()
	ts := s.findTurnLocked(turn)
	var done chan struct{}
	if ts != nil {
		done = ts.done
	}
	s.mu.Unlock()
	if ts == nil {
		return Turn{}, Errorf(CodeNotFound, "session %s has no turn %d yet", id, turn)
	}
	if timeout <= 0 {
		<-done
	} else {
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return ts.Turn, nil
}

func (s *session) findTurnLocked(turn int) *turnState {
	if len(s.turns) == 0 {
		return nil
	}
	if turn <= 0 {
		return s.turns[len(s.turns)-1]
	}
	for _, ts := range s.turns {
		if ts.Turn.Turn == turn {
			return ts
		}
	}
	return nil
}

// Events returns the session's event feed after a cursor.
func (m *Manager) Events(id string, since int64, limit int) ([]Event, int64, error) {
	if _, err := m.lookup(id); err != nil {
		return nil, since, err
	}
	return m.store.ReadEvents(id, since, limit)
}

// Cancel asks the executor to stop the running turn; when the executor ignores
// the request the process is killed, which loses only the in-flight turn (the
// transcript is on disk).
func (m *Manager) Cancel(id string) (Turn, error) {
	s, err := m.lookup(id)
	if err != nil {
		return Turn{}, err
	}
	s.mu.Lock()
	ts := s.cur
	if ts == nil || !s.busy {
		last := (*turnState)(nil)
		if len(s.turns) > 0 {
			last = s.turns[len(s.turns)-1]
		}
		s.mu.Unlock()
		if last == nil {
			return Turn{}, Errorf(CodeNotFound, "session %s has no turns", id)
		}
		turn := last.Turn
		turn.Session = id
		return turn, nil
	}
	p := s.proc
	acpSession := ""
	if p != nil {
		acpSession = p.session
	}
	s.mu.Unlock()

	ts.markCancelled()
	if p != nil {
		_ = p.client.Cancel(acpSession)
	}
	grace := m.cfg.CancelGrace
	select {
	case <-ts.done:
	case <-time.After(grace):
		s.mu.Lock()
		p := s.proc
		s.mu.Unlock()
		s.log.emit(KindError, ts.Turn.Turn, "executor did not acknowledge the cancel; killing the process", map[string]any{
			"grace_seconds": int(grace.Seconds()),
		})
		if p != nil {
			p.client.Kill()
		}
		select {
		case <-ts.done:
		case <-time.After(10 * time.Second):
			s.finishTurn(ts, TurnCancelled, "cancelled", ts.text(), "cancelled after the executor ignored cancel")
		}
	}
	s.mu.Lock()
	t := ts.Turn
	s.mu.Unlock()
	return t, nil
}

// Close stops the executor and keeps the history.
func (m *Manager) Close(id string) error {
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.meta.State = StateClosed
	s.meta.Pid = 0
	p := s.proc
	s.proc = nil
	ts := s.cur
	s.mu.Unlock()
	if ts != nil {
		// Closing mid-turn reports "cancelled", not "the executor died".
		ts.markCancelled()
	}

	if p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.client.CloseSession(ctx, p.session)
		cancel()
		p.stop(5 * time.Second)
	}
	if ts != nil {
		s.finishTurn(ts, TurnCancelled, "cancelled", ts.text(), "session closed")
	}
	m.persist(s)
	s.log.emit(KindTurnEnd, s.meta.LastTurn, "session closed", nil)
	return nil
}

// Shutdown stops every executor (boxd is going down; state stays resumable).
func (m *Manager) Shutdown() {
	sessions := m.List()
	for _, meta := range sessions {
		s, err := m.lookup(meta.ID)
		if err != nil {
			continue
		}
		s.mu.Lock()
		p := s.proc
		s.proc = nil
		s.meta.Pid = 0
		ts := s.cur
		if !s.closed {
			s.meta.State = StateIdle
		}
		s.mu.Unlock()
		if ts != nil {
			s.finishTurn(ts, TurnError, "", ts.text(), "boxd stopped")
		}
		if p != nil {
			p.stop(2 * time.Second)
		}
		m.persist(s)
	}
}

// reloadExecutor stops an idle executor so the next turn starts it with the
// configuration now in the runtime directory. A session with a turn running keeps
// its executor until the turn ends.
func (s *session) reloadExecutor() (stopped, deferred bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false, false
	}
	if s.busy {
		s.reloadPending = true
		s.mu.Unlock()
		return false, true
	}
	p := s.proc
	s.proc = nil
	s.meta.Pid = 0
	s.meta.State = StateIdle
	s.mu.Unlock()
	if p == nil {
		return false, false
	}
	p.stop(5 * time.Second)
	s.m.persist(s)
	return true, false
}

// takeReloadPending consumes the flag finishTurn set while a turn ran.
func (s *session) takeReloadPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.reloadPending
	s.reloadPending = false
	return pending
}

// stopIdleExecutor stops the executor when no turn is running. It is what a
// reload does once the running turn is over.
func (s *session) stopIdleExecutor(reason string) {
	s.mu.Lock()
	if s.busy || s.closed {
		s.reloadPending = true
		s.mu.Unlock()
		return
	}
	p := s.proc
	s.proc = nil
	s.meta.Pid = 0
	if s.meta.State != StateClosed {
		s.meta.State = StateDead
	}
	s.mu.Unlock()
	if p != nil {
		p.stop(5 * time.Second)
		s.log.emit(KindExecutorExit, s.meta.LastTurn, "executor stopped: "+reason, map[string]any{"reason": reason})
	}
	s.m.persist(s)
}

// ------------------------------------------------------------- executor ------

// ensureExecutor makes sure the session has a live executor process with an
// open ACP session. It resumes from the stored transcript instead of starting a
// fresh session when history exists.
func (s *session) ensureExecutor() (*proc, bool, error) {
	s.spawnM.Lock()
	defer s.spawnM.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, false, Errorf(CodeClosed, "session closed")
	}
	if p := s.proc; p != nil {
		select {
		case <-p.client.Done():
			// dead, fall through and respawn
		default:
			s.mu.Unlock()
			return p, false, nil
		}
	}
	s.meta.State = StateStarting
	meta := s.meta
	s.mu.Unlock()
	s.m.persist(s)

	if err := os.MkdirAll(s.m.store.PrimeDir(meta.ID), 0o700); err != nil {
		return nil, false, err
	}
	adapter, err := buildExecutor(meta.Executor, s.m.cfg)
	if err != nil {
		return nil, false, err
	}
	spec := ExecSpec{
		SessionID:    meta.ID,
		Model:        meta.Model,
		Cwd:          meta.Cwd,
		SessionDir:   s.m.store.PrimeDir(meta.ID),
		ResumeRootID: meta.ExecutorSession,
	}
	before := adapter.TranscriptIDs(spec.SessionDir)
	argv, env, err := adapter.Argv(spec)
	if err != nil {
		return nil, false, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = spec.Cwd
	// Own process group: killing a session kills the agent and its children,
	// and nothing else.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logFile, err := openExecutorLog(s.m.store.ExecutorLog(meta.ID))
	if err != nil {
		return nil, false, err
	}
	cmd.Stderr = logFile
	client, err := acp.NewClient(cmd)
	if err != nil {
		logFile.Close()
		return nil, false, err
	}
	if err := client.Start(); err != nil {
		logFile.Close()
		return nil, false, Errorf(CodeExecutor, "start %s: %v", meta.Executor, err)
	}
	s.m.cfg.logf("session %s: started %s pid %d", meta.ID, argv[0], client.Pid())

	startCtx, cancel := context.WithTimeout(context.Background(), s.m.cfg.StartTimeout)
	defer cancel()
	resumed := meta.ExecutorSession != ""
	if _, err := client.Initialize(startCtx, "boxd/1"); err != nil {
		client.Kill()
		client.Shutdown(2 * time.Second)
		logFile.Close()
		return nil, false, Errorf(CodeExecutor, "initialize %s: %v", meta.Executor, oneLine(err.Error(), 200))
	}
	ns, err := client.NewSession(startCtx, spec.Cwd)
	if err != nil {
		client.Kill()
		client.Shutdown(2 * time.Second)
		logFile.Close()
		return nil, false, Errorf(CodeExecutor, "session/new: %v", oneLine(err.Error(), 200))
	}

	p := &proc{name: meta.Executor, client: client, pid: client.Pid(), logFile: logFile, session: ns.SessionID, rootID: meta.ExecutorSession, watched: make(chan struct{})}
	if p.rootID == "" {
		p.rootID = discoverRootID(adapter, spec.SessionDir, before)
	}

	// Adapters other than prime pick the model through their config option; the
	// prime adapter gets it on the command line.
	if meta.Executor != "prime" && len(ns.ConfigOptions) > 0 && meta.Model != "" {
		var opts []configOption
		for _, o := range ns.ConfigOptions {
			opts = append(opts, configOption{ID: o.ID, Name: o.Name, CurrentValue: o.CurrentValue})
		}
		if id, value, ok := modelOption(opts, meta.Model); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := client.Call(ctx, "session/set_config_option", map[string]any{
				"sessionId": ns.SessionID, "configId": id, "value": value,
			})
			cancel()
			if err != nil {
				s.log.emit(KindError, 0, fmt.Sprintf("could not select model %s through the adapter: %s", meta.Model, oneLine(err.Error(), 120)), nil)
			}
		}
	}

	client.OnUpdate(s.handleUpdate)
	client.OnRequest(s.handleAgentRequest)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		client.Kill()
		client.Shutdown(2 * time.Second)
		logFile.Close()
		return nil, false, Errorf(CodeClosed, "session closed while starting")
	}
	s.proc = p
	s.meta.Pid = p.pid
	s.meta.ExecutorSession = p.rootID
	s.mu.Unlock()
	s.m.persist(s)
	go s.watchExit(p)
	return p, resumed && p.rootID != "", nil
}

// watchExit records an executor exit exactly once.
func (s *session) watchExit(p *proc) {
	defer close(p.watched)
	<-p.client.Done()
	<-p.client.Reaped()
	code := p.client.ExitCode()

	s.mu.Lock()
	current := s.proc
	if current == p {
		s.proc = nil
		s.meta.Pid = 0
		if !s.closed {
			s.meta.State = StateDead
		}
	}
	s.mu.Unlock()
	p.closeLog()

	turn := 0
	if cur := s.curTurn(); cur != nil {
		turn = cur.Turn.Turn
	}
	s.log.emit(KindExecutorExit, turn, fmt.Sprintf("%s executor exited with code %d", p.name, code), map[string]any{
		"pid":       p.pid,
		"exit_code": code,
	})
	s.m.persist(s)
}

func (s *session) curTurn() *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// updateParams is one ACP session/update notification.
type updateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string          `json:"sessionUpdate"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Kind          string          `json:"kind"`
		Status        string          `json:"status"`
		Content       json.RawMessage `json:"content"`
	} `json:"update"`
}

// contentText pulls plain text out of ACP content, which is either one content
// block ({"type":"text","text":...}) or an array of them, whose text may sit in
// a nested "content" block.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	type block struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Name    string `json:"name"`
		Content *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	pick := func(b block) string {
		if b.Text != "" {
			return b.Text
		}
		if b.Content != nil && b.Content.Text != "" {
			return b.Content.Text
		}
		return ""
	}
	var one block
	if err := json.Unmarshal(raw, &one); err == nil && (one.Text != "" || one.Content != nil) {
		return pick(one)
	}
	var many []block
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, b := range many {
			if t := pick(b); t != "" {
				return t
			}
		}
	}
	return ""
}

// handleUpdate maps ACP session/update notifications to contract events:
// assistant text becomes "message", tool calls become "tool" and "tool_result".
func (s *session) handleUpdate(params json.RawMessage) {
	var p updateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	ts := s.curTurn()
	turn := 0
	if ts != nil {
		turn = ts.Turn.Turn
	}
	u := p.Update
	data := map[string]any{"update": u.SessionUpdate}
	if u.ToolCallID != "" {
		data["tool_call_id"] = u.ToolCallID
	}
	if u.Kind != "" {
		data["tool_kind"] = u.Kind
	}
	if u.Status != "" {
		data["status"] = u.Status
	}
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if ts != nil {
			ts.appendText(contentText(u.Content))
		}
	case "tool_call":
		s.flushText(ts)
		title := u.Title
		if title == "" {
			title = "tool call"
		}
		s.log.emit(KindTool, turn, title, data)
	case "tool_call_update":
		if u.Status == "completed" || u.Status == "failed" {
			summary := "tool finished"
			if u.Status == "failed" {
				summary = "tool failed"
			}
			if t := contentText(u.Content); t != "" {
				summary = t
			}
			s.log.emit(KindToolResult, turn, summary, data)
		}
	}
}

// flushText emits the assistant text collected since the last flush, so the
// reading order of text and tool calls is preserved.
func (s *session) flushText(ts *turnState) {
	if ts == nil {
		return
	}
	if tail := ts.takeTail(); tail != "" {
		s.log.emit(KindMessage, ts.Turn.Turn, oneLine(tail, 240), map[string]any{"chars": len(tail)})
	}
}

// handleAgentRequest answers permission requests: a box has no human to ask, so
// boxd picks the first "allow" option, which is what a headless agent run needs.
func (s *session) handleAgentRequest(method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, fmt.Errorf("unsupported agent request %q", method)
	}
	var p struct {
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &p)
	choice := ""
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, "allow") {
			choice = o.OptionID
			break
		}
	}
	if choice == "" && len(p.Options) > 0 {
		choice = p.Options[0].OptionID
	}
	turn := 0
	if ts := s.curTurn(); ts != nil {
		turn = ts.Turn.Turn
	}
	s.log.emit(KindTool, turn, "permission request: "+oneLine(p.ToolCall.Title, 120), map[string]any{
		"permission": "auto",
		"option":     choice,
	})
	if choice == "" {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": choice}}, nil
}

// discoverRootID finds the transcript a freshly started executor created.
func discoverRootID(adapter Executor, dir string, before []string) string {
	seen := map[string]bool{}
	for _, id := range before {
		seen[id] = true
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fresh []string
		for _, id := range adapter.TranscriptIDs(dir) {
			if !seen[id] {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) > 0 {
			sort.Sort(sort.Reverse(sort.StringSlice(fresh)))
			return fresh[0]
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openExecutorLog opens the executor's stderr file, trimmed when it is big.
func openExecutorLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > 1<<20 {
		_ = os.Remove(path)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// stop terminates the executor and waits until its exit has been recorded.
func (p *proc) stop(grace time.Duration) {
	p.client.Shutdown(grace)
	select {
	case <-p.watched:
	case <-time.After(10 * time.Second):
	}
	p.closeLog()
}

func (p *proc) closeLog() {
	if p.logFile != nil {
		_ = p.logFile.Close()
		p.logFile = nil
	}
}

// configOption is the part of an ACP config option boxd uses.
type configOption struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	CurrentValue string `json:"currentValue"`
}
