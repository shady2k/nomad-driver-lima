package boxd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- what a finished turn leaves running ---------------------------------------
//
// A turn ends when the executor says it does: the ACP answer comes back with
// stop reason end_turn and boxd records the turn as done. That is the
// executor's decision, and boxd keeps reporting it exactly as given -- the
// turn's state, its stop reason and the exit code of `boxctl wait` do not
// change because something else is running.
//
// What a turn ends with is not always all there is. The executor runs commands
// through its tools, and a tool can start something that outlives the answer:
// a build, a test run, a server put in the background. boxd cannot say the
// work is unfinished -- only the executor knows what it meant to do -- so it
// says what it saw instead: the processes the turn left running in the guest,
// in the words of a measurement, never of a verdict.
//
// WHICH PROCESSES COUNT. A process is reported only when ALL of these hold:
//
//   - It is a userspace process. Its /proc/<pid>/cmdline is not empty (a kernel
//     thread has no command line) and it is not a zombie (a process that has
//     exited and that its parent has not reaped is not running).
//   - It started after the turn started. /proc/<pid>/stat counts starttime in
//     clock ticks since the boot that /proc/uptime reports, which gives the
//     moment it started; a process that started before the turn belongs to
//     something else. boxd starts an executor with its own process group but
//     inside boxd's cgroup (session.go, ensureExecutor), which is why the
//     executor's own program is caught by the next rule and not by this one.
//   - It is not boxd's or an executor's own program. Neither the executable
//     (/proc/<pid>/exe) nor the command it was given is under the platform
//     bundle (<PlatformDir>, /opt/box: boxd, the executors and their tools) or
//     under the executor homes (<StateDir>/homes), where an executor keeps its
//     language kernels, downloaded runtimes and helper programs. Those are the
//     box's machinery, not the work the turn asked for.
//   - It is not a system service. The guest runs boxd as a systemd unit
//     (boxd.service), and a process boxd starts inherits its cgroup; a process
//     in another unit's cgroup is that unit's -- the DHCP client, an apt
//     timer's apt-get, the journal -- and reporting it would say "the turn
//     started the guest's own housekeeping". So only a process in boxd's own
//     cgroup, or below it, is a candidate. When boxd has no unit of its own to
//     compare with (a guest without systemd, a container whose cgroup is "/"),
//     there is no other service to tell it apart from and this rule excludes
//     nothing.
//
// The rule cannot be perfect: a program an executor installed in /usr/local/bin
// and left running starts after the turn and is in boxd's cgroup, and it is
// reported. The wording is therefore "may have started" and "are still
// running", and never "the work is not finished".

const (
	// stillRunningLines caps how many processes one turn reports: a build tree
	// says what it is with one line, and the rest is "and N more".
	stillRunningLines = 10
	// clockTicks is USER_HZ, the unit /proc/<pid>/stat counts utime, stime and
	// starttime in. The kernel fixes it at 100 for everything it exports.
	clockTicks = 100
	// stillRunningCommandMax is how much of a command line a report shows.
	stillRunningCommandMax = 80
	// stillRunningArgs is how many of a command's arguments a report names: a
	// command line can hold a token, and a process list is read by whoever
	// asks for it.
	stillRunningArgs = 3
	// stillRunningTick is the resolution of /proc/<pid>/stat's starttime.
	stillRunningTick = time.Second / clockTicks
)

// StillRunning is what a turn left running in the guest: one entry per process
// at the top of its own tree or process group, at most stillRunningLines of
// them, and how many more there were.
type StillRunning struct {
	Processes []StillRunningProcess `json:"processes"`
	More      int                   `json:"more,omitempty"`
}

// StillRunningProcess is one process a finished turn left running: the top of a
// tree or process group, with the rest of that tree counted in Children.
type StillRunningProcess struct {
	Pid        int     `json:"pid"`
	Command    string  `json:"command"`
	AgeSeconds int     `json:"age_seconds"`
	CPUSeconds float64 `json:"cpu_seconds"`
	Children   int     `json:"children"`
}

// Count is how many processes a turn left running, the reported ones and the
// ones under "and N more".
func (s *StillRunning) Count() int {
	if s == nil {
		return 0
	}
	return len(s.Processes) + s.More
}

