package boxd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// What boxd writes to the journal so a box can be diagnosed from its task log
// alone: what an executor prints outside its turns, how it exited, what the
// guest kernel says about memory and the disk, and when memory or disk runs
// low. All of it goes through logf, so it is stamped and free of terminal
// codes like every other line.

// --- an executor's stderr -----------------------------------------------------

// stderrLines is the executor's stderr as journal lines: "session <id> <executor>:
// <line>", at most stderrBurst lines in a stderrWindow, then one line that
// says how many were left out (they are all in executor.log).
type stderrLines struct {
	logf    func(string, ...any)
	prefix  string
	logPath string

	mu      sync.Mutex
	partial []byte
	window  time.Time
	said    int
	dropped int
}

const (
	stderrBurst  = 40
	stderrWindow = time.Minute
)

func newStderrLines(logf func(string, ...any), prefix, logPath string) *stderrLines {
	return &stderrLines{logf: logf, prefix: prefix, logPath: logPath}
}

func (s *stderrLines) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexAny(s.partial, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(s.partial[:i]))
		s.partial = s.partial[i+1:]
		if line != "" {
			s.sayLocked(line)
		}
	}
	if len(s.partial) > 4096 {
		s.sayLocked(strings.TrimSpace(string(s.partial)))
		s.partial = s.partial[:0]
	}
	return len(p), nil
}

func (s *stderrLines) sayLocked(line string) {
	now := time.Now()
	if now.Sub(s.window) >= stderrWindow {
		if s.dropped > 0 {
			s.logf("%s: %d more line(s) in %s", s.prefix, s.dropped, s.logPath)
		}
		s.window, s.said, s.dropped = now, 0, 0
	}
	if s.said >= stderrBurst {
		s.dropped++
		return
	}
	s.said++
	s.logf("%s: %s", s.prefix, oneLine(line, 300))
}

