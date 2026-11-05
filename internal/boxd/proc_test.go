package boxd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProcs is a /proc tree for the load sampler: one directory per process with
// the few fields a sample reads, so a sample can be handed a table in a state no
// real guest is in (a core held for ten minutes, a pid that ends, a command line
// that is longer than a line).
//
// It is the same shape as the fake tree stillrunning_test.go builds for the turn's
// report: the guest's own files, so nothing here needs a VM.
type fakeProcs struct {
	t    *testing.T
	root string
	boot time.Time
}

// newFakeProcs makes a tree that booted at boot.
func newFakeProcs(t *testing.T, boot time.Time) *fakeProcs {
	t.Helper()
	return &fakeProcs{t: t, root: t.TempDir(), boot: boot}
}

// load is one process of the fake table.
type load struct {
	pid     int
	comm    string
	argv    []string
	cpu     time.Duration // CPU time used in all
	started time.Time
	rssMiB  uint64
}

// write puts one process in the tree, replacing what was there.
func (f *fakeProcs) write(p load) {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(p.pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	tick := time.Second / clockTicks
	ticks := uint64(p.cpu / tick)
	start := uint64(p.started.Sub(f.boot) / tick)
	rssPages := p.rssMiB << 20 / uint64(os.Getpagesize())
	// After the command name: state, ppid, pgrp, then the fields the kernel
	// always writes, with utime at 11, starttime at 19 and rss in pages at 21.
	fields := []string{
		"S", "1", fmt.Sprint(p.pid), "0", "0", "0", "0", "0", "0", "0", "0",
		fmt.Sprint(ticks), "0", "0", "0", "20", "0", "1", "0",
		fmt.Sprint(start), "4096", fmt.Sprint(rssPages),
	}
	for len(fields) < 40 {
		fields = append(fields, "0")
	}
	f.writeFile(dir, "stat", fmt.Sprintf("%d (%s) %s\n", p.pid, p.comm, strings.Join(fields, " ")))
	argv := p.argv
	if argv == nil {
		argv = []string{p.comm}
	}
	f.writeFile(dir, "cmdline", strings.Join(argv, "\x00")+"\x00")
}

// remove takes a process out of the tree, as an exit does.
func (f *fakeProcs) remove(pid int) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.root, fmt.Sprint(pid))); err != nil {
		f.t.Fatal(err)
	}
}

// at writes the uptime that places the tree's boot at the moment now, which is
// what a sample reads to place a start on the clock.
func (f *fakeProcs) at(now time.Time) {
	f.t.Helper()
	f.writeFile(f.root, "uptime", fmt.Sprintf("%.2f 0.00\n", now.Sub(f.boot).Seconds()))
}

