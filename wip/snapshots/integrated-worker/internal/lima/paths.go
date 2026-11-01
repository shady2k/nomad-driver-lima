package lima

import (
	"fmt"
	"path/filepath"
	"runtime"
)

// Upstream creates a temporary OpenSSH ControlPath with this 16-digit suffix.
// Validate bytes before issuing create, including Darwin's smaller sun_path.
func ValidateHome(home string) error {
	max := 108
	if runtime.GOOS == "darwin" {
		max = 104
	}
	name := "nomad-000000000000000000000000"
	n := len(filepath.Join(home, name, "ssh.sock.1234567890123456"))
	if n >= max {
		return fmt.Errorf("lima_home too long for upstream UNIX socket: %d bytes, must be less than %d; choose a shorter operator path", n, max)
	}
	return nil
}