// lastLines returns the last n non-empty lines of a file, read from its end.
func lastLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const chunk = 64 << 10
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := st.Size() - chunk
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	var lines []string
	for _, l := range strings.FieldsFunc(string(buf), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// --- log files an executor keeps by itself ------------------------------------

// tailDir follows the *.log files below dir and writes their new lines to the
// journal as "<label> <file>: <line>", rate-limited per file like stderrLines.
// prime's background workers write their stderr there (setting up its Python
// kernel, downloading packages), which no session's stderr carries.
func tailDir(ctx context.Context, logf func(string, ...any), label, dir string) {
	offsets := map[string]int64{}
	writers := map[string]*stderrLines{}
	first := true
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
		for _, path := range files {
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			off, seen := offsets[path]
			if !seen && first {
				// What was there before boxd started is not news.
				offsets[path] = st.Size()
				continue
			}
			if st.Size() < off {
				off = 0
			}
			if st.Size() == off {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			data := make([]byte, st.Size()-off)
			n, _ := f.ReadAt(data, off)
			f.Close()
			offsets[path] = off + int64(n)
			w := writers[path]
			if w == nil {
				w = newStderrLines(logf, label+" "+filepath.Base(path), path)
				writers[path] = w
			}
			_, _ = w.Write(data[:n])
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// --- the guest kernel ---------------------------------------------------------

// kernelTrouble is what of the kernel log matters for a box: memory, the disk,
// hung tasks and crashes.
var kernelTrouble = regexp.MustCompile(`(?i)out of memory|oom-kill|oom_reaper|killed process|i/o error|ext4-fs (error|warning)|` +
	`buffer i/o|no space left|blocked for more than|soft lockup|hard lockup|segfault|general protection|call trace|remounting filesystem read-only`)

// earlyoomTrouble is what of earlyoom's journal matters for a box: that the
// guest ran out of memory, which process earlyoom killed for it, and whether it
// had to escalate to SIGKILL. Its periodic "mem avail" report, its startup lines
// and the "Will avoid killing process names ..." line are not news.
//
// The patterns are earlyoom's own wording (1.9): "low memory! at or below
// SIGTERM limits", "Low memory! Killing process 4711 "cc1"", "sending SIGTERM to
// process 4711 uid 0 ...", "escalating to SIGKILL after 10.000 seconds".
var earlyoomTrouble = regexp.MustCompile(`(?i)low memory|SIG(TERM|KILL) to process|Killing process [0-9]|` +
	`killing whole process group|escalating to SIGKILL|SIGTERM and SIGKILL`)

// watchKernel follows the guest kernel log and writes the lines of
// kernelTrouble to the journal as "guest kernel: ...". Without journalctl
// (a test, a guest without systemd) it does nothing.
func watchKernel(ctx context.Context, logf func(string, ...any)) {
	followJournal(ctx, logf, "guest kernel", kernelTrouble, "-k", "-f", "-n", "0", "-o", "cat")
}

// watchEarlyOOM follows the guest's earlyoom service, which is what kills a
// process when the guest runs out of memory before the kernel would: a box whose
// guest was frozen by memory has to say which process earlyoom chose, or the
// task log shows a guest that stopped and nothing about why.
//
// It is a journal watch of its own, not part of watchKernel: earlyoom's lines
// are not kernel lines, and -o cat drops the "earlyoom[pid]:" prefix that would
// otherwise name them.
func watchEarlyOOM(ctx context.Context, logf func(string, ...any)) {
	followJournal(ctx, logf, "guest earlyoom", earlyoomTrouble, "-u", "earlyoom", "-f", "-n", "0", "-o", "cat")
}

// followJournal follows one part of the journal and writes the lines keep
// matches to the journal under label, at most 20 a minute. A guest without
// journalctl (a test, a guest without systemd) gets nothing.
func followJournal(ctx context.Context, logf func(string, ...any), label string, keep *regexp.Regexp, args ...string) {
	bin, err := exec.LookPath("journalctl")
	if err != nil {
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, bin, args...)
		out, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		if err := cmd.Start(); err != nil {
			return
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		said, window := 0, time.Now()
		for sc.Scan() {
			line := sc.Text()
			if !keep.MatchString(line) {
				continue
			}
			if time.Since(window) > time.Minute {
				said, window = 0, time.Now()
			}
			if said++; said <= 20 {
				logf("%s: %s", label, oneLine(line, 300))
			}
		}
		_ = cmd.Wait()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// --- memory and disk ----------------------------------------------------------

// resourceWatch says once when the guest's available memory or a disk's free
// space falls below a tenth, with what takes the memory, and once when it is
// back.
type resourceWatch struct {
	logf  func(string, ...any)
	disks []string
	low   map[string]bool
}

func watchResources(ctx context.Context, logf func(string, ...any), disks []string) {
	w := &resourceWatch{logf: logf, disks: disks, low: map[string]bool{}}
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		w.check()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *resourceWatch) check() {
	if total, avail := meminfo(); total > 0 {
		low := avail*10 < total
		if low && !w.low["memory"] {
			w.logf("guest memory low: %d MiB available of %d MiB; the most resident: %s",
				avail>>20, total>>20, topProcesses(3))
		} else if !low && w.low["memory"] && avail*5 >= total {
			w.logf("guest memory: %d MiB available of %d MiB again", avail>>20, total>>20)
		} else {
			low = w.low["memory"] && avail*5 < total
		}
		w.low["memory"] = low
	}
	for _, path := range w.disks {
		var st syscall.Statfs_t
		if syscall.Statfs(path, &st) != nil || st.Blocks == 0 {
			continue
		}
		total := st.Blocks * uint64(st.Bsize)
		free := st.Bavail * uint64(st.Bsize)
		low := free*10 < total || free < 1<<30
		key := "disk " + path
		if low && !w.low[key] {
			w.logf("guest disk low: %s has %d MiB free of %d MiB; `box resize <box> --disk-gib N` grows it",
				path, free>>20, total>>20)
		} else if !low && w.low[key] {
			w.logf("guest disk: %s has %d MiB free of %d MiB again", path, free>>20, total>>20)
		}
		w.low[key] = low
	}
}

// meminfo returns MemTotal and MemAvailable in bytes.
func meminfo() (total, avail uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v << 10
		case "MemAvailable:":
			avail = v << 10
		}
	}
	return total, avail
}

// topProcesses names the n processes with the most resident memory.
func topProcesses(n int) string {
	type proc struct {
		name string
		rss  uint64
	}
	var procs []proc
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		var p proc
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "Name:"); ok {
				p.name = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				f := strings.Fields(v)
				if len(f) > 0 {
					kb, _ := strconv.ParseUint(f[0], 10, 64)
					p.rss = kb << 10
				}
			}
		}
		if p.rss > 0 {
			procs = append(procs, p)
		}
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].rss > procs[j].rss })
	if len(procs) > n {
		procs = procs[:n]
	}
	parts := make([]string, 0, len(procs))
	for _, p := range procs {
		parts = append(parts, fmt.Sprintf("%s %d MiB", p.name, p.rss>>20))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
}

