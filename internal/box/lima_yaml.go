package box

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.yaml.in/yaml/v3"
)

// VM types supported by Lima. Linux defaults to qemu (KVM), darwin to vz.
const (
	VMTypeQEMU = "qemu"
	VMTypeVZ   = "vz"
)

// DefaultVMType returns the Lima VM type for the current OS.
func DefaultVMType() string {
	if runtime.GOOS == "darwin" {
		return VMTypeVZ
	}
	return VMTypeQEMU
}

// How the driver enforces the memory budget of a box.
const (
	// MemoryLimitEnforce puts every VM into a systemd user scope with
	// memory.max = guest + overhead and memory.swap.max = 0, and fails the task
	// when that scope cannot be made. A box without its limit is not what the
	// task asked for, so there is no silent fallback.
	MemoryLimitEnforce = "enforce"
	// MemoryLimitOff is the operator's explicit switch for a node without
	// cgroup v2. The fingerprint reports it, so a node that runs boxes without
	// a memory cap is visible in the cluster.
	MemoryLimitOff = "off"
)

// DefaultMemoryLimit returns the memory limit mode for the current OS: Linux can
// enforce it, macOS has no cgroups and can only be told it is off.
func DefaultMemoryLimit() string {
	if runtime.GOOS == "linux" {
		return MemoryLimitEnforce
	}
	return MemoryLimitOff
}

// LimaArch maps the Go architecture to Lima's arch field.
func LimaArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	case "riscv64":
		return "riscv64"
	case "arm":
		return "armv7l"
	case "s390x":
		return "s390x"
	case "ppc64le":
		return "ppc64le"
	}
	return runtime.GOARCH
}

// limaYAML is the subset of the Lima configuration file the driver writes.
type limaYAML struct {
	MinimumLimaVersion string        `yaml:"minimumLimaVersion,omitempty"`
	VMType             string        `yaml:"vmType,omitempty"`
	Arch               string        `yaml:"arch,omitempty"`
	Images             []limaImage   `yaml:"images,omitempty"`
	CPUs               int           `yaml:"cpus"`
	Memory             string        `yaml:"memory"`
	Disk               string        `yaml:"disk"`
	MountType          string        `yaml:"mountType,omitempty"`
	Mounts             []limaMount   `yaml:"mounts"`
	Containerd         limaContainer `yaml:"containerd"`
}

// limaMount is a read-only host directory in the guest. qemu shares it over
// 9p; vz shares it over virtiofs.
//
// The 9p cache is per mount: the task files are mounted without a cache, so a
// file the driver replaces is the new one in the guest at once, while the
// platform bundle is mounted with fscache, because its directory never changes
// (a new bundle is a new directory).
type limaMount struct {
	Location   string    `yaml:"location"`
	MountPoint string    `yaml:"mountPoint"`
	Writable   bool      `yaml:"writable"`
	NineP      *limaNine `yaml:"9p,omitempty"`
	// Cache is the 9p cache mode of this mount on qemu: CacheNone (default) or
	// CacheFscache. It is not a YAML field of its own; it fills NineP.
	Cache string `yaml:"-"`
}

// 9p cache modes of a mount.
const (
	// CacheNone re-reads every file the guest opens, which is what a directory
	// the driver replaces files in needs.
	CacheNone = "none"
	// CacheFscache caches the attributes and the content of a directory that
	// never changes after it is mounted, which is a platform bundle.
	CacheFscache = "fscache"
)

type limaNine struct {
	Cache string `yaml:"cache"`
}

type limaImage struct {
	Location string `yaml:"location"`
	Arch     string `yaml:"arch,omitempty"`
}

type limaContainer struct {
	System bool `yaml:"system"`
	User   bool `yaml:"user"`
}

// renderLimaYAML builds the configuration for a box. The image location is an
// absolute host path into the driver's image cache. The guest gets three host
// mounts at most, all read-only: the box's own task files (files.go), the
// platform bundle at /opt/box when the task names one (platform.go), and nothing
// else of the Nomad client's filesystem.
func renderLimaYAML(spec Spec, imagePath string, mounts []limaMount) ([]byte, error) {
	if spec.CPUs <= 0 {
		return nil, fmt.Errorf("cpus must be positive, got %d", spec.CPUs)
	}
	if spec.MemoryMiB <= 0 {
		return nil, fmt.Errorf("memory_mib must be positive, got %d", spec.MemoryMiB)
	}
	if spec.DiskGiB <= 0 {
		return nil, fmt.Errorf("disk_gib must be positive, got %d", spec.DiskGiB)
	}
	vmType := spec.VMType
	if vmType == "" {
		vmType = DefaultVMType()
	}
	doc := limaYAML{
		MinimumLimaVersion: "2.0.0",
		VMType:             vmType,
		Arch:               LimaArch(),
		Images:             []limaImage{{Location: imagePath, Arch: LimaArch()}},
		CPUs:               spec.CPUs,
		Memory:             fmt.Sprintf("%dMiB", spec.MemoryMiB),
		Disk:               fmt.Sprintf("%dGiB", spec.DiskGiB),
		Mounts:             []limaMount{},
		Containerd:         limaContainer{System: false, User: false},
	}
	for _, mnt := range mounts {
		mnt.Writable = false
		if vmType == VMTypeQEMU {
			cache := mnt.Cache
			if cache == "" {
				cache = CacheNone
			}
			mnt.NineP = &limaNine{Cache: cache}
		}
		doc.Mounts = append(doc.Mounts, mnt)
	}
	if len(doc.Mounts) > 0 {
		doc.MountType = "9p"
		if vmType == VMTypeVZ {
			doc.MountType = "virtiofs"
		}
	}
	return yaml.Marshal(doc)
}

// writeLimaYAML writes the configuration next to the instance directory so
// limactl create can read it, and returns the path. limactl runs as the runtime
// user, so the file and the Lima home it lives in belong to that user.
func (m *Manager) writeLimaYAML(boxID string, content []byte) (string, error) {
	home := m.opts.LimaHome
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", fmt.Errorf("create lima home %s: %w", home, err)
	}
	if err := m.opts.Identity.Chown(home); err != nil {
		return "", err
	}
	path := filepath.Join(home, strings.TrimSuffix(boxID, ".yaml")+".yaml")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", fmt.Errorf("write lima config %s: %w", path, err)
	}
	if err := m.opts.Identity.Chown(path); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
