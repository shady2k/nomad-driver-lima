// Package version carries the build version of the box driver and volume
// plugin.
package version

// Version is the release version, overridable at build time with
//
//	-ldflags "-X github.com/shady2k/nomad-driver-lima/internal/version.Version=x.y.z"
var Version = "0.1.0-dev"