// Line is one process the way a person reads it: what it runs, how long it has
// run, how much CPU it used and how many processes are in its tree.
func (p StillRunningProcess) Line() string {
	parts := []string{
		fmt.Sprintf("pid %d", p.Pid),
		(time.Duration(p.AgeSeconds) * time.Second).String(),
	}
	if p.Children == 1 {
		parts = append(parts, "1 child")
	} else if p.Children > 1 {
		parts = append(parts, fmt.Sprintf("%d children", p.Children))
	}
	parts = append(parts, fmt.Sprintf("%.1fs CPU", p.CPUSeconds))
	return fmt.Sprintf("%s (%s)", p.Command, strings.Join(parts, ", "))
}

// Lines are the lines of a report, ending with "and N more" when it was cut.
func (s *StillRunning) Lines() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Processes)+1)
	for _, p := range s.Processes {
		out = append(out, p.Line())
	}
	if s.More > 0 {
		out = append(out, fmt.Sprintf("and %d more", s.More))
	}
	return out
}

// newStillRunning caps what a scan found at stillRunningLines and counts the
// rest as "and N more". Nothing found is no report at all: the field is left
// out of the turn, so a turn that left nothing behind says nothing.
func newStillRunning(found []StillRunningProcess) *StillRunning {
	if len(found) == 0 {
		return nil
	}
	if len(found) <= stillRunningLines {
		return &StillRunning{Processes: found}
	}
	return &StillRunning{Processes: found[:stillRunningLines], More: len(found) - stillRunningLines}
}

// --- the scan -----------------------------------------------------------------

// procScan is one read of the guest's process table. Everything it needs from
// outside /proc is decided once, so a test can run it against a fake tree.
type procScan struct {
	// root is the guest's /proc (a test gives a directory of its own).
	root string
	// boot is when the guest booted; now is the moment of the scan. A process's
	// starttime, counted from boot in clockTicks, is placed on the clock with
	// these two.
	boot time.Time
	now  time.Time
	// ownCgroup is boxd's own cgroup, the one its executors and their tools
	// inherit; a process in another unit's cgroup is that unit's. Empty or "/"
	// means boxd has no unit to compare with.
	ownCgroup string
	// skip are the directories that belong to the box's machinery: the platform
	// bundle and the executor homes.
	skip []string
	// self is boxd's own pid, which is never reported.
	self int
}

// stillRunningScan is the scan boxd runs at the end of a turn in this guest.
func (m *Manager) stillRunningScan(now time.Time) procScan {
	cfg := m.cfg
	root := cfg.procRoot()
	skip := []string{cfg.platformDir(), filepath.Join(cfg.StateDir, "homes")}
	// The executors are boxd's own programs wherever they are configured: in a
	// box they are in the bundle's bin/, and a boxd that was given a path starts
	// the executor there.
	for _, bin := range []string{cfg.PrimeAgent, cfg.OpenCode, cfg.ClaudeACP} {
		if bin == "" {
			continue
		}
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		skip = append(skip, path)
		if real, err := filepath.EvalSymlinks(path); err == nil {
			skip = append(skip, real)
		}
	}
	return procScan{
		root:      root,
		boot:      bootTime(now, root),
		now:       now,
		ownCgroup: ownCgroup(root),
		skip:      skip,
		self:      os.Getpid(),
	}
}

// stillRunningAt reports what a turn that started at since and ended at now
// left running.
//
// A scan that fails is not a turn that fails: the failure is logged once and
// the report is empty. The turn's state, its text and its stop reason are the
// executor's, and nothing boxd measures may change them.
func (m *Manager) stillRunningAt(since, now time.Time) *StillRunning {
	if since.IsZero() {
		// A turn without a start has no window: nothing is "after the turn
		// started", so nothing is reported.
		return nil
	}
	found, err := m.stillRunningScan(now).run(since)
	if err != nil {
		m.stillRunningErr.Do(func() {
			m.cfg.logf("could not read the guest's process table to report what the turn left running: %v", err)
		})
		return nil
	}
	return newStillRunning(found)
}

// bootTime is when the guest booted: now, less the uptime the kernel reports.
// A guest that does not report an uptime has no boot time, and the scan places
// no process on the clock (see started).
func bootTime(now time.Time, root string) time.Time {
	data, err := os.ReadFile(filepath.Join(root, "uptime"))
	if err != nil {
		return time.Time{}
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return time.Time{}
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return time.Time{}
	}
	return now.Add(-time.Duration(secs * float64(time.Second)))
}

