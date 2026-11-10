package boxd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProc is a /proc tree a test can build: one directory per process with the
// four files the scan reads, and a boot time that turns a wall-clock moment into
// the starttime ticks the kernel would report.
type fakeProc struct {
	t    *testing.T
	root string
	boot time.Time
	now  time.Time
}

func newFakeProc(t *testing.T, now time.Time) *fakeProc {
	t.Helper()
	f := &fakeProc{t: t, root: t.TempDir(), boot: now.Add(-time.Hour), now: now}
	if err := os.MkdirAll(filepath.Join(f.root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The two files the scan reads besides the processes: the guest's uptime and
	// the cgroup of the process that asks (boxd's own unit).
	f.write(f.root, "uptime", fmt.Sprintf("%.2f 0.00\n", now.Sub(f.boot).Seconds()))
	f.write(filepath.Join(f.root, "self"), "cgroup", "0::/system.slice/boxd.service\n")
	return f
}

// fakeProcess is one process of the fake tree.
type fakeProcess struct {
	pid        int
	ppid       int
	pgid       int
	started    time.Time
	cpu        time.Duration
	argv       []string
	cgroup     string
	exe        string
	state      byte
	envSession string
}

// add writes one process.
func (f *fakeProc) add(p fakeProcess) {
	f.t.Helper()
	if p.ppid == 0 {
		p.ppid = 1
	}
	if p.pgid == 0 {
		p.pgid = p.pid
	}
	if p.cgroup == "" {
		p.cgroup = "/system.slice/boxd.service"
	}
	if p.state == 0 {
		p.state = 'S'
	}
	dir := filepath.Join(f.root, fmt.Sprint(p.pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	ticks := uint64(p.started.Sub(f.boot) / (time.Second / clockTicks))
	utime := uint64(p.cpu / (time.Second / clockTicks))
	// The fields of /proc/<pid>/stat after the command name. The scan reads
	// state (0), ppid (1), pgrp (2), utime (11), stime (12) and starttime (19);
	// the rest is written so a wrong field number shows up as a wrong answer.
	comm := fmt.Sprintf("proc%d", p.pid)
	if p.exe != "" {
		comm = filepath.Base(p.exe)
	}
	fields := []string{
		string(p.state), fmt.Sprint(p.ppid), fmt.Sprint(p.pgid), "0",
		"0", "0", "0", "0", "0", "0", "0",
		fmt.Sprint(utime), "0", "0", "0", "20", "0", "1", "0",
		fmt.Sprint(ticks),
	}
	for len(fields) < 50 {
		fields = append(fields, "0")
	}
	f.write(dir, "stat", fmt.Sprintf("%d (%s) %s\n", p.pid, comm, strings.Join(fields, " ")))
	f.write(dir, "cmdline", strings.Join(p.argv, "\x00")+"\x00")
	f.write(dir, "cgroup", "0::"+p.cgroup+"\n")
	if p.envSession != "" {
		f.write(dir, "environ", "BOX_SESSION="+p.envSession+"\x00")
	}
	if p.exe != "" {
		// /proc/<pid>/exe is a symlink to the program's own file; the target
		// need not exist for a scan that only reads the path.
		if err := os.Symlink(p.exe, filepath.Join(dir, "exe")); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakeProc) write(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// scan is the scan this fake tree answers, from boxd's own cgroup.
func (f *fakeProc) scan() procScan {
	return procScan{
		root:      f.root,
		boot:      f.boot,
		now:       f.now,
		ownCgroup: "/system.slice/boxd.service",
		skip:      []string{"/opt/box", "/var/lib/boxd/homes"},
		self:      os.Getpid(),
	}
}

// TestStillRunningCollapsesATreeToOneLine: a build and its children are one
// line with a count, not forty-one lines: a person reads what is running, not
// every fork of it. The tree is also the process group here, as a tool's
// command is (boxd gives an executor its own group; a shell gives a pipeline
// the same one), and the pid of the top is the one a person can kill.
func TestStillRunningSessionMarker(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	start := now.Add(-time.Minute)
	f.add(fakeProcess{pid: 800, started: now.Add(-10 * time.Minute), argv: []string{"executor"}})
	f.add(fakeProcess{pid: 801, started: now.Add(-10 * time.Minute), argv: []string{"executor"}})
	f.add(fakeProcess{pid: 301, started: start, argv: []string{"/bin/sh", "work"}, envSession: "B"})
	f.add(fakeProcess{pid: 302, started: start, argv: []string{"/bin/sh", "work"}, envSession: "A"})
	f.add(fakeProcess{pid: 303, ppid: 800, started: start, argv: []string{"/bin/sh", "work"}})
	f.add(fakeProcess{pid: 304, ppid: 801, started: start, argv: []string{"/bin/sh", "work"}})
	f.add(fakeProcess{pid: 305, started: start, argv: []string{"/bin/sh", "work"}})
	ps := f.scan()
	ps.session = "A"
	ps.executorPID = 800
	ps.executors = map[int]string{800: "A", 801: "B"}
	found, err := ps.run(now.Add(-2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("found %v, want A marker, A executor descendant and unmarked orphan", found)
	}
}

func TestStillRunningLeavesOutLoneSleep(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-5 * time.Minute)
	f.add(fakeProcess{pid: 200, started: now.Add(-time.Minute), argv: []string{"sleep", "300"}, exe: "/usr/bin/sleep"})
	f.add(fakeProcess{pid: 201, started: now.Add(-time.Minute), argv: []string{"sh", "-c", "sleep 300; true"}, exe: "/bin/sh"})
	f.add(fakeProcess{pid: 202, started: now.Add(-time.Minute), argv: []string{"/usr/bin/python3", "worker.py"}, exe: "/usr/bin/python3"})
	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 || found[0].Pid == 200 || found[1].Pid == 200 {
		t.Fatalf("found = %v, want sh and python but not lone sleep", found)
	}
}

func TestStillRunningCollapsesATreeToOneLine(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-5 * time.Minute)
	start := now.Add(-4 * time.Minute)
	f.add(fakeProcess{pid: 300, started: start, argv: []string{"/bin/sh", "-c", "go build ./..."}})
	for i := 0; i < 40; i++ {
		f.add(fakeProcess{pid: 400 + i, ppid: 300, pgid: 300, started: start.Add(time.Second),
			cpu: 10 * time.Millisecond, argv: []string{"/usr/local/go/pkg/tool/compile", "-o", "x.a", "x.go"}})
	}
	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("%d line(s), want the whole tree as one: %v", len(found), found)
	}
	got := found[0]
	if got.Pid != 300 || got.Children != 40 {
		t.Fatalf("top = pid %d with %d children, want pid 300 with 40", got.Pid, got.Children)
	}
	if got.Command != "sh -c go build ./..." {
		t.Fatalf("command = %q", got.Command)
	}
	if got.AgeSeconds != 240 {
		t.Fatalf("age = %ds, want 240", got.AgeSeconds)
	}
	if line := got.Line(); line != "sh -c go build ./... (pid 300, 4m0s, 40 children, 0.0s CPU)" {
		t.Fatalf("line = %q", line)
	}
}

// TestStillRunningLeavesOutWhatStartedBeforeTheTurn: a process that was already
// there when the turn started is not the turn's, and a scan that cannot place a
// process on the clock reports nothing rather than guessing.
func TestStillRunningLeavesOutWhatStartedBeforeTheTurn(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-4 * time.Minute)
	f.add(fakeProcess{pid: 100, started: now.Add(-30 * time.Minute), argv: []string{"/usr/bin/python3", "-m", "http.server"}})
	f.add(fakeProcess{pid: 200, started: turnStart.Add(-time.Second), argv: []string{"/usr/bin/apt-get", "update"}})
	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("reported processes older than the turn: %v", found)
	}

	// The same tree, a turn that started before them: both are the turn's.
	found, err = f.scan().run(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("%d line(s), want 2: %v", len(found), found)
	}

	// A scan with no boot time places no process: it reports nothing.
	blind := f.scan()
	blind.boot = time.Time{}
	if found, err := blind.run(now.Add(-time.Hour)); err != nil || len(found) != 0 {
		t.Fatalf("a scan without a boot time reported %v (err %v)", found, err)
	}
}

// TestStillRunningLeavesOutTheBoxsOwnMachinery: boxd, the executor and what an
// executor runs out of its own home are the box, not the work the turn asked
// for -- even when they started after the turn did.
func TestStillRunningLeavesOutTheBoxsOwnMachinery(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-4 * time.Minute)
	after := now.Add(-time.Minute)
	// The bundle: boxd, the executor, and a tool the bundle ships.
	f.add(fakeProcess{pid: 100, started: after, argv: []string{"/opt/box/bin/boxd", "-socket", "/run/boxd/boxd.sock"}, exe: "/opt/box/bin/boxd"})
	f.add(fakeProcess{pid: 101, started: after, argv: []string{"prime-agent", "--mode", "acp"}, exe: "/opt/box/tools/prime-agent/bin/prime-agent"})
	// An executor's home: its language kernel and a helper it runs out of it.
	f.add(fakeProcess{pid: 102, started: after, argv: []string{"/var/lib/boxd/homes/prime/.bun/bin/bun", "install"}, exe: "/var/lib/boxd/homes/prime/.bun/bin/bun"})
	f.add(fakeProcess{pid: 103, started: after, argv: []string{"python3", "-m", "pytest"}, exe: "/var/lib/boxd/homes/claude/.venv/bin/python3"})
	// A directory whose name merely starts the same way belongs to somebody else.
	f.add(fakeProcess{pid: 104, started: after, argv: []string{"/opt/boxer/tool"}, exe: "/opt/boxer/tool"})
	// The work: a program the executor installed outside both.
	f.add(fakeProcess{pid: 105, started: after, argv: []string{"/usr/local/bin/go", "test", "./..."}, exe: "/usr/local/bin/go"})

	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("%d line(s), want the bundle and home left out and 2 kept: %v", len(found), found)
	}
	pids := []int{found[0].Pid, found[1].Pid}
	for _, want := range []int{104, 105} {
		if pids[0] != want && pids[1] != want {
			t.Fatalf("pid %d is missing from %v", want, pids)
		}
	}
}

