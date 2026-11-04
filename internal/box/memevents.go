package box

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MemoryEvents reads memory.events of the cgroup the process pid runs in (the
// box's scope, for its qemu): "oom" and "oom_kill" count what the kernel did
// when the box ran out of its memory.max. A box whose qemu is gone has no
// cgroup left, so the driver reads it while the VM runs. nil when there is
// none to read (macOS, no cgroup v2).
func MemoryEvents(pid int) map[string]uint64 {
	if pid <= 0 {
		return nil
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return nil
	}
	var rel string
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel = strings.TrimSpace(p)
		}
	}
	if rel == "" {
		return nil
	}
	return readMemoryEvents(filepath.Join("/sys/fs/cgroup", rel, "memory.events"))
}

func readMemoryEvents(path string) map[string]uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			v, err := strconv.ParseUint(f[1], 10, 64)
			if err == nil {
				out[f[0]] = v
			}
		}
	}
	return out
}