// ownCgroup is the cgroup of the process that asks, from /proc/self/cgroup:
// the "0::/path" line is the one a cgroup v2 guest writes.
func ownCgroup(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "self", "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

// procInfo is the part of /proc/<pid>/stat the scan reads.
type procInfo struct {
	state      byte
	ppid       int
	pgid       int
	utime      uint64
	stime      uint64
	startTicks uint64
}

// parseProcStat reads the fields of /proc/<pid>/stat the scan uses. The command
// name in the second field may hold spaces and parentheses, so the fields start
// after the last ')'.
func parseProcStat(data []byte) (procInfo, error) {
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return procInfo{}, fmt.Errorf("no comm field")
	}
	f := strings.Fields(string(data[i+1:]))
	// After the comm: state, ppid, pgrp, session, tty_nr, tpgid, flags,
	// minflt, cminflt, majflt, cmajflt, utime, stime, ..., starttime at 19.
	if len(f) < 20 {
		return procInfo{}, fmt.Errorf("%d fields after the comma, want 20", len(f))
	}
	if len(f[0]) != 1 {
		return procInfo{}, fmt.Errorf("state %q", f[0])
	}
	var p procInfo
	p.state = f[0][0]
	var err error
	for _, field := range []struct {
		dst *int
		val string
	}{{&p.ppid, f[1]}, {&p.pgid, f[2]}} {
		*field.dst, err = strconv.Atoi(field.val)
		if err != nil {
			return procInfo{}, err
		}
	}
	for _, field := range []struct {
		dst *uint64
		val string
	}{{&p.utime, f[11]}, {&p.stime, f[12]}, {&p.startTicks, f[19]}} {
		*field.dst, err = strconv.ParseUint(field.val, 10, 64)
		if err != nil {
			return procInfo{}, err
		}
	}
	return p, nil
}

// started is when a process of this scan started, on the clock of the turn.
func (ps procScan) started(p procInfo) time.Time {
	return ps.boot.Add(time.Duration(p.startTicks) * time.Second / clockTicks)
}

// cpuSeconds is how much CPU a process used so far.
func (p procInfo) cpuSeconds() float64 {
	return float64(p.utime+p.stime) / clockTicks
}

// candidate is one process the rules above let through, with what a report
// needs of it.
type candidate struct {
	procInfo
	pid    int
	argv   []string
	exe    string
	cgroup string
}

// run scans the process table and returns the processes a turn started at
// since left running, one per tree or process group, the most CPU first.
func (ps procScan) run(since time.Time) ([]StillRunningProcess, error) {
	ents, err := os.ReadDir(ps.root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ps.root, err)
	}
	var found []candidate
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == ps.self {
			continue
		}
		// A process that vanishes between the directory read and its own files
		// is not a failure: it is a process that ended while the scan ran, and
		// it is not running any more.
		c, ok := ps.read(pid)
		if !ok {
			continue
		}
		if ps.belongs(c, since) {
			found = append(found, c)
		}
	}
	return ps.group(found), nil
}

// read reads one process, or reports false when it is not there any more or not
// readable.
func (ps procScan) read(pid int) (candidate, bool) {
	dir := filepath.Join(ps.root, strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return candidate{}, false
	}
	info, err := parseProcStat(data)
	if err != nil {
		return candidate{}, false
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return candidate{}, false
	}
	argv := splitArgv(cmdline)
	cgroup, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		// A process with no readable cgroup is one that ended mid-scan, or one
		// boxd may not place: it cannot be told apart from a service's, so the
		// rule leaves it out.
		return candidate{}, false
	}
	c := candidate{procInfo: info, pid: pid, argv: argv, cgroup: cgroupPath(cgroup)}
	// /proc/<pid>/exe is a symlink into the program's own file; it is missing
	// for a process that ended, and unreadable for one of another user.
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		c.exe = exe
	}
	return c, true
}

