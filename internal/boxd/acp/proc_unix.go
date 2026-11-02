//go:build unix

package acp

import (
	"os"
	"syscall"
)

// killGroup kills the whole process group rooted at pid, falling back to the
// process itself when the group is gone.
func killGroup(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
