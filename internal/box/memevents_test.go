package box

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMemoryEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.events")
	if err := os.WriteFile(path, []byte("low 0\nhigh 0\nmax 1520\noom 2\noom_kill 1\noom_group_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := readMemoryEvents(path)
	if ev["oom_kill"] != 1 || ev["oom"] != 2 || ev["max"] != 1520 {
		t.Fatalf("events = %v", ev)
	}
	if readMemoryEvents(filepath.Join(t.TempDir(), "none")) != nil {
		t.Fatal("a missing file is no events")
	}
	if MemoryEvents(0) != nil {
		t.Fatal("no pid, no events")
	}
}