// --- the guest's processes: what takes the CPU, and what holds a whole core ---

const (
	// usageProcesses is how many of the guest's busiest processes box top shows:
	// enough to say what is doing the work, few enough to read.
	usageProcesses = 5
	// hogPercent is the share of one core a process has to use in every sample
	// for its use to count as a whole core.
	hogPercent = 90.0
	// hogAfter is how long it has to use it before the journal hears of it. A
	// compile holds a core for a minute and a test run for five, and neither is
	// news; ten minutes of it is worth a line, and the line is a measurement.
	hogAfter = 10 * time.Minute
)

// ProcessUsage is one of the guest's busiest processes, as box top shows it: the
// share of one core it used since the last sample, the memory it holds, and what
// it runs.
type ProcessUsage struct {
	Pid        int     `json:"pid"`
	Command    string  `json:"command"`
	CPUPercent float64 `json:"cpu_percent"`
	MemMiB     uint64  `json:"mem_mib"`
}

// Line is one process the way box top prints it.
func (p ProcessUsage) Line() string {
	return fmt.Sprintf("pid %d  %.1f%% of a core  %d MiB  %s", p.Pid, p.CPUPercent, p.MemMiB, p.Command)
}

// Hog is a process that has used a whole core for at least hogAfter of the
// samples boxd took. The journal says so once and says when it stops; box top
// shows it for as long as it lasts.
//
// It is a measurement and never a verdict: a long compile holds a core for ten
// minutes, and so does a process the guest has not finished with. What the line
// is for is that someone was watching a box that had a core taken and nobody said
// so.
type Hog struct {
	Pid        int     `json:"pid"`
	Command    string  `json:"command"`
	CPUPercent float64 `json:"cpu_percent"`
	ForSeconds int     `json:"for_seconds"`
}

// Line says what was seen: who holds a core, and for how long.
func (h Hog) Line() string {
	return fmt.Sprintf("%s (pid %d) has used a whole core for %s", h.Command, h.Pid,
		spanText(time.Duration(h.ForSeconds)*time.Second))
}

