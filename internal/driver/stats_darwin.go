//go:build darwin

package driver

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type procUsage struct {
	cpuNs    float64
	rssBytes uint64
}

// processUsage reads cpu time and resident memory of one process with ps(1):
// macOS has no procfs, but ps reports both values for any pid of the user.
func processUsage(pid int) (procUsage, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-o", "cputime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return procUsage{}, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return procUsage{}, fmt.Errorf("ps -p %d returned %q", pid, strings.TrimSpace(string(out)))
	}
	rssKiB, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return procUsage{}, fmt.Errorf("parse rss of pid %d: %w", pid, err)
	}
	cpuSeconds, err := parseCPUTime(fields[1])
	if err != nil {
		return procUsage{}, fmt.Errorf("parse cputime of pid %d: %w", pid, err)
	}
	return procUsage{cpuNs: cpuSeconds * 1e9, rssBytes: rssKiB * 1024}, nil
}

// hypervisorPID finds the Virtualization.framework process of the box: the one
// holding a file in the box's directories open.
func hypervisorPID(paths []string) int {
	ps, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return 0
	}
	for _, pid := range vzServicePIDs(string(ps)) {
		// lsof exits 1 for any file it could not read; what it did print counts.
		out, _ := exec.Command("lsof", "-nP", "-Fn", "-p", strconv.Itoa(pid)).Output()
		if lsofHolds(string(out), paths) {
			return pid
		}
	}
	return 0
}

// parseCPUTime parses ps cputime, which is [dd-]hh:mm:ss[.ss] or mm:ss[.ss].
func parseCPUTime(s string) (float64, error) {
	days := 0
	if i := strings.Index(s, "-"); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, err
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var hours, minutes int
	var seconds float64
	switch len(parts) {
	case 3:
		h, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		hours = h
		parts = parts[1:]
		fallthrough
	case 2:
		m, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		minutes = m
		sec, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
		seconds = sec
	default:
		return 0, fmt.Errorf("unexpected cputime %q", s)
	}
	return float64(days*24*3600+hours*3600+minutes*60) + seconds, nil
}
