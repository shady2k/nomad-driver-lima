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
	// FilesRoot holds every box's task files, which its guest mounts
	// read-only. It must be a tmpfs: the executor credentials are there.
	// Default /run/box-files on Linux.
	FilesRoot string `codec:"files_root"`
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
	// QEMUImg is the qemu-img binary (default "qemu-img" from PATH). It
	// uncompresses an image once per image, and a bake compresses a sealed disk
	// with it.
	QEMUImg string `codec:"qemu_img"`
	// RuntimeUser is the unprivileged user every Lima process runs as. Lima
	// refuses to run as root, so a driver that runs as root (the Nomad client on
	// a Linux node) requires it; a driver that runs as another user may leave it
	// empty or name its own user.
	RuntimeUser string `codec:"runtime_user"`
	// CPUOvercommit is how many vCPUs the node's cores are shared out to (>= 1,
	// fractions allowed, default 1). A box is idle most of the time -- an agent
	// waits for its model -- so a vCPU does not have to reserve a whole core: the
	// `cpu.max` cap of a box (its vCPUs plus one core) is what keeps a busy box
	// from taking the node. The driver only publishes the number: the CLI divides
	// its per-vCPU reservation by it, and Nomad then places that many more boxes.
	CPUOvercommit float64 `codec:"cpu_overcommit"`
	// DiskReserveGiB and DiskReservePercent are the room this node keeps free on
	// the filesystem its boxes write to: the larger of the two, so a small node
	// keeps a workable number of GiB and a large one keeps a share of itself.
	// `box create`, `box start` and a growing `box resize` are refused on a node
	// that is below it, and a running box that reaches zero freezes in qemu. 0
	// for both turns the reserve off.
	DiskReserveGiB     int64 `codec:"disk_reserve_gib"`
	DiskReservePercent int64 `codec:"disk_reserve_percent"`

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
	"files_root": hclspec.NewDefault(
		hclspec.NewAttr("files_root", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"memory_overhead_mib": hclspec.NewDefault(
		hclspec.NewAttr("memory_overhead_mib", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", box.DefaultMemoryOverheadMiB)),
	),
	// Empty, not "enforce": validate picks the platform's default, and a
	// literal here would refuse every macOS node that leaves the key out.
	"memory_limit": hclspec.NewDefault(
		hclspec.NewAttr("memory_limit", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"vm_type": hclspec.NewDefault(
		hclspec.NewAttr("vm_type", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"qemu_img": hclspec.NewDefault(
		hclspec.NewAttr("qemu_img", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"runtime_user": hclspec.NewDefault(
		hclspec.NewAttr("runtime_user", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"cpu_overcommit": hclspec.NewDefault(
		hclspec.NewAttr("cpu_overcommit", "number", false),
		hclspec.NewLiteral("1"),
	),
	// The defaults are literals here and not in validate: 0 is a value the
	// operator asks for ("no reserve"), while a key left out means the default
	// this node was built for.
	"disk_reserve_gib": hclspec.NewDefault(
		hclspec.NewAttr("disk_reserve_gib", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", defaultDiskReserveGiB)),
	),
	"disk_reserve_percent": hclspec.NewDefault(
		hclspec.NewAttr("disk_reserve_percent", "number", false),
		hclspec.NewLiteral(fmt.Sprintf("%d", defaultDiskReservePercent)),
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
	// Zero is "the key was left out": the public default is 1, so a node that does
	// not ask for overcommit reserves a core for a vCPU exactly as before.
	if c.CPUOvercommit == 0 {
		c.CPUOvercommit = 1
	}
	if !(c.CPUOvercommit >= 1) {
		return fmt.Errorf("cpu_overcommit must be at least 1 (1 disables it, 2 shares a core between two vCPUs), got %v", c.CPUOvercommit)
	}
	if c.DiskReserveGiB < 0 || c.DiskReservePercent < 0 {
		return fmt.Errorf("disk_reserve_gib and disk_reserve_percent must not be negative (0 for both turns the reserve off), got %d and %d",
			c.DiskReserveGiB, c.DiskReservePercent)
	}
	if c.DiskReservePercent > 100 {
		return fmt.Errorf("disk_reserve_percent must not exceed 100, got %d: a node that keeps its whole filesystem free writes no box disk", c.DiskReservePercent)
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

// PlatformConfig is the task's optional platform block: the bundle of boxd and
// executors the guest mounts read-only at /opt/box.
//
// It is optional and has the shape of the image block, so a job that does not
// name one behaves exactly as before: the box runs the boxd its image carries.
type PlatformConfig struct {
	// Version names the bundle for messages and task events. The sha256 is what
	// identifies it: the driver checks the download against it.
	Version string `codec:"version"`
	URL     string `codec:"url"`
	SHA256  string `codec:"sha256"`
}

// BakeConfig is the task's optional bake block: the task makes an image out of the
// disk of a sealed box instead of running a VM.
//
// The disk only exists on the node, and the driver is the host-side code this
// repository has, so baking is a task of the driver: StartTask with this block
// does no VM work at all.
type BakeConfig struct {
	// Name is the project's name; Version is the image's own ("<name>-<n>").
	Name    string `codec:"name"`
	Version string `codec:"version"`
	// Parent is the image the box came from.
	Parent string `codec:"parent"`
	// Arch is amd64 or arm64.
	Arch string `codec:"arch"`
	// Recipe is the script that makes this environment from the parent image,
	// base64, as the image's own /etc/box/image.json carries it.
	Recipe string `codec:"recipe"`
}

// PublishConfig is the task's optional publish block: the task puts an image that
// was baked on this node into the operator's store and into the store's index.
//
// The store itself (its url and its token) comes from the task's secrets, never
// from this block: a task config is readable by anyone who may read the job.
type PublishConfig struct {
	Version string `codec:"version"`
	Arch    string `codec:"arch"`
	SHA256  string `codec:"sha256"`
}

// TaskConfig is the driver configuration of one box task.
type TaskConfig struct {
	BoxID string      `codec:"box_id"`
	Image ImageConfig `codec:"image"`
	// Recipe is the base64 recipe the box's image carries, as the job holds it.
	// A box created from a project image passes it on, so a bake of that box
	// records the recipe it came from; the driver itself does not use it.
	Recipe          string   `codec:"recipe"`
	CPUs            int      `codec:"cpus"`
	MemoryMiB       int      `codec:"memory_mib"`
	DiskGiB         int      `codec:"disk_gib"`
	GuestLogCommand []string `codec:"guest_log_command"`
	// FilesDirInGuest is the root of the task's local/ directory in the guest:
	// its files land in <FilesDirInGuest>/local. Job files are not secrets, so
	// they live on the box disk.
	FilesDirInGuest string `codec:"files_dir_in_guest"`
	// SecretsDirInGuest is where the task's secrets/ directory lands: a tmpfs of
	// the guest, owned by root with mode 0700. A credential must never be on the
	// box disk, and boxd reads this directory (see internal/boxd).
	SecretsDirInGuest string `codec:"secrets_dir_in_guest"`
	// Platform is the platform bundle the guest mounts at /opt/box. Nil means
	// the image's own boxd runs.
	Platform *PlatformConfig `codec:"platform"`
	// Bake, when set, makes this task a bake: it reads the disk of a sealed box
	// and writes it into the image cache as a project image. A bake runs no VM,
	// so the image, cpus, memory and disk of a box do not apply to it.
	Bake *BakeConfig `codec:"bake"`
	// Publish, when set, makes this task a publish: it uploads an image that was
	// baked on this node into the operator's store and writes the store's index.
	Publish *PublishConfig `codec:"publish"`
}

// taskConfigSpec is the HCL schema of the task's driver block.
var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	// Optional, and required of a box task by applyDefaults: a publish job is not a
	// box and carries no id.
	"box_id": hclspec.NewAttr("box_id", "string", false),
	// Optional, because a bake and a publish run no VM: a box task is the one that
	// must carry it, and applyDefaults refuses one that does not (`image.url is
	// required`). A required block here refused the bake and publish jobs of every
	// build with "Missing image block" before the driver ever ran one.
	"image": hclspec.NewBlock("image", false, hclspec.NewObject(map[string]*hclspec.Spec{
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
	// Optional, like a block a newer CLI writes and an older driver ignores: a
	// task without it runs the boxd its image carries.
	"platform": hclspec.NewBlock("platform", false, hclspec.NewObject(map[string]*hclspec.Spec{
		"version": hclspec.NewAttr("version", "string", false),
		"url":     hclspec.NewAttr("url", "string", true),
		"sha256":  hclspec.NewAttr("sha256", "string", true),
	})),
	// A bake or a publish is one step of an image's life, not a box: the block is
	// what says so, and a task carries at most one of them.
	"bake": hclspec.NewBlock("bake", false, hclspec.NewObject(map[string]*hclspec.Spec{
		"name":    hclspec.NewAttr("name", "string", true),
		"version": hclspec.NewAttr("version", "string", true),
		"parent":  hclspec.NewAttr("parent", "string", false),
		"arch":    hclspec.NewAttr("arch", "string", true),
		"recipe":  hclspec.NewAttr("recipe", "string", false),
	})),
	"publish": hclspec.NewBlock("publish", false, hclspec.NewObject(map[string]*hclspec.Spec{
		"version": hclspec.NewAttr("version", "string", true),
		"arch":    hclspec.NewAttr("arch", "string", true),
		"sha256":  hclspec.NewAttr("sha256", "string", true),
	})),
	// The recipe the box's image carries, base64: the job is where a box keeps
	// it, so a box baked again, or rebuilt on a newer parent, replays the script
	// it came from. The driver passes it through and never reads it.
	"recipe": hclspec.NewAttr("recipe", "string", false),
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
	// A bake and a publish run no VM, so the image, cpus, memory and disk of a box
	// do not apply to them, and their own block is what has to be usable. Neither
	// has a box id either: a publish is not a box at all.
	if c.Bake != nil || c.Publish != nil {
		return c.validateImageWork()
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
	if p := c.Platform; p != nil {
		// Validated like the image block: the sha256 is what the download is
		// checked against, so a task without a usable one must not start.
		if p.SHA256 == "" {
			return fmt.Errorf("platform.sha256 is required")
		}
		if p.URL == "" {
			return fmt.Errorf("platform.url is required")
		}
		if _, err := box.NormalizeSHA256(p.SHA256); err != nil {
			return fmt.Errorf("platform.sha256: %w", err)
		}
	}
	return nil
}

// validateImageWork checks the block of a bake or a publish task.
func (c *TaskConfig) validateImageWork() error {
	if c.Bake != nil && c.Publish != nil {
		return fmt.Errorf("a task bakes or publishes an image, not both")
	}
	if b := c.Bake; b != nil {
		rec := box.BakedRecord{Version: b.Version, Arch: b.Arch, Name: b.Name}
		if err := box.ValidateBakedRecord(rec); err != nil {
			return fmt.Errorf("bake: %w", err)
		}
		if strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("bake.name is required")
		}
	}
	if p := c.Publish; p != nil {
		rec := box.BakedRecord{Version: p.Version, Arch: p.Arch}
		if err := box.ValidateBakedRecord(rec); err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		if _, err := box.NormalizeSHA256(p.SHA256); err != nil {
			return fmt.Errorf("publish.sha256: %w", err)
		}
	}
	return nil
}