// spanText is a span the way a person reads it: minutes for the minutes, and
// seconds only below a minute.
func spanText(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// procLoad is one process of one sample: what it runs, how much CPU time it has
// used in all, when it started and how much memory it holds.
type procLoad struct {
	pid     int
	comm    string
	ticks   uint64
	started time.Time
	rssMiB  uint64
	// percent is the share of one core this process used in the sample that
	// placed it here. It is kept so a sample taken at the same moment as the
	// last one has something to say rather than nothing.
	percent float64
}

// procReading is what one sample of the guest's process table says.
type procReading struct {
	// Busy are the processes that used the most CPU since the last sample, most
	// first, at most usageProcesses of them.
	Busy []ProcessUsage
	// Hogs are the processes that have held a whole core for hogAfter.
	Hogs []Hog
	// Said are the journal lines of this sample: one when a process crosses into
	// holding a whole core, one when it stops using it, one when it ends.
	Said []string
}

// procWatch samples the guest's process table and keeps what it needs between
// samples: the CPU time every process had then, so its use is the share of a core
// it used since, and the processes that have held a whole core.
//
// One sample is one read per process -- a few hundred small files every five
// seconds -- and it stays a few milliseconds, which is why it is part of the load
// boxd measures and never on the path of a guest call.
type procWatch struct {
	prev   map[int]procLoad
	prevAt time.Time
	// held are the processes that are using a whole core now, by pid. A process
	// that is gone from the table is dropped: a box that has run for hours must
	// not keep the state of every pid it has seen.
	held map[int]*hogState
}

// hogState is one process that is using a whole core, and whether the journal
// has heard of it yet.
type hogState struct {
	comm    string
	since   time.Time
	said    bool
	percent float64
}

// sample reads the guest's process table at now and returns what box top shows
// and what the journal has to say. A table that cannot be read is not a failure
// of the sample: the processes are simply left out, and the rest of the guest's
// load is still measured.
func (w *procWatch) sample(now time.Time, root string) procReading {
	procs, err := readProcTable(now, root)
	if err != nil {
		return procReading{}
	}
	percents := w.percents(procs, now)
	for pid, pct := range percents {
		p := procs[pid]
		p.percent = pct
		procs[pid] = p
	}
	busy := busiest(procs, percents, usageProcesses)
	for i := range busy {
		busy[i].Command = commandOf(root, busy[i].Pid, procs[busy[i].Pid].comm)
	}
	hogs, said := w.cross(procs, percents, now)
	w.prev, w.prevAt = procs, now
	return procReading{Busy: busy, Hogs: hogs, Said: said}
}

// percents is how much of one core each process used: the CPU time it used since
// the last sample over that sample, or, for a process that is new to this one,
// over its own life.
func (w *procWatch) percents(procs map[int]procLoad, now time.Time) map[int]float64 {
	out := make(map[int]float64, len(procs))
	interval := 0.0
	if !w.prevAt.IsZero() && now.After(w.prevAt) {
		interval = now.Sub(w.prevAt).Seconds()
	}
	for pid, p := range procs {
		if prev, ok := w.prev[pid]; ok && p.ticks >= prev.ticks {
			if interval > 0 {
				out[pid] = 100 * float64(p.ticks-prev.ticks) / (interval * clockTicks)
			} else {
				// The same moment again (a second sample of one instant): the
				// figure of the last sample stands.
				out[pid] = prev.percent
			}
			continue
		}
		// A pid the last sample did not have, or one the kernel has given to
		// another process since: its use since it started is all there is to say
		// about it.
		if !p.started.IsZero() {
			if life := now.Sub(p.started).Seconds(); life > 0 {
				out[pid] = 100 * float64(p.ticks) / (life * clockTicks)
			}
		}
	}
	return out
}

// cross moves every process that uses a whole core one sample further, keeps the
// ones that have used it for hogAfter, and says what changed: the ones that
// cross into holding a whole core, the ones that stop using it and the ones that
// ended while holding it.
//
// It says each crossing once: a process that holds a core for four hours is one
// line when it starts and one when it stops, not one every five seconds.
func (w *procWatch) cross(procs map[int]procLoad, percents map[int]float64, now time.Time) ([]Hog, []string) {
	if w.held == nil {
		w.held = map[int]*hogState{}
	}
	var said []string
	for pid, p := range procs {
		pct := percents[pid]
		h, held := w.held[pid]
		switch {
		case pct >= hogPercent:
			if !held {
				w.held[pid] = &hogState{comm: p.comm, since: now, percent: pct}
				continue
			}
			h.comm, h.percent = p.comm, pct
			if !h.said && now.Sub(h.since) >= hogAfter {
				h.said = true
				said = append(said, Hog{Pid: pid, Command: h.comm, CPUPercent: pct,
					ForSeconds: int(now.Sub(h.since).Round(time.Second) / time.Second)}.Line())
			}
		case held:
			if h.said {
				said = append(said, fmt.Sprintf("%s (pid %d) no longer uses a whole core: %.0f%% of one since the last sample, after %s",
					h.comm, pid, pct, spanText(now.Sub(h.since))))
			}
			delete(w.held, pid)
		}
	}
	// A process that ended while it was using a core stopped using it too, and a
	// pid that is gone must not keep its state.
	for pid, h := range w.held {
		if _, ok := procs[pid]; ok {
			continue
		}
		if h.said {
			said = append(said, fmt.Sprintf("%s (pid %d) is gone; it had used a whole core for %s",
				h.comm, pid, spanText(now.Sub(h.since))))
		}
		delete(w.held, pid)
	}
	var hogs []Hog
	for pid, h := range w.held {
		if !h.said {
			continue
		}
		hogs = append(hogs, Hog{Pid: pid, Command: h.comm, CPUPercent: h.percent,
			ForSeconds: int(now.Sub(h.since).Round(time.Second) / time.Second)})
	}
	// The longest held first: it is the one to look at.
	sort.Slice(hogs, func(i, j int) bool {
		if hogs[i].ForSeconds != hogs[j].ForSeconds {
			return hogs[i].ForSeconds > hogs[j].ForSeconds
		}
		return hogs[i].Pid < hogs[j].Pid
	})
	return hogs, said
}

// readProcTable reads one sample of the guest's process table from root: every
// process the guest has, with what it runs and what it cost so far. A process
// that vanishes between the directory read and its own file is one that ended
// while the sample ran, and a kernel thread or a zombie has nothing to say here.
func readProcTable(now time.Time, root string) (map[int]procLoad, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	boot := bootTime(now, root)
	out := make(map[int]procLoad, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		info, err := parseProcStat(data)
		if err != nil || info.state == 'Z' || info.state == 'X' {
			continue
		}
		p := procLoad{
			pid:    pid,
			comm:   info.comm,
			ticks:  info.utime + info.stime,
			rssMiB: info.rssPages * uint64(os.Getpagesize()) >> 20,
		}
		if !boot.IsZero() {
			p.started = boot.Add(time.Duration(info.startTicks) * time.Second / clockTicks)
		}
		out[pid] = p
	}
	return out, nil
}

// busiest is the n processes that used the most CPU, most first, and the memory
// each holds. A process that used none is still worth showing when the guest is
// idle: what holds the memory is the answer then.
func busiest(procs map[int]procLoad, percents map[int]float64, n int) []ProcessUsage {
	loads := make([]procLoad, 0, len(procs))
	for _, p := range procs {
		loads = append(loads, p)
	}
	sort.Slice(loads, func(i, j int) bool {
		pi, pj := percents[loads[i].pid], percents[loads[j].pid]
		if pi != pj {
			return pi > pj
		}
		if loads[i].rssMiB != loads[j].rssMiB {
			return loads[i].rssMiB > loads[j].rssMiB
		}
		return loads[i].pid < loads[j].pid
	})
	if len(loads) > n {
		loads = loads[:n]
	}
	out := make([]ProcessUsage, 0, len(loads))
	for _, p := range loads {
		out = append(out, ProcessUsage{Pid: p.pid, CPUPercent: percents[p.pid], MemMiB: p.rssMiB})
	}
	return out
}

// commandOf is what a process runs, the way box top shows it: the program and
// its first few arguments, cut short, or the kernel's own name for the process
// when its command line cannot be read.
func commandOf(root string, pid int, comm string) string {
	if data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline")); err == nil {
		if text := commandText(splitArgv(data)); text != "" {
			return text
		}
	}
	return oneLine(comm, 30)
}

