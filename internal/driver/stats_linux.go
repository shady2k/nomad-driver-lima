//go:build linux

package driver

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type procUsage struct {
	cpuNs    float64
	rssBytes uint64
}

// userHZ is the Linux USER_HZ the /proc/<pid>/stat times are measured in.
const userHZ = 100

// processUsage reads cpu time and resident memory of one process from procfs.
func processUsage(pid int) (procUsage, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procUsage{}, err
	}
	line := string(data)
	// The comm field is in parentheses and may contain spaces; everything after
	// the last ')' starts at field 3 (state).
	idx := strings.LastIndex(line, ")")
	if idx < 0 || idx+2 >= len(line) {
		return procUsage{}, fmt.Errorf("parse /proc/%d/stat: unexpected format", pid)
	}
	fields := strings.Fields(line[idx+2:])
	// Fields in /proc/pid/stat are 1-based: utime 14, stime 15, rss 24.
	if len(fields) < 22 {
		return procUsage{}, fmt.Errorf("parse /proc/%d/stat: only %d fields", pid, len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return procUsage{}, fmt.Errorf("parse utime of pid %d: %w", pid, err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return procUsage{}, fmt.Errorf("parse stime of pid %d: %w", pid, err)
	}
	rssPages, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return procUsage{}, fmt.Errorf("parse rss of pid %d: %w", pid, err)
	}
	if rssPages < 0 {
		rssPages = 0
	}
	return procUsage{
		cpuNs:    float64(utime+stime) * (1e9 / userHZ),
		rssBytes: uint64(rssPages) * uint64(os.Getpagesize()),
	}, nil
}

// hypervisorPID is for a VM that runs outside Lima's pid files. On Linux qemu is
// in them.
func hypervisorPID(paths []string) int { return 0 }