// splitArgv splits a /proc/<pid>/cmdline on its NUL separators.
func splitArgv(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

// cgroupPath is the cgroup a /proc/<pid>/cgroup file names: the "0::/path" line
// of a cgroup v2 guest.
func cgroupPath(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

// belongs applies the rule at the top of this file.
func (ps procScan) belongs(c candidate, since time.Time) bool {
	// A kernel thread has no command line; a zombie has exited.
	if len(c.argv) == 0 || c.state == 'Z' || c.state == 'X' {
		return false
	}
	// Started after the turn did. A scan without a boot time cannot place a
	// process on the clock, and an unplaceable process is left out: saying
	// nothing is better than saying a process started when the turn did not.
	//
	// starttime counts in ticks, and it is truncated down, so a process that
	// started a millisecond after the turn did can carry a starttime one tick
	// before it. The comparison therefore allows one tick and leaves out what is
	// certainly older: the cost is that a process that started just before the
	// turn may be reported, and saying one thing too many is the safe way to be
	// wrong about a guest's process table.
	if ps.boot.IsZero() || ps.started(c.procInfo).Add(stillRunningTick).Before(since) {
		return false
	}
	// The box's own machinery: boxd, the executors, their tools, and what an
	// executor runs out of its home.
	for _, path := range []string{c.exe, c.argv[0]} {
		if path == "" || !strings.HasPrefix(path, "/") {
			continue
		}
		for _, dir := range ps.skip {
			if dir != "" && (path == dir || strings.HasPrefix(path, dir+"/")) {
				return false
			}
		}
	}
	return ps.inOwnCgroup(c.cgroup)
}

// inOwnCgroup reports whether a process's cgroup is boxd's own, or below it.
func (ps procScan) inOwnCgroup(path string) bool {
	own := ps.ownCgroup
	if own == "" || own == "/" {
		// boxd has no unit of its own to compare with: there is no other
		// service's cgroup to exclude a process by.
		return true
	}
	return path == own || strings.HasPrefix(path, strings.TrimSuffix(own, "/")+"/")
}

// groupStillRunning reports each process once, at the top of its own tree or
// process group: a `go build` and its forty `compile` children are one line
// with a count of children, not forty-one lines. A process and the parent it
// shares a tree with belong together, and so do a process and the leader of the
// process group it is in -- an executor's tool runs commands in groups of its
// own, and a group survives the shell that led it.
func (ps procScan) group(found []candidate) []StillRunningProcess {
	if len(found) == 0 {
		return nil
	}
	byPid := make(map[int]int, len(found)) // pid -> index in found
	for i, c := range found {
		byPid[c.pid] = i
	}
	parent := make([]int, len(found))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	for i, c := range found {
		if j, ok := byPid[c.ppid]; ok {
			union(i, j)
		}
		if j, ok := byPid[c.pgid]; ok && c.pgid != c.pid {
			union(i, j)
		}
	}
	groups := map[int][]int{}
	for i := range found {
		root := find(i)
		groups[root] = append(groups[root], i)
	}
	out := make([]StillRunningProcess, 0, len(groups))
	for _, members := range groups {
		top := members[0]
		for _, i := range members[1:] {
			// The top of a group is the process that started first; the lowest
			// pid breaks a tie, so a report does not move between scans.
			if found[i].startTicks < found[top].startTicks ||
				(found[i].startTicks == found[top].startTicks && found[i].pid < found[top].pid) {
				top = i
			}
		}
		c := found[top]
		age := ps.now.Sub(ps.started(c.procInfo))
		if age < 0 {
			age = 0
		}
		out = append(out, StillRunningProcess{
			Pid:        c.pid,
			Command:    commandText(c.argv),
			AgeSeconds: int(age.Round(time.Second) / time.Second),
			CPUSeconds: c.cpuSeconds(),
			Children:   len(members) - 1,
		})
	}
	// The heaviest first: a build that takes the box's CPU is what a person
	// wants to see before the tail of a log.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUSeconds != out[j].CPUSeconds {
			return out[i].CPUSeconds > out[j].CPUSeconds
		}
		if out[i].AgeSeconds != out[j].AgeSeconds {
			return out[i].AgeSeconds > out[j].AgeSeconds
		}
		return out[i].Pid < out[j].Pid
	})
	return out
}

// commandText is what a report shows of a command: the program and its first
// arguments, cut to stillRunningCommandMax characters. The journal's tool lines
// keep a tool's own text and no credential is known to be in a process's
// arguments, so this shows the program and the first few arguments and no more:
// an argument can hold a token, and a process list is read by whoever asks.
func commandText(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := []string{filepath.Base(argv[0])}
	for _, a := range argv[1:] {
		if len(parts)-1 >= stillRunningArgs {
			break
		}
		if strings.TrimSpace(a) == "" {
			continue
		}
		parts = append(parts, a)
	}
	return oneLine(strings.Join(parts, " "), stillRunningCommandMax)
}