// --- memory stall -------------------------------------------------------------

// memoryPressurePath is where the Linux kernel reports memory pressure, if it is
// built with PSI (CONFIG_PSI, on by default since 4.20).
const memoryPressurePath = "/proc/pressure/memory"

const (
	// memoryStallAvg10 is the share of the last ten seconds a guest has to be
	// stalled on memory for this to count as a stall: 10%.
	memoryStallAvg10 = 10.0
	// memoryPressureSaidAfter is how long the stall has to last before the
	// journal hears of it, so a single spike says nothing.
	memoryPressureSaidAfter = time.Minute
)

// Pressure is what the kernel says about a resource's stalls: the share of the
// last ten seconds in which some (or, for "full", every) task was stalled on it.
// The kernel also reports the last sixty and three hundred seconds; only the ten
// second figures are kept, because they are the ones that say "now".
type Pressure struct {
	// SomeAvg10 is the percentage of the last ten seconds in which at least one
	// task was stalled.
	SomeAvg10 float64 `json:"some_avg10"`
	// FullAvg10 is the percentage of the last ten seconds in which every task
	// was stalled: the guest did nothing but wait for memory.
	FullAvg10 float64 `json:"full_avg10"`
}

// readPressure reads the pressure file at path. A kernel without the file (no
// PSI) is not an error: the guest simply has nothing to say about pressure, and
// nil says exactly that.
func readPressure(path string) *Pressure {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p Pressure
	var some, full bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		avg10, ok := avgField(fields[1:], "avg10")
		if !ok {
			continue
		}
		switch fields[0] {
		case "some":
			p.SomeAvg10, some = avg10, true
		case "full":
			p.FullAvg10, full = avg10, true
		}
	}
	if !some && !full {
		return nil
	}
	return &p
}

