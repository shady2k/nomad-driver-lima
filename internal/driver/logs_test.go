package driver

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestLineWriterKeepsLinesWhole: a note written while the guest's journal is
// in the middle of a line goes after that line, not inside it.
func TestLineWriterKeepsLinesWhole(t *testing.T) {
	var out bytes.Buffer
	lw := &lineWriter{w: &out}
	_, _ = io.WriteString(lw, "journal: half")
	_, _ = io.WriteString(lw, "box exec: started: true\n")
	_, _ = io.WriteString(lw, " a line\njournal: next")
	lw.flush()
	want := "journal: halfbox exec: started: true\n a line\njournal: next\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Lines written as whole lines stay whole and in order.
	out.Reset()
	lw = &lineWriter{w: &out}
	_, _ = io.WriteString(lw, "one\n")
	_, _ = io.WriteString(lw, "two\nthree\n")
	if got := strings.Split(strings.TrimSpace(out.String()), "\n"); len(got) != 3 {
		t.Fatalf("lines = %q", got)
	}
}

// TestQuietExecIsOnlyTheReadingBoxctlCalls: the CLI's polling stays out of the
// task log; whatever changes something stays in it.
func TestQuietExecIsOnlyTheReadingBoxctlCalls(t *testing.T) {
	b := "/usr/local/bin/boxctl"
	for _, c := range [][]string{{b, "info"}, {b, "wait", "s-1"}, {b, "events", "s-1"}, {b, "session", "ls"},
		{b, "session", "get", "s-1"}, {b, "prepare", "wait", "--timeout", "30"}, {b, "prepare", "get"}} {
		if !quietExec(c) {
			t.Errorf("%v should stay out of the task log", c)
		}
	}
	for _, c := range [][]string{{b, "session", "new"}, {b, "send", "s-1"}, {b, "cancel", "s-1"}, {b, "prepare", "start"},
		{b, "prepare", "cancel"}, {b, "reload"}, {"git", "clone", "x"}, {"tar", "-xf", "-"}, {"boxctl", "info"}} {
		if quietExec(c) {
			t.Errorf("%v should be in the task log", c)
		}
	}
}

// TestExecLabelOfABoxRun: the log names the command a `box run` ran, not the
// wrapper around it.
func TestExecLabelOfABoxRun(t *testing.T) {
	got := execLabel([]string{"setsid", "-w", "sh", "-c", `echo $$ > "$0"; "$@"`, "/tmp/box-run-1-2.pid", "go", "test", "./..."})
	if got != "go test ./..." {
		t.Fatalf("label = %q", got)
	}
}

// TestJournalArgv: the configured journalctl runs in JSON, after the cursor of
// the last line shown, else from the boot or from now.
func TestJournalArgv(t *testing.T) {
	base := []string{"sudo", "-n", "journalctl", "-f", "-o", "cat", "-u", "boxd"}
	if !journalStream(base) || journalStream([]string{"tail", "-f", "/var/log/x"}) {
		t.Fatal("journalStream")
	}
	want := map[string][]string{
		"cursor": {"sudo", "-n", "journalctl", "-f", "-u", "boxd", "-o", "json", "--after-cursor=s=abc"},
		"boot":   {"sudo", "-n", "journalctl", "-f", "-u", "boxd", "-o", "json", "-b", "-n", "all"},
		"now":    {"sudo", "-n", "journalctl", "-f", "-u", "boxd", "-o", "json", "-n", "0"},
	}
	got := map[string][]string{
		"cursor": journalArgv(base, "s=abc", true),
		"boot":   journalArgv(base, "", true),
		"now":    journalArgv(base, "", false),
	}
	for k := range want {
		if strings.Join(got[k], " ") != strings.Join(want[k], " ") {
			t.Errorf("%s: %q, want %q", k, got[k], want[k])
		}
	}
}

// TestJournalWriter: the messages go out as lines, a line split across writes
// is whole, and the cursor is the last one's.
func TestJournalWriter(t *testing.T) {
	var out bytes.Buffer
	j := &journalWriter{out: &out}
	_, _ = io.WriteString(j, `{"__CURSOR":"c1","MESSAGE":"started boxd"}`+"\n"+`{"__CURSOR":"c2","MESS`)
	_, _ = io.WriteString(j, `AGE":[104,105]}`+"\n"+`not json`+"\n")
	j.flush()
	if got := out.String(); got != "started boxd\nhi\n" {
		t.Fatalf("out = %q", got)
	}
	if j.cursor != "c2" {
		t.Fatalf("cursor = %q", j.cursor)
	}
}
