package boxd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// boxdProc is one boxd daemon process started from the built binary.
type boxdProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	journal *syncBuffer // event lines of every process (they accumulate)
	stderr  *syncBuffer
	bin     string
	socket  string
	state   string
	fake    string
	root    string
}

type syncBuffer struct {
	mu  chan struct{}
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer {
	s := &syncBuffer{mu: make(chan struct{}, 1)}
	s.mu <- struct{}{}
	return s
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	return s.buf.String()
}

// startBoxd starts (or restarts) the daemon and waits for its socket.
func startBoxd(t *testing.T, b *boxdProc) {
	t.Helper()
	if b.journal == nil {
		b.journal = newSyncBuffer()
	}
	b.stderr = newSyncBuffer()
	cmd := exec.Command(b.bin,
		"-socket", b.socket,
		"-state-dir", b.state,
		"-secrets-dir", filepath.Join(b.root, "secrets"),
		"-local-dir", filepath.Join(b.root, "local"),
		"-prime-agent", b.fake,
	)
	cmd.Stdout = b.journal
	cmd.Stderr = b.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start boxd: %v", err)
	}
	b.cmd = cmd
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(b.socket); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("boxd did not create %s; stderr: %s", b.socket, b.stderr.String())
}

// stop sends SIGTERM and waits for the process.
func (b *boxdProc) stop() {
	if b.cmd == nil || b.cmd.Process == nil {
		return
	}
	_ = b.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = b.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	_ = os.Remove(b.socket)
	b.cmd = nil
}

// boxctlResult is one invocation of the control client.
type boxctlResult struct {
	status int
	stdout string
	stderr string
}

// doc parses the single JSON document on stdout.
func (r boxctlResult) doc(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &m); err != nil {
		t.Fatalf("stdout is not one JSON document (%v): %q", err, r.stdout)
	}
	return m
}

