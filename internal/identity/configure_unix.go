//go:build linux || darwin

package identity

import (
	"os"
	"os/exec"
	"syscall"
)

// Configure makes cmd run as the identity when the driver runs as root: the
// child gets the user's uid, gid and supplementary groups. When the driver is
// not root the child inherits the driver's own identity, and setting a
// credential would fail with EPERM.
func (i *Identity) Configure(cmd *exec.Cmd) {
	if i == nil || !i.Drop {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: i.Credential()}
}

// Credential is the uid, gid and supplementary groups of the identity.
func (i *Identity) Credential() *syscall.Credential {
	return &syscall.Credential{
		Uid:    uint32(i.UID),
		Gid:    uint32(i.GID),
		Groups: append([]uint32(nil), i.Groups...),
	}
}

// statUID returns the owner of a file, or -1 when the platform does not say.
func statUID(st os.FileInfo) int {
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(raw.Uid)
}
