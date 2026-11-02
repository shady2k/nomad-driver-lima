package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/nomad/plugins/shared/hclspec"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/identity"
)

const (
	// defaultDiskVolumeRequest is the request name of the dynamic host volume
	// that holds the box disk. The driver reads its host path from
	// TaskConfig.mounts[].host_path.
	defaultDiskVolumeRequest = "disk"

	defaultCPUs        = 2
	defaultMemoryMiB   = 1024
	defaultDiskGiB     = 8
	defaultFilesDir    = "/etc/box"
	defaultStopTimeout = 60 * time.Second
)

// defaultGuestLogCommand streams the box daemon log into the Nomad task log.
//
// The journal of another process is only readable for a user in adm or
// systemd-journal, and the task runs as the box user, so the command asks sudo
// (passwordless in a box guest, like every other privileged step there). Without
// it the task log would stay empty while boxd is full of events.
var defaultGuestLogCommand = []string{"sudo", "-n", "journalctl", "-f", "-o", "cat", "-u", "boxd"}

// Config is the plugin configuration from the client's `plugin "box"` block.
type Config struct {
	// LimactlPath is the limactl executable (default "limactl" from PATH).
	LimactlPath string `codec:"limactl"`
	// LimaHome is LIMA_HOME for the boxes this driver owns.
	LimaHome string `codec:"lima_home"`
	// ImageCacheDir holds downloaded cloud images keyed by sha256.
	ImageCacheDir string `codec:"image_cache_dir"`
	// MemoryOverheadMiB is added to guest memory for the Linux cgroup cap.
	MemoryOverheadMiB int `codec:"memory_overhead_mib"`
	// MemoryLimit says whether the driver must be able to put the VM under a
	// memory cap: "enforce" (the default on Linux) fails the task when the
	// systemd user scope cannot be created, so a box never runs unlimited by
	// accident; "off" is the operator's explicit switch for a node that has no
	// cgroup v2 (and the only possible value on macOS, which has no cgroups).
	MemoryLimit string `codec:"memory_limit"`
	// VMType is qemu (Linux) or vz (macOS); empty means the OS default.
	VMType string `codec:"vm_type"`
	// RuntimeUser is the unprivileged user every Lima process runs as. Lima
	// refuses to run as root, so a driver that runs as root (the Nomad client on
	// a Linux node) requires it; a driver that runs as another user may leave it
	// empty or name its own user.
	RuntimeUser string `codec:"runtime_user"`

	// identity is the resolved RuntimeUser; identityErr is why it could not be
	// resolved. A root client without runtime_user is reported by the
	// fingerprint and by every start, not by a plugin that fails to load.
	identity    *identity.Identity
	identityErr error
}

