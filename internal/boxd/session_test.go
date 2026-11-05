package boxd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if s.State != StateIdle {
		t.Fatalf("new session state = %q, want idle", s.State)
	}
	tr, err := m.Send(s.ID, "reply with pong")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tr.Turn != 1 {
		t.Fatalf("turn = %d, want 1", tr.Turn)
	}
	done := waitTurn(t, m, s.ID, 1)
	if done.State != TurnDone {
		t.Fatalf("turn state = %q (%s), want done", done.State, done.Error)
	}
	if !strings.Contains(done.Text, "pong") {
		t.Fatalf("final text = %q, want it to contain pong", done.Text)
	}
	if done.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q", done.StopReason)
	}
	if done.EndedAt == nil {
		t.Fatal("ended_at is missing")
	}
	got, err := m.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutorSession == "" {
		t.Fatal("executor_session (root session id) was not stored")
	}
	if got.Pid == 0 {
		t.Fatal("pid was not recorded")
	}
	if got.State != StateIdle {
		t.Fatalf("state after turn = %q, want idle", got.State)
	}
	evs := allEvents(t, m, s.ID)
	for _, want := range []string{KindTurnStart, KindMessage, KindTurnEnd} {
		if !hasKind(evs, want) {
			t.Fatalf("missing %s event, got %v", want, kinds(evs))
		}
	}
	// The journal and the feed carry the same lines.
	if evs[0].Seq != 1 {
		t.Fatalf("first seq = %d, want 1", evs[0].Seq)
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq not monotonic at %d: %d", i, ev.Seq)
		}
		if ev.Session != s.ID {
			t.Fatalf("event %d session = %q", i, ev.Session)
		}
		if strings.Contains(ev.Summary, "\n") {
			t.Fatalf("event summary is not one line: %q", ev.Summary)
		}
	}
}