// avgField returns the value of key in the k=v pairs of one pressure line.
func avgField(fields []string, key string) (float64, bool) {
	for _, field := range fields {
		value, ok := strings.CutPrefix(field, key+"=")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// --- what the guest is doing, for box status and box top ----------------------

// Usage is the guest's load as boxd last measured it.
type Usage struct {
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpu_percent"`
	IOWaitPercent float64   `json:"iowait_percent"`
	Load1         float64   `json:"load1"`
	Load5         float64   `json:"load5"`
	MemTotalMiB   uint64    `json:"mem_total_mib"`
	MemAvailMiB   uint64    `json:"mem_available_mib"`
	// MemoryPressure is the kernel's own report on memory stalls, nil on a
	// kernel without /proc/pressure/memory.
	MemoryPressure *Pressure `json:"memory_pressure,omitempty"`
	DiskPath       string    `json:"disk_path"`
	DiskTotalMiB   uint64    `json:"disk_total_mib"`
	DiskFreeMiB    uint64    `json:"disk_free_mib"`
	Top            string    `json:"top,omitempty"`
	// Processes are the guest's busiest processes by CPU, most first: what box
	// top prints without a flag. A sample that cannot read the process table
	// leaves them out.
	Processes []ProcessUsage `json:"processes,omitempty"`
	// Hogs are the processes that have used a whole core for ten minutes, the
	// longest first. box top shows them; the journal said each one once.
	Hogs []Hog `json:"hogs,omitempty"`
	// Leftover is the work running with no turn in progress (stillrunning.go).
	// It is filled in when boxd is asked for the guest's load and no session is
	// running a turn, and it is left out when one is: a turn's own work cannot
	// be told from what it leaves behind.
	Leftover *Leftover `json:"leftover,omitempty"`
}

// usageSampler measures the guest every usageEvery and keeps the last sample.
type usageSampler struct {
	mu       sync.Mutex
	last     Usage
	prevBusy uint64
	prevIO   uint64
	prevAll  uint64
	// ioSince is when iowait went above a third; the journal hears of it
	// after a minute of it.
	ioSince time.Time
	ioSaid  bool
	// memSince is when the memory stall started; the journal hears of it after
	// a minute of it, once per crossing.
	memSince time.Time
	memSaid  bool
	// pressureFile is where the memory pressure file is read from;
	// memoryPressurePath when empty (the tests point it at a file).
	pressureFile string
	// procRoot is where the process table is read from; procRoot() of the
	// daemon's configuration calls it /proc, and a test points it at a tree of
	// its own (stillrunning_test.go builds one).
	procRoot string
	// procs samples the process table: what takes the CPU, and what holds a
	// whole core.
	procs procWatch
}

// pressurePath is the file this sampler reads memory pressure from.
func (u *usageSampler) pressurePath() string {
	if u.pressureFile != "" {
		return u.pressureFile
	}
	return memoryPressurePath
}

// processesPath is the process table this sampler reads.
func (u *usageSampler) processesPath() string {
	if u.procRoot != "" {
		return u.procRoot
	}
	return "/proc"
}

const usageEvery = 5 * time.Second

func (u *usageSampler) Last() Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

func (u *usageSampler) run(ctx context.Context, logf func(string, ...any)) {
	tick := time.NewTicker(usageEvery)
	defer tick.Stop()
	for {
		u.sample(logf)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (u *usageSampler) sample(logf func(string, ...any)) {
	busy, iowait, all := cpuTimes()
	total, avail := meminfo()
	var st syscall.Statfs_t
	_ = syscall.Statfs("/", &st)
	l1, l5 := loadavg()

	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now().UTC()
	procs := u.procs.sample(now, u.processesPath())
	s := Usage{At: now, Load1: l1, Load5: l5, MemTotalMiB: total >> 20, MemAvailMiB: avail >> 20,
		MemoryPressure: readPressure(u.pressurePath()),
		DiskPath:       "/", DiskTotalMiB: st.Blocks * uint64(st.Bsize) >> 20, DiskFreeMiB: st.Bavail * uint64(st.Bsize) >> 20,
		Top: topProcesses(3), Processes: procs.Busy, Hogs: procs.Hogs}
	if u.prevAll > 0 && all > u.prevAll {
		d := float64(all - u.prevAll)
		s.CPUPercent = 100 * float64(busy-u.prevBusy) / d
		s.IOWaitPercent = 100 * float64(iowait-u.prevIO) / d
	}
	u.prevBusy, u.prevIO, u.prevAll = busy, iowait, all
	u.last = s
	if logf == nil {
		return
	}
	// A process that has held a whole core, and one that stopped holding it: a
	// measurement of what the guest does, said once per crossing, because a box
	// that lost a core for four hours and said nothing is what this is for.
	for _, line := range procs.Said {
		logf("guest: %s", line)
	}
	switch {
	case s.IOWaitPercent > 33:
		if u.ioSince.IsZero() {
			u.ioSince = s.At
		}
		if !u.ioSaid && s.At.Sub(u.ioSince) >= time.Minute {
			u.ioSaid = true
			logf("guest waits on its disk: iowait %.0f%% for %s (CPU %.0f%%, %d MiB available); the most resident: %s",
				s.IOWaitPercent, s.At.Sub(u.ioSince).Round(time.Second), s.CPUPercent, s.MemAvailMiB, s.Top)
		}
	case s.IOWaitPercent < 10:
		if u.ioSaid {
			logf("guest disk wait is back to %.0f%%", s.IOWaitPercent)
		}
		u.ioSince, u.ioSaid = time.Time{}, false
	}
	if p := s.MemoryPressure; p != nil {
		switch {
		case p.FullAvg10 > memoryStallAvg10:
			if u.memSince.IsZero() {
				u.memSince = s.At
			}
			if !u.memSaid && s.At.Sub(u.memSince) >= memoryPressureSaidAfter {
				u.memSaid = true
				logf("guest is stalled on memory: full avg10=%.1f%%, MemAvailable %d of %d MiB; the biggest processes: %s",
					p.FullAvg10, s.MemAvailMiB, s.MemTotalMiB, s.Top)
			}
		// Half the threshold is the way back, so a stall that hovers around 10%
		// does not say so again every ten seconds.
		case p.FullAvg10 < memoryStallAvg10/2:
			if u.memSaid {
				logf("guest memory stall is over: full avg10=%.1f%%, %d MiB available of %d MiB",
					p.FullAvg10, s.MemAvailMiB, s.MemTotalMiB)
			}
			u.memSince, u.memSaid = time.Time{}, false
		}
	}
}

// cpuTimes reads the busy, iowait and total jiffies of all CPUs.
func cpuTimes() (busy, iowait, total uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(line)
	if len(f) < 8 || f[0] != "cpu" {
		return 0, 0, 0
	}
	var v [10]uint64
	for i := 1; i < len(f) && i <= 10; i++ {
		v[i-1], _ = strconv.ParseUint(f[i], 10, 64)
	}
	// user nice system idle iowait irq softirq steal guest guest_nice
	for i := 0; i < 8; i++ {
		total += v[i]
	}
	idle := v[3] + v[4]
	return total - idle, v[4], total
}

func loadavg() (float64, float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(string(data))
	if len(f) < 2 {
		return 0, 0
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	b, _ := strconv.ParseFloat(f[1], 64)
	return a, b
}