// TestStillRunningLeavesOutAnotherServicesCgroup: the guest's own housekeeping
// -- the DHCP client, an apt timer, the journal -- is a systemd unit's work and
// not the box's, so a process in another unit's cgroup is left out.
func TestStillRunningLeavesOutAnotherServicesCgroup(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-4 * time.Minute)
	after := now.Add(-time.Minute)
	f.add(fakeProcess{pid: 100, started: after, cgroup: "/system.slice/systemd-networkd.service",
		argv: []string{"/usr/lib/systemd/systemd-networkd"}, exe: "/usr/lib/systemd/systemd-networkd"})
	f.add(fakeProcess{pid: 101, started: after, cgroup: "/system.slice/apt-daily.service",
		argv: []string{"/usr/bin/apt-get", "update"}, exe: "/usr/bin/apt-get"})
	f.add(fakeProcess{pid: 102, started: after, cgroup: "/system.slice/cron.service",
		argv: []string{"/usr/sbin/cron", "-f"}, exe: "/usr/sbin/cron"})
	// In boxd's own unit, and below it: the box's work.
	f.add(fakeProcess{pid: 103, started: after, cgroup: "/system.slice/boxd.service",
		argv: []string{"go", "build"}, exe: "/usr/local/bin/go"})
	f.add(fakeProcess{pid: 104, started: after, cgroup: "/system.slice/boxd.service/session-1.scope",
		argv: []string{"pytest", "-x"}, exe: "/usr/local/bin/pytest"})

	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("%d line(s), want the two in boxd's own cgroup: %v", len(found), found)
	}
	for _, p := range found {
		if p.Pid != 103 && p.Pid != 104 {
			t.Fatalf("reported a process of another unit: %v", found)
		}
	}

	// A guest without a unit of its own (no systemd, a container at "/") has no
	// other service to tell a process apart from, so the rule leaves the other
	// tests to decide.
	loose := f.scan()
	loose.ownCgroup = "/"
	found, err = loose.run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 5 {
		t.Fatalf("%d line(s) with no unit to compare with, want all 5: %v", len(found), found)
	}
}