func TestSendRejectsBusyAndEmpty(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")

	if _, err := m.Send(s.ID, "   "); err == nil || CodeOf(err) != CodeInvalid {
		t.Fatalf("empty prompt error = %v, want invalid_argument", err)
	}
	if _, err := m.Send(s.ID, "slow:1500"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := m.Send(s.ID, "again"); err == nil || CodeOf(err) != CodeBusy {
		t.Fatalf("second send error = %v, want busy", err)
	}
	// Once the executor is up, the session says it runs the turn, not that it
	// is still starting.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := m.Get(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Pid != 0 {
			if got.State != StateRunning {
				t.Fatalf("state mid-turn with executor pid %d = %q, want running", got.Pid, got.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("executor did not start; state %q", got.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	done := waitTurn(t, m, s.ID, 1)
	if done.State != TurnDone {
		t.Fatalf("slow turn state = %q (%s)", done.State, done.Error)
	}
}

func TestParallelSessions(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	a := mustSession(t, m, "prime", "fake/model-1")
	b := mustSession(t, m, "prime", "fake/model-2")
	if a.ID == b.ID {
		t.Fatal("session ids collide")
	}
	start := time.Now()
	if _, err := m.Send(a.ID, "slow:1200"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(b.ID, "slow:1200"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var ta, tb Turn
	wg.Add(2)
	go func() { defer wg.Done(); ta = waitTurn(t, m, a.ID, 1) }()
	go func() { defer wg.Done(); tb = waitTurn(t, m, b.ID, 1) }()
	wg.Wait()
	elapsed := time.Since(start)
	if ta.State != TurnDone || tb.State != TurnDone {
		t.Fatalf("states = %q/%q (%s / %s)", ta.State, tb.State, ta.Error, tb.Error)
	}
	// Two 1.2 s turns that overlapped cannot take 2.4 s.
	if elapsed > 2200*time.Millisecond {
		t.Fatalf("two parallel turns took %v, want overlap", elapsed)
	}
	ga, _ := m.Get(a.ID)
	gb, _ := m.Get(b.ID)
	if ga.Pid == gb.Pid {
		t.Fatalf("both sessions share pid %d; each session needs its own process", ga.Pid)
	}
}

func TestToolEvents(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "please use a tool"); err != nil {
		t.Fatal(err)
	}
	if tr := waitTurn(t, m, s.ID, 1); tr.State != TurnDone {
		t.Fatalf("state = %q (%s)", tr.State, tr.Error)
	}
	evs := allEvents(t, m, s.ID)
	var tools, results int
	for _, e := range evs {
		switch e.Kind {
		case KindTool:
			tools++
			if e.Data["tool_call_id"] == nil {
				t.Fatalf("tool event has no tool_call_id: %+v", e)
			}
		case KindToolResult:
			results++
			if e.Summary == "" || len(e.Summary) > 300 {
				t.Fatalf("bad tool_result summary %q", e.Summary)
			}
		}
	}
	if tools == 0 || results == 0 {
		t.Fatalf("tool events = %d, results = %d, want both (events: %v)", tools, results, kinds(evs))
	}
}

// TestAFailedTurnCancelsTheExecutorAndSaysItIsActiveOutsideTheTurn: a turn that
// ended in an error from the executor is not a turn the agent goes on writing
// in. boxd asks the executor to cancel, records the updates that still arrive as
// activity outside a turn, says so once in the journal, and says when it went
// quiet again.
func TestAFailedTurnCancelsTheExecutorAndSaysItIsActiveOutsideTheTurn(t *testing.T) {
	cfg := testConfig(t)
	journal := &lockedBuffer{}
	cfg.Journal = journal
	cfg.OutsideTurnQuiet = 300 * time.Millisecond
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "fail-work"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tr := waitTurn(t, m, s.ID, 1); tr.State != TurnError {
		t.Fatalf("turn 1 = %q, want error: %+v", tr.State, tr)
	}

	// The updates that arrive after the error are recorded against the session,
	// with the number of seconds since the last of them.
	var active *int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := m.Get(s.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ExecutorActive != nil {
			active = got.ExecutorActive
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if active == nil {
		t.Fatal("an executor sending updates with no turn running must be reported as active outside a turn")
	}
	if *active < 0 {
		t.Fatalf("executor_active = %d seconds, want a measurement", *active)
	}

	// The events it sent are in the feed with turn 0: nothing pretends they
	// belong to the turn that ended.
	evs := allEvents(t, m, s.ID)
	outside := 0
	for _, ev := range evs {
		if ev.Turn == 0 && ev.Kind == KindTool && strings.Contains(ev.Summary, "make test") {
			outside++
		}
	}
	if outside == 0 {
		t.Fatalf("no event outside a turn in the feed: %v", kinds(evs))
	}

	// Once it stops, the session stops reporting it, and the journal says so.
	for time.Now().Before(deadline.Add(2 * time.Second)) {
		got, err := m.Get(s.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ExecutorActive == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := m.Get(s.ID); got.ExecutorActive != nil {
		t.Fatal("a quiet executor must stop being reported as active outside a turn")
	}
	lines := journal.String()
	if strings.Count(lines, "active outside a turn") != 2 {
		t.Fatalf("the journal must say it once when it starts and once when it stops:\n%s", lines)
	}
	if !strings.Contains(lines, "keeps working") || !strings.Contains(lines, "quiet again") {
		t.Fatalf("the journal lines do not say what happened:\n%s", lines)
	}

	// The executor was asked to cancel: the fake keeps that in its transcript.
	if !transcriptHas(t, cfg, s.ID, `"type":"cancel"`) {
		t.Fatal("boxd did not send session/cancel to an executor whose turn ended in an error")
	}
}

func TestCancelIsAcknowledged(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "hang"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	tr, err := m.Cancel(s.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if tr.State != TurnCancelled {
		t.Fatalf("turn state = %q, want cancelled", tr.State)
	}
	if tr.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q", tr.StopReason)
	}
	got, _ := m.Get(s.ID)
	if got.State != StateIdle {
		t.Fatalf("session state = %q, want idle (executor still alive)", got.State)
	}
	// The same executor keeps working afterwards.
	if _, err := m.Send(s.ID, "say:after cancel"); err != nil {
		t.Fatalf("send after cancel: %v", err)
	}
	if t2 := waitTurn(t, m, s.ID, 2); t2.State != TurnDone {
		t.Fatalf("turn 2 state = %q (%s)", t2.State, t2.Error)
	}
}

func TestCancelWhenExecutorIgnoresIt(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "deaf hang"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	tr, err := m.Cancel(s.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if tr.State != TurnCancelled {
		t.Fatalf("turn state = %q, want cancelled", tr.State)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("cancel took %v", elapsed)
	}
	evs := allEvents(t, m, s.ID)
	if !hasKind(evs, KindExecutorExit) {
		t.Fatalf("expected an executor_exit event, got %v", kinds(evs))
	}
	got, _ := m.Get(s.ID)
	if got.State != StateDead {
		t.Fatalf("session state = %q, want dead after the kill", got.State)
	}
	// The next send resumes the transcript instead of starting fresh.
	if _, err := m.Send(s.ID, "say:resumed"); err != nil {
		t.Fatal(err)
	}
	tr2 := waitTurn(t, m, s.ID, 2)
	if tr2.State != TurnDone {
		t.Fatalf("turn 2 state = %q (%s)", tr2.State, tr2.Error)
	}
	evs = allEvents(t, m, s.ID)
	if !hasKind(evs, KindExecutorRestart) {
		t.Fatalf("expected executor_restart, got %v", kinds(evs))
	}
	argv := readArgvLines(t, m, s.ID)
	if len(argv) < 2 {
		t.Fatalf("expected two executor starts, got %d", len(argv))
	}
	if !strings.Contains(argv[len(argv)-1], "-r") {
		t.Fatalf("resumed start did not pass -r: %s", argv[len(argv)-1])
	}
}

func TestCrashMidTurnThenResume(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "crash now"); err != nil {
		t.Fatal(err)
	}
	tr := waitTurn(t, m, s.ID, 1)
	if tr.State != TurnError {
		t.Fatalf("state = %q, want error", tr.State)
	}
	if tr.Error == "" {
		t.Fatal("error text is empty")
	}
	evs := allEvents(t, m, s.ID)
	if !hasKind(evs, KindExecutorExit) || !hasKind(evs, KindError) {
		t.Fatalf("events = %v, want executor_exit and error", kinds(evs))
	}
	got, _ := m.Get(s.ID)
	if got.ExecutorSession == "" {
		t.Fatal("root session id was lost")
	}
	if got.State != StateDead {
		t.Fatalf("state = %q, want dead", got.State)
	}
	if _, err := m.Send(s.ID, "say:still here"); err != nil {
		t.Fatal(err)
	}
	tr2 := waitTurn(t, m, s.ID, 2)
	if tr2.State != TurnDone {
		t.Fatalf("turn 2 state = %q (%s)", tr2.State, tr2.Error)
	}
	if !strings.Contains(tr2.Text, "still here") {
		t.Fatalf("turn 2 text = %q", tr2.Text)
	}
	evs = allEvents(t, m, s.ID)
	if !hasKind(evs, KindExecutorRestart) {
		t.Fatalf("missing executor_restart: %v", kinds(evs))
	}
	// The resumed process must carry the stored root id.
	lines := readArgvLines(t, m, s.ID)
	last := lines[len(lines)-1]
	if !strings.Contains(last, got.ExecutorSession) {
		t.Fatalf("resume line %q does not carry root id %q", last, got.ExecutorSession)
	}
}

func TestRestartKeepsSessionsResumable(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "say:first"); err != nil {
		t.Fatal(err)
	}
	if tr := waitTurn(t, m, s.ID, 1); tr.State != TurnDone {
		t.Fatalf("turn 1: %q %s", tr.State, tr.Error)
	}
	before, _ := m.Get(s.ID)
	root := before.ExecutorSession
	if root == "" {
		t.Fatal("no root id after turn 1")
	}

	// A second manager over the same state dir is a boxd restart.
	m2 := newManager(t, cfg)
	sessions := m2.List()
	if len(sessions) != 1 {
		t.Fatalf("sessions after restart = %d, want 1", len(sessions))
	}
	if sessions[0].State != StateIdle {
		t.Fatalf("state after restart = %q, want idle", sessions[0].State)
	}
	if sessions[0].Pid != 0 {
		t.Fatalf("pid after restart = %d, want 0", sessions[0].Pid)
	}
	if sessions[0].ExecutorSession != root {
		t.Fatalf("root id after restart = %q, want %q", sessions[0].ExecutorSession, root)
	}
	if _, err := m2.Send(s.ID, "say:after restart"); err != nil {
		t.Fatal(err)
	}
	tr := waitTurn(t, m2, s.ID, 2)
	if tr.State != TurnDone {
		t.Fatalf("turn 2: %q %s", tr.State, tr.Error)
	}
	evs := allEvents(t, m2, s.ID)
	if !hasKind(evs, KindExecutorRestart) {
		t.Fatalf("missing executor_restart after restart: %v", kinds(evs))
	}
	lines := readArgvLines(t, m2, s.ID)
	last := lines[len(lines)-1]
	if !strings.Contains(last, "-r") || !strings.Contains(last, root) {
		t.Fatalf("restart did not resume: %s", last)
	}
}

func TestInterruptedTurnIsClosedOnRestart(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "hang"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// Hard stop without a graceful turn end: the state file still says running.
	m.Shutdown()
	m2 := newManager(t, cfg)
	got, err := m2.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateIdle {
		t.Fatalf("state = %q, want idle", got.State)
	}
	tr, err := m2.Wait(s.ID, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State == TurnRunning {
		t.Fatal("interrupted turn is still running after restart")
	}
}

func TestCloseStopsExecutorKeepsHistory(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "say:hi"); err != nil {
		t.Fatal(err)
	}
	if tr := waitTurn(t, m, s.ID, 1); tr.State != TurnDone {
		t.Fatalf("turn: %q", tr.State)
	}
	before, _ := m.Get(s.ID)
	if err := m.Close(s.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, _ := m.Get(s.ID)
	if after.State != StateClosed || after.Pid != 0 {
		t.Fatalf("after close: state %q pid %d", after.State, after.Pid)
	}
	if after.ExecutorSession != before.ExecutorSession {
		t.Fatal("close dropped the stored root id")
	}
	if _, err := m.Send(s.ID, "more"); err == nil || CodeOf(err) != CodeClosed {
		t.Fatalf("send on closed session error = %v, want session_closed", err)
	}
	procs, err := os.ReadDir("/proc")
	if err == nil {
		for _, p := range procs {
			if p.Name() == "" || !p.IsDir() || p.Name()[0] < '0' || p.Name()[0] > '9' {
				continue
			}
			b, err := os.ReadFile(filepath.Join("/proc", p.Name(), "cmdline"))
			if err != nil {
				continue
			}
			if strings.Contains(string(b), before.ExecutorSession) && strings.Contains(string(b), "--session-dir") {
				t.Fatalf("executor for %s is still running (pid %s)", s.ID, p.Name())
			}
		}
	}
}

func TestInfoDocument(t *testing.T) {
	cfg := testConfig(t)
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Manager().NewSession("prime", "fake/model-1", "", "t"); err != nil {
		t.Fatal(err)
	}
	info := d.Info(t.Context())
	if info.BoxdVersion == "" {
		t.Fatal("no boxd version")
	}
	if info.Sessions != 1 {
		t.Fatalf("sessions = %d, want 1", info.Sessions)
	}
	if info.CPUs == 0 || info.MemTotalMiB == 0 {
		t.Fatalf("resources: cpus=%d mem=%d", info.CPUs, info.MemTotalMiB)
	}
	var names []string
	for _, e := range info.Executors {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "prime,opencode,claude" {
		t.Fatalf("executors = %v", names)
	}
	if !info.Executors[0].Available || info.Executors[0].Version == "" {
		t.Fatalf("prime executor not available: %+v", info.Executors[0])
	}
	// opencode or claude may really be installed on the machine running the
	// tests; what matters is that an unavailable executor explains itself.
	for _, e := range info.Executors[1:] {
		if !e.Available && e.Reason == "" {
			t.Fatalf("%s is unavailable without a reason", e.Name)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	if _, err := m.NewSession("nope", "x/y", "", ""); err == nil || CodeOf(err) != CodeInvalid {
		t.Fatalf("unknown executor error = %v", err)
	}
	if _, err := m.NewSession("prime", "", "", ""); err == nil || CodeOf(err) != CodeInvalid {
		t.Fatalf("missing model error = %v", err)
	}
	if _, err := m.Get("s-nope"); err == nil || CodeOf(err) != CodeNotFound {
		t.Fatalf("unknown session error = %v", err)
	}
	if _, err := m.Send("s-nope", "hi"); err == nil || CodeOf(err) != CodeNotFound {
		t.Fatalf("send to unknown session error = %v", err)
	}
	if _, err := m.Cancel("s-nope"); err == nil || CodeOf(err) != CodeNotFound {
		t.Fatalf("cancel unknown session error = %v", err)
	}
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Wait(s.ID, 1, time.Second); err == nil || CodeOf(err) != CodeNotFound {
		t.Fatalf("wait with no turns error = %v", err)
	}
}

func TestOneLine(t *testing.T) {
	in := "hello\nworld\twith   spaces and a very long tail that should be cut"
	out := oneLine(in, 20)
	if strings.ContainsAny(out, "\n\t") {
		t.Fatalf("newline survived: %q", out)
	}
	if len(out) > 24 {
		t.Fatalf("not truncated: %q", out)
	}
	if oneLine("  a  b  ", 0) != "a b" {
		t.Fatalf("collapse failed: %q", oneLine("  a  b  ", 0))
	}
}

// readArgvLines returns the argv lines the fake executor wrote for a session.
func readArgvLines(t *testing.T, m *Manager, id string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.store.PrimeDir(id), "fake-argv.log"))
	if err != nil {
		t.Fatalf("read fake argv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines
}

// TestTurnReportsWhatItLeftRunning: a turn whose executor left a command
// running says so, and the turn itself stays what the executor said it was --
// done, with its own stop reason. The scan is the boxd one, against this
// machine's /proc, so the report holds whatever else the machine runs; what it
// must hold is the background command the fake executor started.
func TestTurnReportsWhatItLeftRunning(t *testing.T) {
	cfg := testConfig(t)
	journal := &strings.Builder{}
	cfg.Journal = journal
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "leave-bg"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	done := waitTurn(t, m, s.ID, 1)
	if done.State != TurnDone || done.StopReason != "end_turn" {
		t.Fatalf("turn = %q/%q (%s), want done/end_turn: a report must not change the turn", done.State, done.StopReason, done.Error)
	}
	if done.StillRunning == nil || len(done.StillRunning.Processes) == 0 {
		t.Fatal("the turn left a background command running and reported nothing")
	}
	var found *StillRunningProcess
	for i, p := range done.StillRunning.Processes {
		if strings.Contains(p.Command, "sleep 300") {
			found = &done.StillRunning.Processes[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the background command is not in the report: %+v", done.StillRunning.Processes)
	}
	// `sh -c 'sleep 300 & wait'` and the sleep under it are one line with one
	// child, not two lines.
	if found.Children != 1 {
		t.Fatalf("line = %q, want the sleep counted as its child", found.Line())
	}
	if found.Pid == 0 || found.AgeSeconds < 0 || found.CPUSeconds < 0 {
		t.Fatalf("line = %+v, want a pid and a measurement", found)
	}
	// The executor's own program is the box, not the work.
	for _, p := range done.StillRunning.Processes {
		if strings.Contains(p.Command, "fakeacp") {
			t.Fatalf("the report names the executor itself: %+v", done.StillRunning.Processes)
		}
	}
	// The journal says it on its own line, after the turn's own line.
	lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
	finished := -1
	for i, line := range lines {
		if strings.Contains(line, "turn 1") && strings.Contains(line, " finished: ") {
			finished = i
		}
	}
	if finished < 0 {
		t.Fatalf("no finished line in the journal:\n%s", journal.String())
	}
	if finished+1 >= len(lines) {
		t.Fatalf("the finished line has nothing after it:\n%s", journal.String())
	}
	next := lines[finished+1]
	// Whatever else this machine ran in the same cgroup is in the count; what
	// matters here is that the line is the one after the turn's and that it
	// names the background command.
	if !regexp.MustCompile(`^session \S+ turn 1: finished, and \d+ process\(es\) it started are still running: `).MatchString(next) {
		t.Fatalf("the line after the turn's is %q", next)
	}
	if !strings.Contains(next, "sleep 300") {
		t.Fatalf("the line after the turn's does not name the command: %q", next)
	}
	// The turn is persisted like every other turn: the field survives a reload.
	if b, err := os.ReadFile(filepath.Join(cfg.StateDir, "state.json")); err != nil {
		t.Fatalf("read state: %v", err)
	} else if !strings.Contains(string(b), `"still_running"`) {
		t.Fatalf("state.json does not carry still_running:\n%s", b)
	}
	// Nothing is left behind for the rest of the run.
	t.Cleanup(func() {
		_ = syscall.Kill(-found.Pid, syscall.SIGKILL)
		_ = syscall.Kill(found.Pid, syscall.SIGKILL)
	})
}

// TestTurnWithoutLeftoversHasNoField: a turn that leaves nothing running has no
// field at all, so a person reads "done" and nothing else. The scan reads a tree
// of this test's own, because the /proc of a shared machine has other work in it
// and this is about the turn that started none.
func TestTurnWithoutLeftoversHasNoField(t *testing.T) {
	cfg := testConfig(t)
	f := newFakeProc(t, time.Now().UTC())
	cfg.ProcRoot = f.root
	journal := &strings.Builder{}
	cfg.Journal = journal
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "fake/model-1")
	if _, err := m.Send(s.ID, "reply with pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	done := waitTurn(t, m, s.ID, 1)
	if done.State != TurnDone {
		t.Fatalf("turn = %q (%s)", done.State, done.Error)
	}
	if done.StillRunning != nil {
		t.Fatalf("a turn that left nothing reports %+v", done.StillRunning)
	}
	if strings.Contains(journal.String(), "still running") {
		t.Fatalf("the journal says something is still running:\n%s", journal.String())
	}
	// A turn whose end could not be scanned keeps the field empty too, and says
	// so once in the log.
	cfg2 := testConfig(t)
	cfg2.ProcRoot = filepath.Join(t.TempDir(), "no-such-proc")
	var logged []string
	cfg2.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m2 := newManager(t, cfg2)
	s2 := mustSession(t, m2, "prime", "fake/model-1")
	if _, err := m2.Send(s2.ID, "reply with pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if done := waitTurn(t, m2, s2.ID, 1); done.State != TurnDone || done.StillRunning != nil {
		t.Fatalf("turn with a failed scan = %q/%+v, want done and no report", done.State, done.StillRunning)
	}
	var failures []string
	for _, line := range logged {
		if strings.Contains(line, "process table") {
			failures = append(failures, line)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("the failed scan was logged %d times, want once: %v", len(failures), logged)
	}
}
