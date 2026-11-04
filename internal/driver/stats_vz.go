package driver

import (
	"strconv"
	"strings"
)

// vzServiceName is the process that runs a VM on macOS. It is an XPC service of
// Virtualization.framework, a child of launchd: Lima's vz.pid names the
// hostagent that asked for it, not this process, so the CPU and memory of the VM
// are only found here.
const vzServiceName = "com.apple.Virtualization.VirtualMachine"

// vzServicePIDs picks the VM processes out of `ps -axo pid=,comm=`.
func vzServicePIDs(ps string) []int {
	var pids []int
	for _, line := range strings.Split(ps, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasSuffix(strings.Join(fields[1:], " "), vzServiceName) {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// lsofHolds reports whether `lsof -Fn` output names a file at or below one of
// the paths. A box's disk lies in its own directories, so an open file there says
// the process runs this box.
func lsofHolds(lsof string, paths []string) bool {
	for _, line := range strings.Split(lsof, "\n") {
		if !strings.HasPrefix(line, "n/") {
			continue
		}
		name := line[1:]
		for _, path := range paths {
			if path != "" && (name == path || strings.HasPrefix(name, strings.TrimSuffix(path, "/")+"/")) {
				return true
			}
		}
	}
	return false
}