// TestStillRunningCapsTheListAtTenLines: a guest with a lot running says so in
// ten lines and a count, not in a wall of text.
func TestStillRunningCapsTheListAtTenLines(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-4 * time.Minute)
	for i := 0; i < 13; i++ {
		f.add(fakeProcess{pid: 200 + i, started: now.Add(-time.Minute), cpu: time.Duration(13-i) * time.Second,
			argv: []string{"/usr/bin/sleep", fmt.Sprint(300 + i)}})
	}
	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	report := newStillRunning(found)
	if report == nil || len(report.Processes) != stillRunningLines {
		t.Fatalf("report = %+v, want %d lines", report, stillRunningLines)
	}
	if report.More != 3 || report.Count() != 13 {
		t.Fatalf("more = %d, count = %d, want 3 and 13", report.More, report.Count())
	}
	lines := report.Lines()
	if len(lines) != stillRunningLines+1 || lines[len(lines)-1] != "and 3 more" {
		t.Fatalf("lines end with %q", lines[len(lines)-1])
	}
	// The heaviest first: a build that takes the box's CPU is what a person
	// wants to see before the tail of a log.
	if report.Processes[0].CPUSeconds != 13 {
		t.Fatalf("first line = %+v, want the most CPU", report.Processes[0])
	}
	// Nothing found is no report at all: the field stays out of the turn.
	if newStillRunning(nil) != nil {
		t.Fatal("an empty scan made a report")
	}
}