// configSpec is the HCL schema of the plugin block.
var configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"limactl": hclspec.NewDefault(
		hclspec.NewAttr("limactl", "string", false),
		hclspec.NewLiteral(`"limactl"`),
	),
	"lima_home": hclspec.NewDefault(
		hclspec.NewAttr("lima_home", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"image_cache_dir": hclspec.NewDefault(
		hclspec.NewAttr("image_cache_dir", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"memory_overhead_mib": hclspec.NewDefault(
		hclspec.NewAttr("memory_overhead_mib", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", box.DefaultMemoryOverheadMiB)),
	),
	"memory_limit": hclspec.NewDefault(
		hclspec.NewAttr("memory_limit", "string", false),
		hclspec.NewLiteral(`"enforce"`),
	),
	"vm_type": hclspec.NewDefault(
		hclspec.NewAttr("vm_type", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"runtime_user": hclspec.NewDefault(
		hclspec.NewAttr("runtime_user", "string", false),
		hclspec.NewLiteral(`""`),
	),
})

// validate fills in defaults and rejects configurations that cannot work.
func (c *Config) validate() error {
	if c.LimactlPath == "" {
		c.LimactlPath = "limactl"
	}
	if c.MemoryOverheadMiB <= 0 {
		c.MemoryOverheadMiB = box.DefaultMemoryOverheadMiB
	}
	if c.MemoryLimit == "" {
		c.MemoryLimit = box.DefaultMemoryLimit()
	}
	switch c.MemoryLimit {
	case box.MemoryLimitEnforce:
		if runtime.GOOS != "linux" {
			return fmt.Errorf("memory_limit = %q needs cgroup v2, which %s does not have: set memory_limit = %q", box.MemoryLimitEnforce, runtime.GOOS, box.MemoryLimitOff)
		}
	case box.MemoryLimitOff:
	default:
		return fmt.Errorf("memory_limit must be %q or %q, got %q", box.MemoryLimitEnforce, box.MemoryLimitOff, c.MemoryLimit)
	}
	if c.VMType == "" {
		c.VMType = box.DefaultVMType()
	}
	switch c.VMType {
	case box.VMTypeQEMU:
	case box.VMTypeVZ:
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("vm_type %q is only available on macOS; use %q on %s", box.VMTypeVZ, box.VMTypeQEMU, runtime.GOOS)
		}
	default:
		return fmt.Errorf("vm_type must be %q or %q, got %q", box.VMTypeQEMU, box.VMTypeVZ, c.VMType)
	}
	if c.LimaHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve lima_home: %w", err)
		}
		c.LimaHome = filepath.Join(home, ".lima")
	}
	if c.ImageCacheDir == "" {
		c.ImageCacheDir = filepath.Join(c.LimaHome, "_image-cache")
	}
	c.identity, c.identityErr = identity.Resolve(c.RuntimeUser)
	return nil
}

// ImageConfig is the task's image block.
type ImageConfig struct {
	URL    string `codec:"url"`
	SHA256 string `codec:"sha256"`
}

// TaskConfig is the driver configuration of one box task.
type TaskConfig struct {
	BoxID           string      `codec:"box_id"`
	Image           ImageConfig `codec:"image"`
	CPUs            int         `codec:"cpus"`
	MemoryMiB       int         `codec:"memory_mib"`
	DiskGiB         int         `codec:"disk_gib"`
	GuestLogCommand []string    `codec:"guest_log_command"`
	// FilesDirInGuest is the root of the task's local/ directory in the guest:
	// its files land in <FilesDirInGuest>/local. Job files are not secrets, so
	// they live on the box disk.
	FilesDirInGuest string `codec:"files_dir_in_guest"`
	// SecretsDirInGuest is where the task's secrets/ directory lands: a tmpfs of
	// the guest, owned by root with mode 0700. A credential must never be on the
	// box disk, and boxd reads this directory (see internal/boxd).
	SecretsDirInGuest string `codec:"secrets_dir_in_guest"`
}

// taskConfigSpec is the HCL schema of the task's driver block.
var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"box_id": hclspec.NewAttr("box_id", "string", true),
	"image": hclspec.NewBlock("image", true, hclspec.NewObject(map[string]*hclspec.Spec{
		"url":    hclspec.NewAttr("url", "string", true),
		"sha256": hclspec.NewAttr("sha256", "string", true),
	})),
	"cpus": hclspec.NewDefault(
		hclspec.NewAttr("cpus", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", defaultCPUs)),
	),
	"memory_mib": hclspec.NewDefault(
		hclspec.NewAttr("memory_mib", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", defaultMemoryMiB)),
	),
	"disk_gib": hclspec.NewDefault(
		hclspec.NewAttr("disk_gib", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", defaultDiskGiB)),
	),
	"guest_log_command": hclspec.NewDefault(
		hclspec.NewAttr("guest_log_command", "list(string)", false),
		hclspec.NewLiteral(`["sudo", "-n", "journalctl", "-f", "-o", "cat", "-u", "boxd"]`),
	),
	"files_dir_in_guest": hclspec.NewDefault(
		hclspec.NewAttr("files_dir_in_guest", "string", false),
		hclspec.NewLiteral(`"/etc/box"`),
	),
	"secrets_dir_in_guest": hclspec.NewDefault(
		hclspec.NewAttr("secrets_dir_in_guest", "string", false),
		hclspec.NewLiteral(`"`+box.DefaultSecretsDirInGuest+`"`),
	),
})

// applyDefaults fills the optional fields and validates the required ones.
func (c *TaskConfig) applyDefaults() error {
	if c.CPUs == 0 {
		c.CPUs = defaultCPUs
	}
	if c.MemoryMiB == 0 {
		c.MemoryMiB = defaultMemoryMiB
	}
	if c.DiskGiB == 0 {
		c.DiskGiB = defaultDiskGiB
	}
	if len(c.GuestLogCommand) == 0 {
		c.GuestLogCommand = append([]string(nil), defaultGuestLogCommand...)
	}
	if c.FilesDirInGuest == "" {
		c.FilesDirInGuest = defaultFilesDir
	}
	if c.SecretsDirInGuest == "" {
		c.SecretsDirInGuest = box.DefaultSecretsDirInGuest
	}
	// The secrets directory is a tmpfs of the guest, not a place on its disk: a
	// path outside /run would put credentials on the box disk (or, worse, on the
	// host through a mount).
	if !strings.HasPrefix(filepath.Clean(c.SecretsDirInGuest), "/run/") {
		return fmt.Errorf("secrets_dir_in_guest %q must be below /run: the executor credentials live on the guest's tmpfs and must never reach the box disk", c.SecretsDirInGuest)
	}
	if c.BoxID == "" {
		return fmt.Errorf("box_id is required")
	}
	if c.Image.URL == "" {
		return fmt.Errorf("image.url is required")
	}
	if c.Image.SHA256 == "" {
		return fmt.Errorf("image.sha256 is required")
	}
	if c.CPUs <= 0 || c.MemoryMiB <= 0 || c.DiskGiB <= 0 {
		return fmt.Errorf("cpus, memory_mib and disk_gib must be positive")
	}
	return nil
}
