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

// pressureFile writes a /proc/pressure/memory document into dir and returns its
// path, the way the other sources of this package are made testable: a plain
// file the check reads instead of /proc.
func pressureFile(t *testing.T, some, full float64) string {
	t.Helper()
	path := t.TempDir() + "/memory"
	doc := "some avg10=" + ftoa(some) + " avg60=0.00 avg300=0.00 total=0\n" +
		"full avg10=" + ftoa(full) + " avg60=0.00 avg300=0.00 total=0\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// ftoa formats a pressure value the way the kernel does.
func ftoa(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// TestReadPressureReadsTheKernelsNumbers: the some and full avg10 of
// /proc/pressure/memory, which is what box top shows and the journal quotes.
func TestReadPressureReadsTheKernelsNumbers(t *testing.T) {
	got := readPressure(pressureFile(t, 1.25, 12.5))
	if got == nil {
		t.Fatal("a readable pressure file must be read")
	}
	if got.SomeAvg10 != 1.25 || got.FullAvg10 != 12.5 {
		t.Fatalf("pressure = %+v, want some 1.25 and full 12.5", got)
	}
}

// TestReadPressureWithoutTheFile: a kernel without PSI is not an error and says
// nothing about pressure.
func TestReadPressureWithoutTheFile(t *testing.T) {
	if got := readPressure(t.TempDir() + "/no-such-memory-pressure"); got != nil {
		t.Fatalf("a kernel without the file has no pressure to report, got %+v", got)
	}
	if got := readPressure(t.TempDir()); got != nil {
		t.Fatalf("a directory is not a pressure file, got %+v", got)
	}
}

// TestUsageCarriesThePressure: the sampler measures pressure with the rest of
// the load, so box top and box status show it.
func TestUsageCarriesThePressure(t *testing.T) {
	u := &usageSampler{pressureFile: pressureFile(t, 2.5, 0)}
	u.sample(nil)
	usage := u.Last()
	if usage.MemoryPressure == nil {
		t.Fatal("the sample must carry the kernel's pressure")
	}
	if usage.MemoryPressure.SomeAvg10 != 2.5 {
		t.Fatalf("some avg10 = %v, want 2.5", usage.MemoryPressure.SomeAvg10)
	}
	// A kernel without the file leaves the field empty rather than zero.
	u = &usageSampler{pressureFile: t.TempDir() + "/no-such-file"}
	u.sample(nil)
	if got := u.Last().MemoryPressure; got != nil {
		t.Fatalf("no pressure file means no pressure field, got %+v", got)
	}
}

// TestMemoryStallIsSaidOncePerCrossing: a minute of full avg10 above 10% is one
// journal line with what takes the memory, and the way back is one more.
func TestMemoryStallIsSaidOncePerCrossing(t *testing.T) {
	path := pressureFile(t, 30, 25)
	u := &usageSampler{pressureFile: path}
	// The stall started 61 seconds ago: this sample is the one that says so.
	u.memSince = time.Now().Add(-61 * time.Second)

	var said []string
	logf := func(format string, args ...any) {
		said = append(said, fmt.Sprintf(format, args...))
	}
	u.sample(logf)
	if len(said) != 1 {
		t.Fatalf("one line after a minute of stall, got %v", said)
	}
	if !strings.Contains(said[0], "guest is stalled on memory: full avg10=25.0%") {
		t.Fatalf("the line names the full avg10: %q", said[0])
	}
	if !strings.Contains(said[0], "MemAvailable ") || !strings.Contains(said[0], "the biggest processes: ") {
		t.Fatalf("the line says how much is left and what takes it: %q", said[0])
	}

	// A second sample of the same stall says nothing more.
	u.sample(logf)
	if len(said) != 1 {
		t.Fatalf("a stall is said once per crossing, got %v", said)
	}

	// The stall ends: one line, and the next sample is quiet again.
	writePressure(t, path, 40, 1)
	u.sample(logf)
	if len(said) != 2 || !strings.Contains(said[1], "guest memory stall is over: full avg10=1.0%") {
		t.Fatalf("the recovery is one line, got %v", said)
	}
	u.sample(logf)
	if len(said) != 2 {
		t.Fatalf("the recovery is said once, got %v", said)
	}
}

// TestASpikeIsNotAStall: pressure above the threshold for a few seconds says
// nothing, and the clock starts again from the first sample above it.
func TestASpikeIsNotAStall(t *testing.T) {
	u := &usageSampler{pressureFile: pressureFile(t, 50, 90)}
	var said []string
	logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	u.sample(logf)
	if len(said) != 0 {
		t.Fatalf("one sample is not a stall, got %v", said)
	}
	if u.memSince.IsZero() {
		t.Fatal("the first sample above the threshold starts the clock")
	}
}

// writePressure replaces a pressure file with new numbers.
func writePressure(t *testing.T, path string, some, full float64) {
	t.Helper()
	doc := "some avg10=" + ftoa(some) + " avg60=0.00 avg300=0.00 total=0\n" +
		"full avg10=" + ftoa(full) + " avg60=0.00 avg300=0.00 total=0\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestEarlyoomTroubleMatchesWhatEarlyoomSays: the filter has to pass the lines
// that say the guest ran out of memory and which process was killed, and drop
// the ones that are not news.
func TestEarlyoomTroubleMatchesWhatEarlyoomSays(t *testing.T) {
	want := []string{
		// earlyoom 1.9, as it writes them to the journal (without its prefix).
		`low memory! at or below SIGTERM limits: mem 10.00%, swap 10.00%`,
		`Low memory! Killing process 4711 "cc1"`,
		`sending SIGTERM to process 4711 uid 0 "cc1": oom_score 1213, oom_score_adj 0, VmRSS 812 MiB, cmdline "cc1 -c big.c"`,
		`sending SIGKILL to process 4711 uid 0 "cc1": oom_score 1433, oom_score_adj 0, VmRSS 812 MiB, cmdline "cc1 -c big.c"`,
		`escalating to SIGKILL after 10.000 seconds`,
	}
	for _, line := range want {
		if !earlyoomTrouble.MatchString(line) {
			t.Fatalf("earlyoom's %q must reach the task log", line)
		}
	}
	for _, line := range []string{
		`mem avail:  6637 of  9882 MiB (67.16%), swap free: 3099 of 5980 MiB (51.83%)`,
		`Will avoid killing process names that match regex '(^|/)(boxd|sshd)$'`,
		`sending SIGTERM when mem avail <= 10.00% and swap free <= 10.00%,`,
		`earlyoom 1.9.0`,
		`dryrun, not actually sending any signal`,
	} {
		if earlyoomTrouble.MatchString(line) {
			t.Fatalf("%q is not news and must stay out of the task log", line)
		}
	}
}

// TestWatchEarlyOOMWritesWhatItCaught: the watcher follows the earlyoom unit and
// writes its own lines to the journal under a label of their own, one per line
// that matters.
func TestWatchEarlyOOMWritesWhatItCaught(t *testing.T) {
	dir := t.TempDir()
	// A fake journalctl, which is how the watcher is testable without a guest:
	// it prints one line that matters per invocation and then exits.
	script := "#!/bin/sh\n" +
		"printf 'mem avail: 900 of 1960 MiB (45%), swap free: 0 of 0 MiB (0%)\\n'\n" +
		"printf 'low memory! at or below SIGTERM limits: mem 10.00%, swap 10.00%\\n'\n" +
		"printf 'sending SIGTERM to process 4711 uid 0 \"cc1\": oom_score 1213, VmRSS 812 MiB\\n'\n"
	bin := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake journalctl: %v", err)
	}
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 4)
	go watchEarlyOOM(ctx, func(format string, args ...any) {
		lines <- fmt.Sprintf(format, args...)
	})
	select {
	case line := <-lines:
		if !strings.HasPrefix(line, "guest earlyoom: ") {
			t.Fatalf("the line names its source: %q", line)
		}
		if !strings.Contains(line, `sending SIGTERM to process 4711`) {
			t.Fatalf("the line says what earlyoom killed: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher wrote nothing")
	}
	select {
	case line := <-lines:
		t.Fatalf("only the lines that matter are written, got %q", line)
	case <-time.After(100 * time.Millisecond):
	}
}