// TestStillRunningSkipsAProcessThatVanishes: a process that ends while the scan
// runs is not an error and not a line. /proc/<pid> can go between the directory
// read and the files of that pid, and a half-written entry is one that is gone.
func TestStillRunningSkipsAProcessThatVanishes(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-4 * time.Minute)
	f.add(fakeProcess{pid: 300, started: now.Add(-time.Minute), argv: []string{"go", "test"}})
	// Gone between the read of the directory and the read of its stat.
	if err := os.MkdirAll(filepath.Join(f.root, "301"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Gone after the stat: no cmdline, no cgroup.
	if err := os.MkdirAll(filepath.Join(f.root, "302"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "302"), "stat", "302 (proc302) S 1 302 0 0 0 0 0 0 0 0 0 0 0 0 20 0 1 0 1 0\n")
	// A zombie: exited, not reaped, not running.
	f.add(fakeProcess{pid: 303, started: now.Add(-time.Minute), state: 'Z', argv: []string{"grep", "-r", "x"}})
	// A kernel thread: no command line at all.
	f.add(fakeProcess{pid: 304, started: now.Add(-time.Minute), argv: nil})

	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("a vanishing process failed the scan: %v", err)
	}
	if len(found) != 1 || found[0].Pid != 300 {
		t.Fatalf("found %v, want only pid 300", found)
	}

	// A tree that cannot be read at all is a failure the caller logs and hides,
	// never a failed turn.
	broken := f.scan()
	broken.root = filepath.Join(f.root, "no-such-proc")
	if _, err := broken.run(turnStart); err == nil {
		t.Fatal("a scan of a missing /proc reported no error")
	}
}