// run invokes boxctl through the "boxctl" symlink (argv[0] dispatch).
func (b *boxdProc) run(t *testing.T, stdin string, args ...string) boxctlResult {
	t.Helper()
	cmd := exec.Command(filepath.Join(b.root, "boxctl"), append([]string{"--socket", b.socket}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	status := 0
	if ee, ok := err.(*exec.ExitError); ok {
		status = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("boxctl %v: %v", args, err)
	}
	res := boxctlResult{status: status, stdout: strings.TrimSpace(out.String()), stderr: errb.String()}
	if strings.Count(res.stdout, "\n") != 0 {
		t.Fatalf("boxctl printed more than one document: %q", res.stdout)
	}
	return res
}

// ok runs boxctl and requires exit 0.
func (b *boxdProc) ok(t *testing.T, stdin string, args ...string) map[string]any {
	t.Helper()
	res := b.run(t, stdin, args...)
	if res.status != 0 {
		t.Fatalf("boxctl %v exited %d: %s %s", args, res.status, res.stdout, res.stderr)
	}
	return res.doc(t)
}

// TestEndToEnd drives the real boxd and boxctl binaries over a unix socket with
// the fake ACP executor, covering every contract command.
func TestEndToEnd(t *testing.T) {
	root := t.TempDir()
	repo, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boxdBin := filepath.Join(binDir, "boxd")
	build := exec.Command("go", "build", "-o", boxdBin, "./cmd/boxd")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build boxd: %v\n%s", err, out)
	}
	// The same binary is boxctl when argv[0] says so.
	if err := os.Symlink(boxdBin, filepath.Join(root, "boxctl")); err != nil {
		t.Fatal(err)
	}
	b := &boxdProc{
		t:      t,
		bin:    boxdBin,
		socket: filepath.Join(root, "run", "boxd.sock"),
		state:  filepath.Join(root, "state"),
		fake:   buildFake(t),
		root:   root,
	}
	if err := os.MkdirAll(filepath.Dir(b.socket), 0o755); err != nil {
		t.Fatal(err)
	}
	startBoxd(t, b)
	t.Cleanup(func() { b.stop() })

	// --- info ---------------------------------------------------------------
	info := b.ok(t, "", "info")
	if info["boxd_version"] == nil || info["boxd_version"] == "" {
		t.Fatalf("info has no boxd_version: %v", info)
	}
	if n, _ := info["sessions"].(float64); n != 0 {
		t.Fatalf("sessions = %v, want 0", info["sessions"])
	}
	if n, _ := info["cpus"].(float64); n == 0 {
		t.Fatal("info reports 0 cpus")
	}
	if n, _ := info["mem_total_mib"].(float64); n == 0 {
		t.Fatal("info reports 0 memory")
	}
	execs, _ := info["executors"].([]any)
	if len(execs) != 3 {
		t.Fatalf("executors = %v", info["executors"])
	}

	// --- new, ls, get -------------------------------------------------------
	sid, _ := b.ok(t, "", "session", "new", "--executor", "prime", "--model", "fake/model-1", "--title", "e2e")["session"].(string)
	if sid == "" {
		t.Fatal("session new returned no id")
	}
	sess := b.ok(t, "", "session", "get", sid)
	if sess["id"] != sid || sess["state"] != StateIdle || sess["executor"] != "prime" || sess["model"] != "fake/model-1" {
		t.Fatalf("session = %v", sess)
	}
	if sess["created_at"] == nil {
		t.Fatal("session has no created_at")
	}
	ls := b.ok(t, "", "session", "ls")
	if got, _ := ls["sessions"].([]any); len(got) != 1 {
		t.Fatalf("session ls = %v", ls)
	}

	// --- send, wait, events -------------------------------------------------
	if got := b.ok(t, "reply with pong", "send", sid); got["turn"] != float64(1) || got["session"] != sid {
		t.Fatalf("send = %v", got)
	}
	w := b.ok(t, "", "wait", sid, "--timeout", "20")
	if w["state"] != TurnDone {
		t.Fatalf("wait = %v", w)
	}
	if text, _ := w["text"].(string); !strings.Contains(text, "pong") {
		t.Fatalf("turn text = %q", w["text"])
	}
	if w["stop_reason"] != "end_turn" || w["ended_at"] == nil {
		t.Fatalf("turn = %v", w)
	}
	ev := b.ok(t, "", "events", sid, "--since", "0")
	next, _ := ev["next"].(float64)
	events, _ := ev["events"].([]any)
	if len(events) == 0 || next == 0 {
		t.Fatalf("events = %v", ev)
	}
	seen := map[string]bool{}
	for i, raw := range events {
		e := raw.(map[string]any)
		if int(e["seq"].(float64)) != i+1 {
			t.Fatalf("seq %v at index %d", e["seq"], i)
		}
		if e["session"] != sid {
			t.Fatalf("event session = %v", e["session"])
		}
		seen[e["kind"].(string)] = true
	}
	for _, want := range []string{KindTurnStart, KindMessage, KindTurnEnd} {
		if !seen[want] {
			t.Fatalf("no %s event: %v", want, ev["events"])
		}
	}
	// The cursor does not repeat events.
	ev2 := b.ok(t, "", "events", sid, "--since", fmt.Sprint(int(next)))
	if got, _ := ev2["events"].([]any); len(got) != 0 {
		t.Fatalf("cursor repeated events: %v", got)
	}
	// limit works.
	ev3 := b.ok(t, "", "events", sid, "--since", "0", "--limit", "2")
	if got, _ := ev3["events"].([]any); len(got) != 2 {
		t.Fatalf("limit ignored: %v", ev3["events"])
	}

	sess = b.ok(t, "", "session", "get", sid)
	rootID, _ := sess["executor_session"].(string)
	if rootID == "" {
		t.Fatalf("session has no executor_session: %v", sess)
	}
	if pidf, _ := sess["pid"].(float64); pidf == 0 {
		t.Fatalf("session has no pid: %v", sess)
	}

	// --- two sessions in parallel ------------------------------------------
	s1, _ := b.ok(t, "", "session", "new", "--executor", "prime", "--model", "fake/model-1")["session"].(string)
	s2, _ := b.ok(t, "", "session", "new", "--executor", "prime", "--model", "fake/model-2")["session"].(string)
	b.ok(t, "slow:1500", "send", s1)
	b.ok(t, "slow:1500", "send", s2)
	t0 := time.Now()
	t1 := b.ok(t, "", "wait", s1, "--turn", "1", "--timeout", "30")
	t2 := b.ok(t, "", "wait", s2, "--turn", "1", "--timeout", "30")
	if t1["state"] != TurnDone || t2["state"] != TurnDone {
		t.Fatalf("parallel turns: %v / %v", t1, t2)
	}
	if elapsed := time.Since(t0); elapsed > 2500*time.Millisecond {
		t.Fatalf("parallel turns took %v, want overlap", elapsed)
	}

	// --- busy ---------------------------------------------------------------
	b.ok(t, "slow:1200", "send", s1)
	busy := b.run(t, "again", "send", s1)
	if busy.status != 1 {
		t.Fatalf("busy send exited %d", busy.status)
	}
	if code := errorCode(t, busy); code != CodeBusy {
		t.Fatalf("busy send error code = %q", code)
	}
	b.ok(t, "", "wait", s1, "--turn", "2", "--timeout", "30")

	// --- wait timeout returns the running turn with exit 0 ------------------
	b.ok(t, "slow:2500", "send", s2)
	partial := b.ok(t, "", "wait", s2, "--turn", "2", "--timeout", "1")
	if partial["state"] != TurnRunning {
		t.Fatalf("wait with a short timeout = %v", partial)
	}
	if partial["ended_at"] != nil {
		t.Fatalf("running turn has ended_at: %v", partial)
	}
	if done := b.ok(t, "", "wait", s2, "--turn", "2", "--timeout", "30"); done["state"] != TurnDone {
		t.Fatalf("turn after timeout = %v", done)
	}
	// The whole wait honours --turn: turn 1 is already finished.
	if got := b.ok(t, "", "wait", s2, "--turn", "1", "--timeout", "5"); got["state"] != TurnDone || got["turn"] != float64(1) {
		t.Fatalf("wait --turn 1 = %v", got)
	}

	// --- cancel -------------------------------------------------------------
	b.ok(t, "hang", "send", s1)
	time.Sleep(300 * time.Millisecond)
	cancel := b.ok(t, "", "cancel", s1)
	if cancel["state"] != TurnCancelled {
		t.Fatalf("cancel = %v", cancel)
	}
	// wait on a cancelled turn returns immediately with state cancelled
	if w := b.ok(t, "", "wait", s1, "--turn", "3", "--timeout", "5"); w["state"] != TurnCancelled {
		t.Fatalf("wait after cancel = %v", w)
	}

	// --- crash -> resume ----------------------------------------------------
	crash, _ := b.ok(t, "", "session", "new", "--executor", "prime", "--model", "fake/model-1")["session"].(string)
	b.ok(t, "crash now", "send", crash)
	ct := b.ok(t, "", "wait", crash, "--timeout", "20")
	if ct["state"] != TurnError {
		t.Fatalf("crashed turn = %v", ct)
	}
	cs := b.ok(t, "", "session", "get", crash)
	if cs["state"] != StateDead {
		t.Fatalf("session after crash = %v", cs)
	}
	crashRoot, _ := cs["executor_session"].(string)
	b.ok(t, "say:back again", "send", crash)
	ct2 := b.ok(t, "", "wait", crash, "--turn", "2", "--timeout", "20")
	if ct2["state"] != TurnDone || !strings.Contains(fmt.Sprint(ct2["text"]), "back again") {
		t.Fatalf("resumed turn = %v", ct2)
	}
	evs := b.ok(t, "", "events", crash, "--since", "0")
	if !strings.Contains(fmt.Sprint(evs["events"]), KindExecutorRestart) {
		t.Fatalf("no executor_restart event: %v", evs["events"])
	}
	argv := argvLines(t, b, crash)
	if len(argv) < 2 || !strings.Contains(argv[len(argv)-1], crashRoot) {
		t.Fatalf("resumed executor argv does not carry %q: %v", crashRoot, argv)
	}

	// --- close --------------------------------------------------------------
	if got := b.ok(t, "", "session", "close", crash); got["state"] != StateClosed || got["session"] != crash {
		t.Fatalf("close = %v", got)
	}
	if s := b.ok(t, "", "session", "get", crash); s["state"] != StateClosed || s["pid"] != float64(0) {
		t.Fatalf("after close = %v", s)
	}
	closedSend := b.run(t, "still there?", "send", crash)
	if closedSend.status != 1 || errorCode(t, closedSend) != CodeClosed {
		t.Fatalf("send on closed session = %d %s", closedSend.status, closedSend.stdout)
	}
	if got := b.ok(t, "", "session", "close", crash); got["state"] != StateClosed {
		t.Fatalf("second close = %v", got)
	}

	// --- boxd restart -------------------------------------------------------
	before, _ := b.ok(t, "", "session", "get", sid)["executor_session"].(string)
	b.stop()
	startBoxd(t, b)
	ls = b.ok(t, "", "session", "ls")
	sessions, _ := ls["sessions"].([]any)
	if len(sessions) < 4 {
		t.Fatalf("sessions after restart = %d, want every session", len(sessions))
	}
	afterGet := b.ok(t, "", "session", "get", sid)
	if afterGet["state"] != StateIdle || afterGet["pid"] != float64(0) {
		t.Fatalf("session after restart = %v", afterGet)
	}
	if afterGet["executor_session"] != before {
		t.Fatalf("root id changed across restart: %v -> %v", before, afterGet["executor_session"])
	}
	b.ok(t, "say:after restart", "send", sid)
	rt := b.ok(t, "", "wait", sid, "--turn", "2", "--timeout", "20")
	if rt["state"] != TurnDone {
		t.Fatalf("turn after restart = %v", rt)
	}
	evs = b.ok(t, "", "events", sid, "--since", "0")
	if !strings.Contains(fmt.Sprint(evs["events"]), KindExecutorRestart) {
		t.Fatalf("restart did not emit executor_restart: %v", evs["events"])
	}
	argv = argvLines(t, b, sid)
	if !strings.Contains(argv[len(argv)-1], before) {
		t.Fatalf("restart did not resume: %v", argv)
	}

	// --- journal ------------------------------------------------------------
	journal := b.journal.String()
	lines := strings.Split(strings.TrimSpace(journal), "\n")
	if len(lines) < 10 {
		t.Fatalf("journal has %d lines", len(lines))
	}
	parsed := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("journal line is not JSON: %q", line)
		}
		if ev["kind"] == nil || ev["seq"] == nil || ev["session"] == nil {
			t.Fatalf("journal event lacks contract fields: %v", ev)
		}
		parsed++
	}
	if parsed < 10 {
		t.Fatalf("only %d journal events", parsed)
	}

	// --- unknown ids and usage errors ---------------------------------------
	bad := b.run(t, "", "session", "get", "s-nope")
	if bad.status != 1 || errorCode(t, bad) != CodeNotFound {
		t.Fatalf("unknown session = %d %s", bad.status, bad.stdout)
	}
	usage := b.run(t, "", "bogus-command")
	if usage.status != 1 || errorCode(t, usage) != CodeInvalid {
		t.Fatalf("unknown command = %d %s", usage.status, usage.stdout)
	}
	if usage.stderr == "" {
		t.Fatal("usage errors should explain themselves on stderr")
	}

	// --- boxd down ----------------------------------------------------------
	b.stop()
	down := b.run(t, "", "info")
	if down.status != 1 || errorCode(t, down) != CodeBoxdUnavailable {
		t.Fatalf("boxctl without boxd = %d %s", down.status, down.stdout)
	}
}

func errorCode(t *testing.T, r boxctlResult) string {
	t.Helper()
	doc := r.doc(t)
	e, _ := doc["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error document: %q", r.stdout)
	}
	code, _ := e["code"].(string)
	if msg, _ := e["message"].(string); msg == "" {
		t.Fatalf("error has no message: %q", r.stdout)
	}
	return code
}

// argvLines returns the argv lines the fake executor logged for a session.
func argvLines(t *testing.T, b *boxdProc, id string) []string {
	t.Helper()
	path := filepath.Join(b.state, "sessions", id, "prime", "fake-argv.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}