func (f *fakeProcs) writeFile(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// TestProcessSampleShowsTheBusiestByCPU: box top asked for the busiest processes
// gets them, most CPU first, with what of a core each used since the last sample,
// the memory it holds and the command cut short.
func TestProcessSampleShowsTheBusiestByCPU(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProcs(t, t0.Add(-time.Hour))
	f.write(load{pid: 100, comm: "cc1", argv: []string{"cc1", "-c", "big.c", "-o", "big.o"},
		cpu: 10 * time.Second, started: t0.Add(-time.Minute), rssMiB: 512})
	f.write(load{pid: 200, comm: "bash", argv: []string{"/bin/bash", "-l"},
		cpu: time.Second, started: t0.Add(-time.Minute), rssMiB: 8})
	w := &procWatch{}

	// The first sample has nothing to compare with: it says the use since each
	// process started.
	f.at(t0)
	first := w.sample(t0, f.root)
	if len(first.Busy) != 2 {
		t.Fatalf("%d processes, want 2: %+v", len(first.Busy), first.Busy)
	}
	if first.Busy[0].Pid != 100 {
		t.Fatalf("the busiest is %+v, want pid 100", first.Busy[0])
	}
	if first.Busy[0].Command != "cc1 -c big.c -o" {
		t.Fatalf("command = %q", first.Busy[0].Command)
	}
	if first.Busy[0].MemMiB != 512 {
		t.Fatalf("memory = %d MiB, want 512", first.Busy[0].MemMiB)
	}

	// Five seconds later cc1 used another five seconds: a whole core. bash used
	// half a second: a tenth of one.
	f.write(load{pid: 100, comm: "cc1", argv: []string{"cc1", "-c", "big.c"}, cpu: 15 * time.Second,
		started: t0.Add(-time.Minute), rssMiB: 512})
	f.write(load{pid: 200, comm: "bash", argv: []string{"/bin/bash", "-l"}, cpu: 1500 * time.Millisecond,
		started: t0.Add(-time.Minute), rssMiB: 8})
	t1 := t0.Add(usageEvery)
	f.at(t1)
	second := w.sample(t1, f.root)
	if got := second.Busy[0].CPUPercent; got < 99 || got > 101 {
		t.Fatalf("cc1 used %.1f%% of a core, want about 100", got)
	}
	if got := second.Busy[1].CPUPercent; got < 9 || got > 11 {
		t.Fatalf("bash used %.1f%% of a core, want about 10", got)
	}
}

// TestProcessSampleIsCappedAndNamesAFew: box top prints five processes and not a
// wall of them, and a process with no command line still has a name.
func TestProcessSampleIsCappedAndNamesAFew(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFakeProcs(t, t0.Add(-time.Hour))
	for i := 0; i < 8; i++ {
		f.write(load{pid: 300 + i, comm: fmt.Sprint("proc", i), cpu: time.Duration(i+1) * time.Second,
			started: t0.Add(-time.Hour)})
	}
	// A process whose command line is empty: the kernel's own name stands in.
	f.write(load{pid: 400, comm: "kworker/0:1", argv: []string{""}, cpu: 100 * time.Second, started: t0.Add(-time.Hour)})
	f.at(t0)
	reading := (&procWatch{}).sample(t0, f.root)
	if len(reading.Busy) != usageProcesses {
		t.Fatalf("%d processes, want %d", len(reading.Busy), usageProcesses)
	}
	if reading.Busy[0].Pid != 400 || reading.Busy[0].Command != "kworker/0:1" {
		t.Fatalf("a process with no command line is shown as %+v", reading.Busy[0])
	}
	if got := commandOf(f.root, 999, "gone"); got != "gone" {
		t.Fatalf("a process that cannot be read is named %q, want its kernel name", got)
	}
}

// TestAWholeCoreIsSaidOncePerCrossing: a process that uses a whole core for ten
// minutes gets one journal line, and one when it stops using it -- a box that lost
// a core for four hours and said nothing is what this is for.
func TestAWholeCoreIsSaidOncePerCrossing(t *testing.T) {
	boot := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	f := newFakeProcs(t, boot)
	w := &procWatch{}
	started := boot.Add(10 * time.Minute)
	var said []string
	// A whole core, in five second samples, for twelve minutes.
	now := boot.Add(20 * time.Minute)
	used := time.Duration(0)
	for i := 0; i < 12*60/5; i++ {
		now = now.Add(usageEvery)
		used += usageEvery
		f.write(load{pid: 851, comm: "systemd-logind", argv: []string{"/usr/lib/systemd/systemd-logind"},
			cpu: used, started: started, rssMiB: 4})
		f.at(now)
		reading := w.sample(now, f.root)
		said = append(said, reading.Said...)
		if len(said) > 1 {
			// The crossing is one line and stays one line.
			t.Fatalf("the crossing was said %d times: %v", len(said), said)
		}
		if len(said) == 1 && reading.Hogs == nil {
			t.Fatalf("the hog is not in the sample that said it: %+v", reading)
		}
	}
	if len(said) != 1 {
		t.Fatalf("%d journal lines, want the one crossing: %v", len(said), said)
	}
	if said[0] != "systemd-logind (pid 851) has used a whole core for 10m" {
		t.Fatalf("line = %q", said[0])
	}
	last := w.sample(now, f.root)
	if len(last.Hogs) != 1 || last.Hogs[0].Pid != 851 || last.Hogs[0].Command != "systemd-logind" {
		t.Fatalf("hogs = %+v, want the process that holds a core", last.Hogs)
	}
	if last.Hogs[0].ForSeconds < 11*60 {
		t.Fatalf("for_seconds = %d, want the twelve minutes it has held the core", last.Hogs[0].ForSeconds)
	}

	// It stops using the core: one line, and then nothing more.
	f.write(load{pid: 851, comm: "systemd-logind", argv: []string{"/usr/lib/systemd/systemd-logind"},
		cpu: used + 250*time.Millisecond, started: started, rssMiB: 4})
	f.at(now.Add(usageEvery))
	stopped := w.sample(now.Add(usageEvery), f.root)
	if len(stopped.Said) != 1 {
		t.Fatalf("lines when a core is given up = %v, want one", stopped.Said)
	}
	if !strings.Contains(stopped.Said[0], "systemd-logind (pid 851) no longer uses a whole core") {
		t.Fatalf("line = %q", stopped.Said[0])
	}
	if len(stopped.Hogs) != 0 {
		t.Fatalf("hogs = %+v, want none once the core is given up", stopped.Hogs)
	}
	f.at(now.Add(2 * usageEvery))
	if quiet := w.sample(now.Add(2*usageEvery), f.root); len(quiet.Said) != 0 {
		t.Fatalf("a process that gave up a core is said once: %v", quiet.Said)
	}
}

// TestASpikeIsNotAWholeCore: a compile that holds a core for a minute is not
// news, and a process that drops below the threshold starts its clock again.
func TestASpikeIsNotAWholeCore(t *testing.T) {
	boot := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	f := newFakeProcs(t, boot)
	w := &procWatch{}
	started := boot.Add(10 * time.Minute)
	now := boot.Add(20 * time.Minute)
	used := time.Duration(0)
	var said []string
	// A whole core for two minutes, then nothing, then a whole core again: the
	// second stretch is measured from the sample after the drop.
	for i := 0; i < 24; i++ {
		now = now.Add(usageEvery)
		if i < 12 {
			used += usageEvery
		} else {
			used += time.Millisecond
		}
		f.write(load{pid: 900, comm: "cc1", cpu: used, started: started})
		f.at(now)
		said = append(said, w.sample(now, f.root).Said...)
	}
	if len(said) != 0 {
		t.Fatalf("a two minute burst of a whole core is not news: %v", said)
	}
	if len(w.held) != 0 {
		t.Fatalf("the state of a process that gave up its core is kept: %+v", w.held)
	}
}

// TestAProcessThatEndsWhileHoldingACoreIsSaidAndDropped: a pid that is gone takes
// its state with it, and a process that was holding a whole core says so on its
// way out.
func TestAProcessThatEndsWhileHoldingACoreIsSaidAndDropped(t *testing.T) {
	boot := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	f := newFakeProcs(t, boot)
	w := &procWatch{}
	now := boot.Add(20 * time.Minute)
	used := time.Duration(0)
	var said []string
	for i := 0; i < 12*60/5+1; i++ {
		now = now.Add(usageEvery)
		used += usageEvery
		f.write(load{pid: 851, comm: "systemd-logind", cpu: used, started: boot.Add(10 * time.Minute)})
		f.at(now)
		said = append(said, w.sample(now, f.root).Said...)
	}
	if len(said) != 1 {
		t.Fatalf("lines so far = %v, want the one crossing", said)
	}
	f.remove(851)
	f.at(now.Add(usageEvery))
	gone := w.sample(now.Add(usageEvery), f.root)
	if len(gone.Said) != 1 || !strings.Contains(gone.Said[0], "is gone") {
		t.Fatalf("lines when the process ended = %v, want one that says so", gone.Said)
	}
	if len(w.held) != 0 {
		t.Fatalf("the state of a pid that is gone is kept: %+v", w.held)
	}
}

// TestProcessSampleIsCheap: it runs every five seconds in a guest of a few
// hundred processes, so one sample is a few milliseconds and not a stall.
func TestProcessSampleIsCheap(t *testing.T) {
	w := &procWatch{}
	const runs = 10
	start := time.Now()
	for i := 0; i < runs; i++ {
		w.sample(time.Now().UTC(), "/proc")
	}
	each := time.Since(start) / runs
	if each > 200*time.Millisecond {
		t.Fatalf("one sample took %s, want a few milliseconds", each)
	}
	t.Logf("one sample of %d processes took %s", len(w.prev), each)
	if len(w.prev) == 0 {
		t.Fatal("a sample of /proc found no process")
	}
}

// TestTheSamplerLeavesTheProcessesOutWhenTheTableIsUnreadable: a sample that
// cannot read the guest's process table says nothing about its processes rather
// than failing the rest of the guest's load.
func TestTheSamplerLeavesTheProcessesOutWhenTheTableIsUnreadable(t *testing.T) {
	u := &usageSampler{procRoot: filepath.Join(t.TempDir(), "no-such-proc")}
	var said []string
	logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	u.sample(logf)
	usage := u.Last()
	if usage.At.IsZero() {
		t.Fatal("the guest's load is measured with or without its process table")
	}
	if usage.Processes != nil || usage.Hogs != nil {
		t.Fatalf("processes = %+v, hogs = %+v, want both left out", usage.Processes, usage.Hogs)
	}
	if len(said) != 0 {
		t.Fatalf("a box with no process table said %v", said)
	}
}

// TestTheSamplerSaysAProcessThatHoldsACore: the journal line names the guest, as
// the other measured lines of this file do, and the process is in the load box top
// shows.
func TestTheSamplerSaysAProcessThatHoldsACore(t *testing.T) {
	boot := time.Now().UTC().Add(-time.Hour)
	f := newFakeProcs(t, boot)
	started := boot.Add(10 * time.Minute)
	f.write(load{pid: 851, comm: "systemd-logind", cpu: time.Since(started), started: started})
	f.at(time.Now().UTC())
	u := &usageSampler{procRoot: f.root}
	// A crossing the sampler did not see start: the state says it has held a
	// whole core since eleven minutes ago, which is what the next sample finds.
	u.procs.held = map[int]*hogState{851: {comm: "systemd-logind", since: time.Now().UTC().Add(-11 * time.Minute)}}
	var said []string
	logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	u.sample(logf)
	if len(said) != 1 {
		t.Fatalf("lines = %v, want one", said)
	}
	if !strings.HasPrefix(said[0], "guest: systemd-logind (pid 851) has used a whole core for ") {
		t.Fatalf("line = %q", said[0])
	}
	usage := u.Last()
	if len(usage.Hogs) != 1 || usage.Hogs[0].Pid != 851 {
		t.Fatalf("hogs = %+v, want the process that holds a core", usage.Hogs)
	}
	if len(usage.Processes) == 0 {
		t.Fatal("the load has no processes to show")
	}
}