// TestStillRunningCommandKeepsTheProgramAndAFewArguments: a command line is
// shown short -- the program and its first arguments -- because an argument can
// hold a token and the line is read by whoever asks for it.
func TestStillRunningCommandKeepsTheProgramAndAFewArguments(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := commandText([]string{"deploy", "--target", "prod", "--token", "s3cret-value", long})
	if strings.Contains(got, "s3cret-value") {
		t.Fatalf("the line shows a credential in a later argument: %q", got)
	}
	if strings.Contains(got, long) {
		t.Fatalf("the line shows a long argument in full: %q", got)
	}
	if got != "deploy --target prod --token" {
		t.Fatalf("the line is not the program and its first arguments: %q", got)
	}
	if len(got) > stillRunningCommandMax+3 {
		t.Fatalf("the line is %d characters, want about %d", len(got), stillRunningCommandMax)
	}
	if got := commandText([]string{"/bin/sh", "-c", "sleep 300 & wait"}); got != "sh -c sleep 300 & wait" {
		t.Fatalf("command = %q", got)
	}
	if got := commandText(nil); got != "" {
		t.Fatalf("command of nothing = %q", got)
	}
}

// TestStillRunningReportIsPartialButHonest: the lines say what was seen and how
// long it has run, and a group of one says no children at all.
func TestStillRunningReportIsPartialButHonest(t *testing.T) {
	p := StillRunningProcess{Pid: 123, Command: "go build", AgeSeconds: 250, CPUSeconds: 12.5, Children: 1}
	if got, want := p.Line(), "go build (pid 123, 4m10s, 1 child, 12.5s CPU)"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	one := StillRunningProcess{Pid: 7, Command: "sleep 300", AgeSeconds: 5}
	if got, want := one.Line(), "sleep 300 (pid 7, 5s, 0.0s CPU)"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	var none *StillRunning
	if none.Count() != 0 || none.Lines() != nil {
		t.Fatal("a nil report is not empty")
	}
}

