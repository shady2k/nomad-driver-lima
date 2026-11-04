package boxd

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolInputSaysWhatRuns: a Python cell or a shell shows its code, without
// the comment lines in front of it.
func TestToolInputSaysWhatRuns(t *testing.T) {
	cases := map[string]string{
		`{"code":"# check the tree\nimport subprocess\nsubprocess.run(['go','test','./...'])"}`: "import subprocess ⏎ subprocess.run(['go','test','./...'])",
		`{"command":["bash","-c","make lint"]}`:                                                 "bash -c make lint",
		`{"file_path":"/work/nocx/main.go"}`:                                                    "/work/nocx/main.go",
		`"ls -la"`:                                                                              "ls -la",
		``:                                                                                      "",
	}
	for raw, want := range cases {
		if got := toolInput(json.RawMessage(raw)); got != want {
			t.Errorf("toolInput(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := toolLine("Python cell", "import os"); got != "Python cell: import os" {
		t.Fatalf("toolLine = %q", got)
	}
}

// TestResultLineKeepsTheEnd: the exit code and time come first, every content
// block counts, and a long output keeps its end, where the errors are.
func TestResultLineKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("ok package line ", 60) + "FAIL nocx/update 3.2s"
	content, _ := json.Marshal([]map[string]any{
		{"type": "content", "content": map[string]string{"type": "text", "text": "pid 9915"}},
		{"type": "content", "content": map[string]string{"type": "text", "text": long}},
	})
	got := resultLine(content, json.RawMessage(`{"exit_code":1,"elapsed":12.34}`))
	for _, want := range []string{"exit 1, 12.3s: pid 9915", " … ", "FAIL nocx/update 3.2s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("resultLine = %q, want %q in it", got, want)
		}
	}
	if len(got) > 450 {
		t.Fatalf("resultLine is %d bytes", len(got))
	}
	if got := resultLine(nil, json.RawMessage(`{"returncode":0,"stdout":"done"}`)); got != "exit 0: done" {
		t.Fatalf("raw output only = %q", got)
	}
}
