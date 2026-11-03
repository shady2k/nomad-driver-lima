package box

import (
	"fmt"
	"path/filepath"
	"runtime"
)

// Lima keeps its sockets in the instance directory, <lima_home>/<box id>/, and
// the longest path it binds there is ssh's: the ControlMaster binds
// ssh.sock.<16 random characters> and renames it to ssh.sock. A unix socket
// path is limited by the kernel (sun_path: 104 bytes on macOS, 108 on Linux,
// with the NUL), and a path over it does not fail the start: ssh cannot set up
// its master connection, and Lima retries its readiness checks until the start
// times out.
const (
	// sshControlSuffix is what ssh appends to ControlPath while it binds.
	sshControlSuffix = ".XXXXXXXXXXXXXXXX"
	// MaxBoxIDLen is the longest box id the driver accepts.
	MaxBoxIDLen = 40
)

// socketPathMax is the longest socket path this kernel accepts, without the NUL.
func socketPathMax() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// limaSocketPath is the longest socket path Lima binds for a box.
func limaSocketPath(limaHome, boxID string) string {
	return filepath.Join(limaHome, boxID, "ssh.sock") + sshControlSuffix
}

// CheckSocketPath reports whether Lima can bind its sockets for boxID under
// limaHome. An empty boxID checks the longest box id the driver accepts.
func CheckSocketPath(limaHome, boxID string) error {
	if boxID == "" {
		boxID = fmt.Sprintf("%0*d", MaxBoxIDLen, 0)
	}
	path := limaSocketPath(limaHome, boxID)
	if n, max := len(path), socketPathMax(); n > max {
		return fmt.Errorf("lima_home %s is too long: Lima's ssh socket for a %d-character box id would be %d bytes, "+
			"and %s accepts %d; %s - choose a shorter lima_home",
			limaHome, len(boxID), n, runtime.GOOS, max, roomText(MaxBoxIDFor(limaHome)))
	}
	return nil
}

// MaxBoxIDFor returns the longest box id whose sockets fit under limaHome,
// at most MaxBoxIDLen; 0 means none fits.
func MaxBoxIDFor(limaHome string) int {
	// limaSocketPath with an empty id lacks the separator the id brings.
	room := socketPathMax() - len(limaSocketPath(limaHome, "")) - 1
	switch {
	case room < 1:
		return 0
	case room > MaxBoxIDLen:
		return MaxBoxIDLen
	}
	return room
}

func roomText(room int) string {
	if room < 1 {
		return "no box id fits"
	}
	return fmt.Sprintf("box ids of at most %d characters fit", room)
}