// TestStillRunningScanFailureIsLoggedOnceAndLeavesTheTurnAlone: a guest that
// cannot be scanned says so in the log once, and the turn keeps the field empty.
func TestStillRunningScanFailureIsLoggedOnceAndLeavesTheTurnAlone(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProcRoot = filepath.Join(t.TempDir(), "no-such-proc")
	var logged []string
	cfg.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := newManager(t, cfg)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if report := m.stillRunningAt(now.Add(-time.Minute), now, "", 0); report != nil {
			t.Fatalf("a failed scan reported %+v", report)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("the failure was logged %d times, want once: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "process table") {
		t.Fatalf("the log line does not say what failed: %q", logged[0])
	}
}

// TestStillRunningAtReadsTheManagersOwnGuest: the scan boxd runs takes the
// bundle, the executor homes and the configured executors from the daemon's
// configuration, so a test's paths are the ones that are left out.
func TestStillRunningAtReadsTheManagersOwnGuest(t *testing.T) {
	cfg := testConfig(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	cfg.ProcRoot = f.root
	f.add(fakeProcess{pid: 100, started: now.Add(-time.Minute), argv: []string{cfg.PrimeAgent, "--mode", "acp"}, exe: cfg.PrimeAgent})
	f.add(fakeProcess{pid: 101, started: now.Add(-time.Minute), argv: []string{filepath.Join(cfg.PlatformDir, "bin", "boxd")},
		exe: filepath.Join(cfg.PlatformDir, "bin", "boxd")})
	f.add(fakeProcess{pid: 102, started: now.Add(-time.Minute), argv: []string{"pytest", "-x"},
		exe: filepath.Join(cfg.StateDir, "homes", "prime", ".venv", "bin", "pytest")})
	f.add(fakeProcess{pid: 103, started: now.Add(-time.Minute), argv: []string{"/usr/bin/sleep", "300"},
		exe: "/usr/bin/sleep"})
	m := newManager(t, cfg)
	report := m.stillRunningAt(now.Add(-2*time.Minute), now, "", 0)
	if report != nil {
		t.Fatalf("report = %+v, want no lone sleep", report)
	}
	// A turn without a start has no window at all.
	if report := m.stillRunningAt(time.Time{}, now, "", 0); report != nil {
		t.Fatalf("a turn without a start reported %+v", report)
	}
}

// TestStillRunningScanStaysUnderASecond: the scan runs at the end of every
// turn, so it has to be cheap. This reads the process table of the machine the
// test runs on -- a few hundred processes -- and says how long one scan takes.
func TestStillRunningScanStaysUnderASecond(t *testing.T) {
	cfg := testConfig(t)
	m := newManager(t, cfg)
	now := time.Now().UTC()
	const runs = 5
	start := time.Now()
	for i := 0; i < runs; i++ {
		m.stillRunningAt(now.Add(-time.Minute), time.Now().UTC(), "", 0)
	}
	each := time.Since(start) / runs
	if each > time.Second {
		t.Fatalf("one scan took %s, want well under a second", each)
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	t.Logf("one scan of %d processes took %s", n, each)
}

// TestStillRunningAllowsOneTickOfStarttime: starttime counts in clock ticks, so
// a process that started a millisecond after the turn can carry a starttime one
// tick before it -- a mock's sleep does exactly that. A process that is a tick
// or two older is left out.
func TestStillRunningAllowsOneTickOfStarttime(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProc(t, now)
	turnStart := now.Add(-4 * time.Minute)
	// The fake tree can only place a start on a tick, so these three are one
	// tick before the turn, exactly on it and one tick after it.
	f.add(fakeProcess{pid: 100, started: turnStart.Add(-stillRunningTick), argv: []string{"one", "tick", "before"}})
	f.add(fakeProcess{pid: 101, started: turnStart, argv: []string{"on", "the", "turn's", "start"}})
	f.add(fakeProcess{pid: 102, started: turnStart.Add(stillRunningTick), argv: []string{"one", "tick", "after"}})
	f.add(fakeProcess{pid: 103, started: turnStart.Add(-2 * stillRunningTick), argv: []string{"two", "ticks", "before"}})
	found, err := f.scan().run(turnStart)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := map[int]bool{}
	for _, p := range found {
		got[p.Pid] = true
	}
	if got[103] {
		t.Fatalf("a process two ticks older than the turn was reported: %v", found)
	}
	for _, pid := range []int{100, 101, 102} {
		if !got[pid] {
			t.Fatalf("pid %d, which started at or after the turn's start, is missing from %v", pid, found)
		}
	}
}

// TestLeftoverReportsWorkWithNoTurnRunning: a test run three hours ago can leave
// its helpers and its shells alive, and no turn is running to be blamed for them.
// The report is the rule of a finished turn without its start-time test, in the
// same lines and with the same cap.
func TestLeftoverReportsWorkWithNoTurnRunning(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t)
	f := newFakeProc(t, now)
	cfg.ProcRoot = f.root
	// Fifteen shells that started three hours before any turn: a turn's own
	// report would leave every one of them out.
	for i := 0; i < 15; i++ {
		f.add(fakeProcess{pid: 300 + i, started: now.Add(-3 * time.Hour), cpu: time.Duration(15-i) * time.Second,
			argv: []string{"/bin/sh", "-c", "sleep 300"}})
	}
	// The box's own machinery and another unit's work are not the work.
	f.add(fakeProcess{pid: 100, started: now.Add(-3 * time.Hour), argv: []string{filepath.Join(cfg.PlatformDir, "bin", "boxd")},
		exe: filepath.Join(cfg.PlatformDir, "bin", "boxd")})
	f.add(fakeProcess{pid: 101, started: now.Add(-3 * time.Hour), cgroup: "/system.slice/apt-daily.service",
		argv: []string{"/usr/bin/apt-get", "update"}, exe: "/usr/bin/apt-get"})
	m := newManager(t, cfg)

	left := m.leftoverAt(now)
	if left == nil {
		t.Fatal("fifteen shells with no turn running is not nothing")
	}
	if left.Count != 15 {
		t.Fatalf("count = %d, want the fifteen shells: %+v", left.Count, left)
	}
	if len(left.Processes) != stillRunningLines || left.More != 5 {
		t.Fatalf("%d line(s) and %d more, want %d and 5", len(left.Processes), left.More, stillRunningLines)
	}
	lines := left.Lines()
	if len(lines) != stillRunningLines+1 || lines[len(lines)-1] != "and 5 more" {
		t.Fatalf("lines end with %q", lines[len(lines)-1])
	}
	if !strings.Contains(lines[0], "sleep 300 (pid ") {
		t.Fatalf("first line = %q", lines[0])
	}
	// The heaviest first: the shell that used the most CPU is the one a person
	// wants to see first.
	if left.Processes[0].CPUSeconds != 15 {
		t.Fatalf("first line = %+v, want the most CPU", left.Processes[0])
	}
	// The age is the process's own, not a turn's: three hours.
	if left.Processes[0].AgeSeconds != 3*3600 {
		t.Fatalf("age = %ds, want 10800", left.Processes[0].AgeSeconds)
	}

	// Nothing running is no report at all.
	emptyCfg := testConfig(t)
	emptyCfg.ProcRoot = newFakeProc(t, now).root
	if left := newManager(t, emptyCfg).leftoverAt(now); left != nil {
		t.Fatalf("an empty guest reported %+v", left)
	}
}

// TestLeftoverSaysNothingWhileATurnRuns: a process of a running turn cannot be
// told from the work the turn was asked to do, so boxd says nothing at all then
// rather than reporting a turn's own build as leftover.
func TestLeftoverSaysNothingWhileATurnRuns(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t)
	f := newFakeProc(t, now)
	cfg.ProcRoot = f.root
	f.add(fakeProcess{pid: 300, started: now.Add(-3 * time.Hour), argv: []string{"/usr/bin/sleep", "300"},
		exe: "/usr/bin/sleep"})
	m := newManager(t, cfg)
	if m.TurnInProgress() {
		t.Fatal("no session of this box runs a turn")
	}
	if left := m.leftoverAt(now); left != nil {
		t.Fatalf("leftover = %+v, want no lone sleep", left)
	}
	// A session in a turn: the report is left out until it ends.
	s := m.lookupSession(mustSession(t, m, "prime", "p/m").ID)
	s.mu.Lock()
	s.busy = true
	s.mu.Unlock()
	if !m.TurnInProgress() {
		t.Fatal("a session with a turn running is a turn in progress")
	}
	if left := m.leftoverAt(now); left != nil {
		t.Fatalf("a running turn reported %+v as leftover", left)
	}
}

// TestLeftoverScanFailureIsLoggedOnce: a guest whose process table cannot be read
// says so once in the log, and boxctl info keeps answering.
func TestLeftoverScanFailureIsLoggedOnce(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProcRoot = filepath.Join(t.TempDir(), "no-such-proc")
	var logged []string
	cfg.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := newManager(t, cfg)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if left := m.leftoverAt(now); left != nil {
			t.Fatalf("a failed scan reported %+v", left)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("the failure was logged %d times, want once: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "no turn in progress") {
		t.Fatalf("the log line does not say what failed: %q", logged[0])
	}
}

// TestInfoCarriesTheWorkRunningWithNoTurn: the guest's load that boxctl info
// answers with carries the leftover report, so box status and box top can say
// that a box is busy with no turn in progress.
func TestInfoCarriesTheWorkRunningWithNoTurn(t *testing.T) {
	now := time.Now().UTC()
	cfg := testConfig(t)
	f := newFakeProc(t, now)
	cfg.ProcRoot = f.root
	f.add(fakeProcess{pid: 300, started: now.Add(-3 * time.Hour), cpu: 12 * time.Second,
		argv: []string{"/usr/bin/cc1", "-c", "big.c"}, exe: "/usr/bin/cc1"})
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	t.Cleanup(d.Manager().Shutdown)
	d.usage.sample(nil)
	info := d.Info(context.Background())
	if info.Usage == nil {
		t.Fatal("the info carries no usage")
	}
	if info.Usage.Leftover == nil || info.Usage.Leftover.Count != 1 {
		t.Fatalf("leftover = %+v, want the one process", info.Usage.Leftover)
	}
	if line := info.Usage.Leftover.Processes[0].Line(); !strings.Contains(line, "cc1 -c big.c") {
		t.Fatalf("line = %q", line)
	}
}
