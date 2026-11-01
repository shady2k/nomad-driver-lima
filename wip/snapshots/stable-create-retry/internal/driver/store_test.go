package driver

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestTaskLockCannotLeakAcrossExec(t *testing.T) {
	s, e := openStore(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	f, e := s.lock("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if e != nil {
		t.Fatal(e)
	}
	defer unlock(f)
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("runtime children would inherit task ownership lock after driver SIGKILL")
	}
}
