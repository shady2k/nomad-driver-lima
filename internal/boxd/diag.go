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

// watchKernel follows the guest kernel log and writes the lines of
// kernelTrouble to the journal as "guest kernel: ...". Without journalctl
// (a test, a guest without systemd) it does nothing.
func watchKernel(ctx context.Context, logf func(string, ...any)) {
	bin, err := exec.LookPath("journalctl")
	if err != nil {
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, bin, "-k", "-f", "-n", "0", "-o", "cat")
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
			if !kernelTrouble.MatchString(line) {
				continue
			}
			if time.Since(window) > time.Minute {
				said, window = 0, time.Now()
			}
			if said++; said <= 20 {
				logf("guest kernel: %s", oneLine(line, 300))
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
	DiskPath      string    `json:"disk_path"`
	DiskTotalMiB  uint64    `json:"disk_total_mib"`
	DiskFreeMiB   uint64    `json:"disk_free_mib"`
	Top           string    `json:"top,omitempty"`
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
	s := Usage{At: time.Now().UTC(), Load1: l1, Load5: l5, MemTotalMiB: total >> 20, MemAvailMiB: avail >> 20,
		DiskPath: "/", DiskTotalMiB: st.Blocks * uint64(st.Bsize) >> 20, DiskFreeMiB: st.Bavail * uint64(st.Bsize) >> 20,
		Top: topProcesses(3)}
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
