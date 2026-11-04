package boxd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// defaultPlatformDir is where the driver mounts the platform bundle: a
// read-only directory the box carries instead of the executors in its image.
const defaultPlatformDir = "/opt/box"

// platformDir is the root of the platform bundle, the directory the box mounts
// read-only at /opt/box. A bundle is a directory with a manifest.json (the
// version it is, the boxd it carries, the tools it holds) and a bin/ of
// executables the daemon and its executors run.
func (c DaemonConfig) platformDir() string {
	if c.PlatformDir != "" {
		return c.PlatformDir
	}
	return defaultPlatformDir
}

// platformBinDir is the bundle's bin: the executors the box runs, in front of
// whatever PATH the image gives it.
func (c DaemonConfig) platformBinDir() string {
	return filepath.Join(c.platformDir(), "bin")
}

// platformManifest is manifest.json of a platform bundle. The bundle's builder
// writes it; boxd only reads it.
type platformManifest struct {
	Version string            `json:"version"`
	Boxd    string            `json:"boxd"`
	Tools   map[string]string `json:"tools"`
}

// readPlatform reads the bundle mounted at dir as the guest sees it.
//
// A box without a bundle is not an error: an image that carries boxd and the
// executors itself is still a box, and the info document says so with Mounted
// false and a reason that names the path that was looked at.
func readPlatform(dir string) PlatformInfo {
	info := PlatformInfo{Path: dir}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			info.Reason = fmt.Sprintf("no bundle is mounted at %s", dir)
			return info
		}
		info.Reason = unreadableBundle(dir, err)
		return info
	}
	var m platformManifest
	if err := json.Unmarshal(b, &m); err != nil {
		info.Reason = unreadableBundle(dir, err)
		return info
	}
	if m.Version == "" {
		// A bundle whose manifest names no version cannot be reported as one: the
		// operator of the box would have no way to say which bundle it runs.
		info.Reason = unreadableBundle(dir, fmt.Errorf("it names no version"))
		return info
	}
	info.Mounted = true
	info.Version = m.Version
	info.Boxd = m.Boxd
	info.Tools = m.Tools
	return info
}

// unreadableBundle is the reason for a manifest.json that is there but cannot be
// used.
func unreadableBundle(dir string, err error) string {
	return fmt.Sprintf("the bundle at %s has no readable manifest.json: %v", dir, err)
}
