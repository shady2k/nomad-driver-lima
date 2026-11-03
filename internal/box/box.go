// Package box owns the lifecycle of one Lima instance ("box"): its disk on a
// Nomad dynamic host volume, the instance directory in LIMA_HOME, the image
// cache, offline resize and the memory cgroup the VM runs in.
//
// It deliberately knows nothing about Nomad; the driver translates task config
// into a Spec and turns the emitted events into Nomad task events.
package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/identity"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

const (
	// DefaultMemoryOverheadMiB is the host memory the qemu process and the lima
	// hostagent need on top of the guest RAM (spike S3.d: qemu ~ guest RAM +
	// 60 MB, hostagent ~ 60 MB).
	DefaultMemoryOverheadMiB = 150

	// gracefulStopTimeout bounds a shutdown the driver starts by itself (the
	// graceful limit Nomad asks for comes from StopTask).
	gracefulStopTimeout = 3 * time.Minute

	// diskFilePrefix is the volume file name of a box disk: box-<box id>.qcow2.
	diskFilePrefix = "box-"
	diskFileSuffix = ".qcow2"
)

var boxIDPattern = regexp.MustCompile(fmt.Sprintf(`^[a-z0-9-]{1,%d}$`, MaxBoxIDLen))

// ErrInvalidSpec marks a task configuration the driver must reject before it
// touches the host.
var ErrInvalidSpec = errors.New("invalid box spec")

// Reporter receives human readable lifecycle messages for one box.
type Reporter func(kind, message string)

// Options configure a Manager.
type Options struct {
	// Limactl is the path to the limactl binary (default "limactl").
	Limactl string
	// LimaHome is LIMA_HOME for this driver; a real directory per box lives here.
	LimaHome string
	// ImageCacheDir holds downloaded cloud images, keyed by sha256.
	ImageCacheDir string
	// MemoryOverheadMiB is added to the guest memory for the cgroup limit.
	MemoryOverheadMiB int
	// VMType is the Lima VM type (qemu or vz); empty means the OS default.
	VMType string
	// MemoryLimitOff starts boxes without the Linux memory scope. It is the
	// driver's memory_limit = "off" setting: an explicit operator decision for a
	// node that has no cgroup v2. Without it a start that cannot create the
	// scope fails, because a box must never quietly run unlimited.
	MemoryLimitOff bool
	// SystemdRun is the path to systemd-run (default "systemd-run").
	SystemdRun string
	// Identity is the unprivileged user every child process runs as and the
	// owner of everything this manager creates that Lima touches. Nil means the
	// current user, which is an error when the driver runs as root.
	Identity *identity.Identity
}

// Spec is everything the manager needs to create or adopt a box.
type Spec struct {
	BoxID       string
	VolumeDir   string // host path of the dynamic host volume holding the disk
	ImageURL    string
	ImageSHA256 string
	CPUs        int
	MemoryMiB   int
	DiskGiB     int
	VMType      string
}

// Validate checks the parts of the spec the driver cannot check from HCL.
func (s *Spec) Validate() error {
	if !boxIDPattern.MatchString(s.BoxID) {
		return fmt.Errorf("%w: box_id %q must match [a-z0-9-]{1,40}", ErrInvalidSpec, s.BoxID)
	}
	if s.VolumeDir == "" {
		return fmt.Errorf("%w: the task has no mount with request name \"disk\"", ErrInvalidSpec)
	}
	if s.ImageURL == "" || s.ImageSHA256 == "" {
		return fmt.Errorf("%w: image.url and image.sha256 are required", ErrInvalidSpec)
	}
	if s.CPUs <= 0 {
		return fmt.Errorf("%w: cpus must be positive", ErrInvalidSpec)
	}
	if s.MemoryMiB <= 0 {
		return fmt.Errorf("%w: memory_mib must be positive", ErrInvalidSpec)
	}
	if s.DiskGiB <= 0 {
		return fmt.Errorf("%w: disk_gib must be positive", ErrInvalidSpec)
	}
	return nil
}

// State is the observable result of Ensure.
type State struct {
	BoxID      string
	Instance   *limactl.Instance
	DiskPath   string
	Created    bool // a new disk was made
	Adopted    bool // an existing instance or disk was reused
	Resized    bool
	CgroupMode string // "systemd-scope" when the VM runs under a memory cap
	VMType     string
}

// Manager creates and adopts boxes. It is safe for concurrent use.
type Manager struct {
	opts Options
	lima *limactl.Client
}

// New returns a Manager. It does not touch the host.
func New(opts Options) (*Manager, error) {
	if opts.LimaHome == "" {
		return nil, fmt.Errorf("limactl home directory is required")
	}
	if opts.ImageCacheDir == "" {
		opts.ImageCacheDir = filepath.Join(opts.LimaHome, "_images")
	}
	if opts.MemoryOverheadMiB <= 0 {
		opts.MemoryOverheadMiB = DefaultMemoryOverheadMiB
	}
	if opts.VMType == "" {
		opts.VMType = DefaultVMType()
	}
	if opts.Limactl == "" {
		opts.Limactl = "limactl"
	}
	if opts.SystemdRun == "" {
		opts.SystemdRun = "systemd-run"
	}
	if opts.Identity == nil {
		id, err := identity.Resolve("")
		if err != nil {
			return nil, err
		}
		opts.Identity = id
	}
	lima := limactl.New(opts.Limactl, opts.LimaHome)
	lima.Identity = opts.Identity
	lima.Env = opts.Identity.Env(opts.LimaHome)
	return &Manager{opts: opts, lima: lima}, nil
}

// Identity returns the unprivileged user the boxes run as.
func (m *Manager) Identity() *identity.Identity { return m.opts.Identity }

// childEnv is the complete environment of every child process this manager
// starts (limactl, systemd-run, systemctl): an explicit HOME and LIMA_HOME, a
// PATH that finds lima and qemu, and the user's runtime directory.
func (m *Manager) childEnv() []string { return m.opts.Identity.Env(m.opts.LimaHome) }

// childCommand builds a child process that runs as the runtime user.
func (m *Manager) childCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = m.childEnv()
	m.opts.Identity.Configure(cmd)
	return cmd
}

// Options returns the manager options.
func (m *Manager) Options() Options { return m.opts }

// Lima returns the limactl client.
func (m *Manager) Lima() *limactl.Client { return m.lima }

// DiskFileName returns the file name a box disk has inside its volume.
func DiskFileName(boxID string) string { return diskFilePrefix + boxID + diskFileSuffix }

// VolumeDiskPath returns the path of the box disk inside its volume directory.
func VolumeDiskPath(volumeDir, boxID string) string {
	return filepath.Join(volumeDir, DiskFileName(boxID))
}

// BoxIDsFromVolume lists the box ids whose disks live in dir.
func BoxIDsFromVolume(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, diskFilePrefix) || !strings.HasSuffix(name, diskFileSuffix) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, diskFilePrefix), diskFileSuffix)
		if boxIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Box returns the handle for one box. rep may be nil.
func (m *Manager) Box(spec Spec, rep Reporter) (*Box, error) {
	if spec.VMType == "" {
		spec.VMType = m.opts.VMType
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := CheckSocketPath(m.opts.LimaHome, spec.BoxID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	spec.VolumeDir = filepath.Clean(spec.VolumeDir)
	return &Box{m: m, spec: spec, rep: rep}, nil
}

// Box is one Lima instance and the volume disk behind it.
type Box struct {
	m    *Manager
	spec Spec
	rep  Reporter
}

// Spec returns the box spec.
func (b *Box) Spec() Spec { return b.spec }

// DiskPath returns the path of the box disk inside its volume.
func (b *Box) DiskPath() string { return VolumeDiskPath(b.spec.VolumeDir, b.spec.BoxID) }

// InstanceDir returns <LIMA_HOME>/<box id>.
func (b *Box) InstanceDir() string { return b.m.lima.InstanceDir(b.spec.BoxID) }

func (b *Box) event(kind, message string) {
	if b.rep != nil {
		b.rep(kind, message)
	}
}

// Ensure creates or adopts the box and leaves it running.
func (b *Box) Ensure(ctx context.Context) (*State, error) {
	spec := b.spec
	volumeDir := spec.VolumeDir
	if st, err := os.Stat(volumeDir); err != nil {
		return nil, fmt.Errorf("volume directory %s: %w", volumeDir, err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("volume path %s is not a directory", volumeDir)
	}
	b.ownVolumeDir(volumeDir)

	volDisk := b.DiskPath()
	state := &State{BoxID: spec.BoxID, DiskPath: volDisk, VMType: spec.VMType}

	inst, err := b.m.lima.Get(ctx, spec.BoxID)
	if err != nil {
		return nil, fmt.Errorf("list lima instances: %w", err)
	}

	freshInstanceDisk := false
	if inst == nil {
		state.Created = !b.m.diskExists(volDisk)
		if state.Created {
			b.event("create", fmt.Sprintf("No disk for box %s in volume %s: creating a new VM", spec.BoxID, volumeDir))
		} else {
			state.Adopted = true
			b.event("adopt", fmt.Sprintf("Volume %s holds the disk of box %s but the Lima instance directory is gone: recreating the instance around the existing disk", volumeDir, spec.BoxID))
		}
		if err := b.createInstance(ctx); err != nil {
			return nil, err
		}
		inst, err = b.m.lima.Get(ctx, spec.BoxID)
		if err != nil {
			return nil, fmt.Errorf("list lima instances after create: %w", err)
		}
		if inst == nil {
			return nil, fmt.Errorf("limactl create reported success but instance %q is not listed", spec.BoxID)
		}
		freshInstanceDisk = true
	} else {
		state.Adopted = true
	}

	if _, err := b.attachVolumeDisk(freshInstanceDisk); err != nil {
		return nil, err
	}

	inst, err = b.m.lima.Get(ctx, spec.BoxID)
	if err != nil {
		return nil, fmt.Errorf("refresh instance: %w", err)
	}
	if inst == nil {
		return nil, fmt.Errorf("instance %q disappeared while preparing its disk", spec.BoxID)
	}

	if reason := b.m.brokenReason(ctx, inst); reason != "" {
		b.event("recover", fmt.Sprintf("Box %s is %q (%s): forcing a stop, then starting it again", spec.BoxID, inst.Status, reason))
		if err := b.m.lima.StopForce(ctx, spec.BoxID); err != nil {
			b.event("recover", fmt.Sprintf("Forced stop of box %s reported: %v", spec.BoxID, err))
		}
		inst, err = b.m.lima.Get(ctx, spec.BoxID)
		if err != nil {
			return nil, fmt.Errorf("refresh instance after forced stop: %w", err)
		}
		if inst != nil && inst.IsRunning() {
			return nil, fmt.Errorf("box %s is still running after a forced stop", spec.BoxID)
		}
	}

	if inst != nil && b.m.Alive(inst) {
		diff := b.m.configDiff(inst, spec)
		if diff == "" {
			b.event("adopt", fmt.Sprintf("Box %s is already running with the requested configuration: attaching to the existing VM (disk %s)", spec.BoxID, volDisk))
			state.Instance = inst
			state.CgroupMode = b.m.cgroupModeFor(inst, spec.BoxID)
			return state, nil
		}
		// A jobspec change replaced the allocation while the old VM was still
		// up (Nomad starts the replacement before it stops the old allocation).
		// Resize means stop, edit, start: the box has real downtime here.
		b.event("resize", fmt.Sprintf("Box %s runs an older configuration (%s): stopping it to apply the new one", spec.BoxID, diff))
		if err := b.Stop(ctx, gracefulStopTimeout); err != nil {
			return nil, fmt.Errorf("stop box %s for a resize: %w", spec.BoxID, err)
		}
		inst, err = b.m.lima.Get(ctx, spec.BoxID)
		if err != nil {
			return nil, fmt.Errorf("refresh instance after stop: %w", err)
		}
	}

	resized, err := b.resize(ctx, inst)
	if err != nil {
		return nil, err
	}
	state.Resized = resized

	mode, err := b.m.startInstance(ctx, b)
	if err != nil {
		return nil, err
	}
	state.CgroupMode = mode

	inst, err = b.m.lima.Get(ctx, spec.BoxID)
	if err != nil {
		return nil, fmt.Errorf("refresh instance after start: %w", err)
	}
	if inst == nil || !inst.IsRunning() {
		status := "not listed"
		if inst != nil {
			status = inst.StatusMessage()
		}
		return nil, fmt.Errorf("limactl start finished but box %s is %s", spec.BoxID, status)
	}
	state.Instance = inst
	b.event("start", fmt.Sprintf("Box %s is running (pid %d, %d vCPU, %d MiB RAM, disk %s, cgroup %s)",
		spec.BoxID, inst.DriverPID, spec.CPUs, spec.MemoryMiB, volDisk, mode))
	return state, nil
}

// ownVolumeDir gives the volume directory to the runtime user when it belongs to
// someone else: limactl, which runs as that user, writes the box disk into it and
// qemu opens the disk through the instance symlink. A volume the box-volume
// plugin already chowned is left untouched.
func (b *Box) ownVolumeDir(dir string) {
	id := b.m.opts.Identity
	if !id.Drop {
		return
	}
	owned, err := id.Owns(dir)
	if err != nil || owned {
		return
	}
	if err := id.Chown(dir); err != nil {
		b.event("disk", fmt.Sprintf("Volume directory %s does not belong to %s: %v", dir, id.Name, err))
		return
	}
	b.event("disk", fmt.Sprintf("Volume directory %s belonged to another user: gave it to %s", dir, id.Name))
}

// createInstance writes the Lima configuration and creates the instance. On
// failure a half-created instance directory is removed again.
func (b *Box) createInstance(ctx context.Context) error {
	spec := b.spec
	imagePath, err := b.EnsureImage(ctx, spec.ImageURL, spec.ImageSHA256, func(percent int) {
		b.event("image", fmt.Sprintf("Image download %d%%", percent))
	})
	if err != nil {
		return err
	}
	content, err := renderLimaYAML(spec, imagePath)
	if err != nil {
		return err
	}
	yamlPath, err := b.m.writeLimaYAML(spec.BoxID, content)
	if err != nil {
		return err
	}
	defer os.Remove(yamlPath)

	instDir := b.InstanceDir()
	if _, statErr := os.Stat(instDir); statErr == nil {
		return fmt.Errorf("lima instance directory %s already exists; refusing to touch it", instDir)
	}
	b.event("create", fmt.Sprintf("Creating Lima instance %s (%d vCPU, %d MiB, %d GiB disk, %s)", spec.BoxID, spec.CPUs, spec.MemoryMiB, spec.DiskGiB, spec.VMType))
	if err := b.m.lima.Create(ctx, spec.BoxID, yamlPath); err != nil {
		if _, statErr := os.Stat(instDir); statErr == nil {
			b.event("create", fmt.Sprintf("Removing partially created instance directory %s after a failed create", instDir))
			_ = os.RemoveAll(instDir)
		}
		return fmt.Errorf("create lima instance %s: %w", spec.BoxID, err)
	}
	return nil
}

// attachVolumeDisk makes <LIMA_HOME>/<box>/disk a symlink to the disk inside the
// volume. freshInstanceDisk is true right after `limactl create` wrote a fresh,
// never booted disk: it is moved into an empty volume, or thrown away when the
// volume already holds this box's disk. An existing disk is never overwritten.
func (b *Box) attachVolumeDisk(freshInstanceDisk bool) (string, error) {
	spec := b.spec
	instDisk := b.m.lima.DiskPath(spec.BoxID)
	volDisk := b.DiskPath()

	fi, err := os.Lstat(instDisk)
	switch {
	case os.IsNotExist(err):
		if !b.m.diskExists(volDisk) {
			return "", fmt.Errorf("box %s has neither an instance disk (%s) nor a volume disk (%s)", spec.BoxID, instDisk, volDisk)
		}
		if err := b.symlinkDisk(volDisk); err != nil {
			return "", err
		}
		return volDisk, nil
	case err != nil:
		return "", fmt.Errorf("inspect instance disk %s: %w", instDisk, err)
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(instDisk)
		if err != nil {
			return "", fmt.Errorf("read disk symlink %s: %w", instDisk, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(instDisk), target)
		}
		if target == volDisk {
			if !b.m.diskExists(volDisk) {
				return "", fmt.Errorf("box %s disk symlink %s points at %s, which does not exist", spec.BoxID, instDisk, volDisk)
			}
			return volDisk, nil
		}
		if b.m.diskExists(target) {
			return "", fmt.Errorf("box %s disk %s points at %s instead of the volume disk %s; refusing to touch either file", spec.BoxID, instDisk, target, volDisk)
		}
		if !b.m.diskExists(volDisk) {
			return "", fmt.Errorf("box %s disk symlink %s points at missing file %s and the volume has no disk %s", spec.BoxID, instDisk, target, volDisk)
		}
		b.event("disk", fmt.Sprintf("Repointing box %s disk symlink from %s to %s", spec.BoxID, target, volDisk))
		if err := os.Remove(instDisk); err != nil {
			return "", fmt.Errorf("remove stale disk symlink %s: %w", instDisk, err)
		}
		if err := b.symlinkDisk(volDisk); err != nil {
			return "", err
		}
		return volDisk, nil
	}

	// A regular file: either the freshly created disk (move it into the volume)
	// or a disk that predates the volume layout (also move it, never lose it).
	if b.m.diskExists(volDisk) {
		if freshInstanceDisk {
			b.event("disk", fmt.Sprintf("Volume already holds disk %s: discarding the fresh, never booted instance disk %s", volDisk, instDisk))
			if err := os.Remove(instDisk); err != nil {
				return "", fmt.Errorf("remove fresh instance disk %s: %w", instDisk, err)
			}
			if err := b.symlinkDisk(volDisk); err != nil {
				return "", err
			}
			return volDisk, nil
		}
		return "", fmt.Errorf("box %s has two disks: %s and %s; refusing to choose one", spec.BoxID, instDisk, volDisk)
	}
	if err := b.m.moveFile(instDisk, volDisk); err != nil {
		return "", fmt.Errorf("move box disk into volume: %w", err)
	}
	if err := b.symlinkDisk(volDisk); err != nil {
		return "", err
	}
	b.event("disk", fmt.Sprintf("Box %s disk placed at %s and symlinked from the instance directory", spec.BoxID, volDisk))
	return volDisk, nil
}

func (b *Box) symlinkDisk(volDisk string) error {
	instDisk := b.m.lima.DiskPath(b.spec.BoxID)
	if err := b.m.opts.Identity.EnsureDir(filepath.Dir(instDisk), 0o755); err != nil {
		return fmt.Errorf("create instance directory %s: %w", filepath.Dir(instDisk), err)
	}
	if err := os.Symlink(volDisk, instDisk); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", instDisk, volDisk, err)
	}
	// The symlink sits in the Lima home, which belongs to the runtime user;
	// limactl (as that user) has to be able to replace it.
	if err := b.m.opts.Identity.Chown(instDisk); err != nil {
		return err
	}
	return nil
}

// resize applies an offline cpu/memory/disk change. Lima cannot edit a running
// instance, so this must only be called while the box is stopped.
func (b *Box) resize(ctx context.Context, inst *limactl.Instance) (bool, error) {
	if inst == nil {
		return false, nil
	}
	if inst.IsRunning() {
		return false, fmt.Errorf("refusing to resize running box %s", b.spec.BoxID)
	}
	diff := b.m.configDiff(inst, b.spec)
	if diff == "" {
		return false, nil
	}
	opts := limactl.EditOptions{}
	if inst.CPUs != b.spec.CPUs {
		opts.CPUs = b.spec.CPUs
	}
	if want := int64(b.spec.MemoryMiB) * 1024 * 1024; inst.Memory != want {
		opts.MemoryGiB = float64(b.spec.MemoryMiB) / 1024
	}
	wantDisk := int64(b.spec.DiskGiB) * 1024 * 1024 * 1024
	if inst.Disk != 0 && wantDisk < inst.Disk {
		b.event("resize", fmt.Sprintf("Box %s disk is %d GiB and disk_gib asks for %d GiB: disks can only grow, keeping the larger disk", b.spec.BoxID, inst.Disk/(1024*1024*1024), b.spec.DiskGiB))
	} else if inst.Disk != wantDisk {
		opts.DiskGiB = float64(b.spec.DiskGiB)
	}
	if opts.CPUs == 0 && opts.MemoryGiB == 0 && opts.DiskGiB == 0 {
		return false, nil
	}
	b.event("resize", fmt.Sprintf("Resizing box %s offline (the VM is not running): %s", b.spec.BoxID, diff))
	if err := b.m.lima.Edit(ctx, b.spec.BoxID, opts); err != nil {
		return false, fmt.Errorf("resize box %s: %w", b.spec.BoxID, err)
	}
	return true, nil
}

// Stop shuts the box down, escalating to a hard kill after timeout.
func (b *Box) Stop(ctx context.Context, timeout time.Duration) error {
	boxID := b.spec.BoxID
	inst, err := b.m.lima.Get(ctx, boxID)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	if inst == nil {
		return nil
	}
	if !inst.IsRunning() {
		b.event("stop", fmt.Sprintf("Box %s is already %s", boxID, inst.StatusMessage()))
		return nil
	}
	b.event("stop", fmt.Sprintf("Stopping box %s (graceful timeout %s)", boxID, timeout))
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	err = b.m.lima.Stop(stopCtx, boxID)
	cancel()
	if err != nil {
		b.event("stop", fmt.Sprintf("Graceful stop of box %s failed (%v): forcing the VM to stop", boxID, err))
		if ferr := b.m.lima.StopForce(ctx, boxID); ferr != nil {
			return fmt.Errorf("stop box %s: %w (forced stop also failed: %v)", boxID, err, ferr)
		}
	}
	return b.waitStopped(ctx, 30*time.Second)
}

// waitStopped polls until lima reports the instance as not running.
func (b *Box) waitStopped(ctx context.Context, timeout time.Duration) error {
	boxID := b.spec.BoxID
	deadline := time.Now().Add(timeout)
	for {
		inst, err := b.m.lima.Get(ctx, boxID)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		if inst == nil || !inst.IsRunning() {
			status := "gone"
			if inst != nil {
				status = inst.StatusMessage()
			}
			b.event("stop", fmt.Sprintf("Box %s stopped (%s)", boxID, status))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("box %s is still running after %s", boxID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Discard stops the box and deletes its instance, keeping the disk. It is for a
// start that cannot finish: a task whose files could not be installed must not
// leave a VM behind, because the next start would adopt it with a disk symlink
// of a volume that `box rm` may have deleted in between.
func (b *Box) Discard(ctx context.Context) error {
	boxID := b.spec.BoxID
	exists, err := b.m.lima.Exists(ctx, boxID)
	if err != nil {
		return fmt.Errorf("look for box %s: %w", boxID, err)
	}
	if !exists {
		return nil
	}
	if err := b.m.lima.StopForce(ctx, boxID); err != nil {
		b.event("discard", fmt.Sprintf("Could not stop box %s: %v", boxID, err))
	}
	if err := b.m.lima.Delete(ctx, boxID); err != nil {
		return fmt.Errorf("delete box %s: %w", boxID, err)
	}
	b.event("discard", fmt.Sprintf("Removed box %s: its start failed and the disk stays in its volume", boxID))
	return nil
}

// Liveness is the process-level state of a box, read from the pid files lima
// writes into the instance directory instead of from `limactl list`.
//
// This matters for the wait loop: `limactl list` asks the hostagent over its API
// with multi-second timeouts, and when the hostagent is gone that call is slow
// or hangs, which is exactly the moment the driver has to notice that the VM
// died.
type Liveness struct {
	HostAgentPID   int
	DriverPID      int
	HostAgentAlive bool
	DriverAlive    bool
	// Known is true when at least one pid file was readable.
	Known bool
}

// Liveness reads the pid files of the box.
func (b *Box) Liveness() Liveness {
	var l Liveness
	dir := b.InstanceDir()
	if pid, err := readPIDFile(filepath.Join(dir, DriverPIDFile(b.spec.VMType))); err == nil {
		l.DriverPID = pid
		l.DriverAlive = processAlive(pid)
		l.Known = true
	}
	if pid, err := readPIDFile(filepath.Join(dir, "ha.pid")); err == nil {
		l.HostAgentPID = pid
		l.HostAgentAlive = processAlive(pid)
		l.Known = true
	}
	return l
}

// DriverPIDFile is the pid file Lima writes for the VM of the given type:
// qemu.pid for the qemu process, vz.pid for the hostagent that runs the VZ VM
// in-process.
func DriverPIDFile(vmType string) string {
	if vmType == "" {
		vmType = DefaultVMType()
	}
	return vmType + ".pid"
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pid file %s: %w", path, err)
	}
	return pid, nil
}

// Status returns the current instance state (nil when the instance is gone).
func (b *Box) Status(ctx context.Context) (*limactl.Instance, error) {
	return b.m.lima.Get(ctx, b.spec.BoxID)
}

// Guest roots of a task directory.
//
// The task directory has two roots: local/ holds files a job wants in the box and
// secrets/ holds credentials. They end up in different places because a
// credential must never be on the box disk: local/ files go under
// filesDirInGuest on the disk, secrets/ files go to secretsDirInGuest, which is
// a tmpfs (see DefaultSecretsDirInGuest).
const (
	boxLocalDir   = "local"
	boxSecretsDir = "secrets"
)

// DefaultSecretsDirInGuest is where the driver puts the task's secrets files: a
// tmpfs of the guest, owned by root with mode 0700. boxd reads them there and
// writes everything it derives from them into /run/boxd, also a tmpfs, so no
// credential and no executor configuration reaches the box disk.
const DefaultSecretsDirInGuest = "/run/box/secrets"

// tmpfsStagingRoot is the staging directory of a file copy: inside /run, so a
// secret is never written to the disk even while it is being copied.
const tmpfsStagingRoot = "/run/box"

// CopyTaskFiles copies the files Nomad rendered into the task's local and
// secrets directories into the guest: local files below localDirInGuest, secrets
// below secretsDirInGuest. It returns the number of files.
//
// The secrets directory exists afterwards even when the box got no credentials,
// because boxd reads it and its mode (0700, root) is what keeps the box user out
// of it.
func (b *Box) CopyTaskFiles(ctx context.Context, localDir, secretsDir, localDirInGuest, secretsDirInGuest string) (int, error) {
	if localDirInGuest == "" {
		localDirInGuest = "/etc/box"
	}
	if secretsDirInGuest == "" {
		secretsDirInGuest = DefaultSecretsDirInGuest
	}
	localFiles, err := collectTaskFiles(localDir, boxLocalDir)
	if err != nil {
		return 0, fmt.Errorf("read task local directory %s: %w", localDir, err)
	}
	secretFiles, err := collectTaskFiles(secretsDir, boxSecretsDir)
	if err != nil {
		return 0, fmt.Errorf("read task secrets directory %s: %w", secretsDir, err)
	}

	count := 0
	if len(localFiles) > 0 {
		n, err := b.installTaskFiles(ctx, localFiles, filepath.Join(localDirInGuest, boxLocalDir), "0755")
		if err != nil {
			return count, err
		}
		count += n
	}
	n, err := b.installTaskFiles(ctx, secretFiles, secretsDirInGuest, "0700")
	if err != nil {
		return count, err
	}
	count += n
	if count > 0 {
		b.event("files", fmt.Sprintf("Copied %d task file(s) into the guest (%s, and the executor credentials in %s)",
			count, filepath.Join(localDirInGuest, boxLocalDir), secretsDirInGuest))
	}
	return count, nil
}

// collectTaskFiles lists the regular files below root, keyed by the path below
// it with prefix in front (for the log messages).
func collectTaskFiles(root, prefix string) ([]taskFile, error) {
	if root == "" {
		return nil, nil
	}
	var files []taskFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, taskFile{host: path, rel: rel})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	for i := range files {
		files[i].log = filepath.Join(prefix, files[i].rel)
	}
	return files, nil
}

// installTaskFiles puts the files into destDirInGuest. dirMode is the mode of the
// destination tree (0755 for job files, 0700 for the secrets).
//
// The task directory belongs to the Nomad client, which is root on a Linux node,
// and limactl copy is an rsync over ssh that runs as the runtime user: it cannot
// read the task directory, so the files are staged in a directory the runtime
// user owns first. That staging directory is in /run, like the destination of the
// secrets, so a secret never touches the disk on the way in.
func (b *Box) installTaskFiles(ctx context.Context, files []taskFile, destDirInGuest, dirMode string) (int, error) {
	if !filepath.IsAbs(destDirInGuest) {
		return 0, fmt.Errorf("the guest destination %q is not an absolute path", destDirInGuest)
	}
	if len(files) == 0 {
		// The directory itself still has to exist with the right mode and owner.
		script := fmt.Sprintf("set -eu; sudo -n install -d -m %s -o root -g root '%s'", dirMode, destDirInGuest)
		if _, stderr, err := b.m.lima.Shell(ctx, b.spec.BoxID, []string{"bash", "-euo", "pipefail", "-c", script}); err != nil {
			return 0, fmt.Errorf("create the guest directory %s: %w%s", destDirInGuest, err, suffix(stderr))
		}
		return 0, nil
	}

	stage, err := b.stageTaskFiles(files)
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(stage)

	// The staging directory in the guest belongs to the guest user too: a
	// root-owned directory refuses the write ("mkstemp ...: Permission denied").
	// It is created by root (the box user cannot write to /run/box) and given to
	// the box user, so limactl copy can write into it.
	create := fmt.Sprintf("set -eu; sudo -n install -d -m 0755 -o root -g root '%s'; "+
		"d=$(sudo -n mktemp -d '%s/stage-%s-XXXXXXXX'); sudo -n chown $(id -u):$(id -g) \"$d\"; printf '%%s\\n' \"$d\"",
		tmpfsStagingRoot, tmpfsStagingRoot, b.spec.BoxID)
	out, stderr, err := b.m.lima.Shell(ctx, b.spec.BoxID, []string{"bash", "-euo", "pipefail", "-c", create})
	staging := ""
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) > 0 {
		staging = strings.TrimSpace(lines[len(lines)-1])
	}
	if err != nil || staging == "" {
		return 0, fmt.Errorf("create a staging directory in the guest: %w%s (output %q)", err, suffix(stderr), strings.TrimSpace(out))
	}

	for _, f := range files {
		parent := filepath.Dir(filepath.Join(staging, f.rel))
		if _, stderr, err := b.m.lima.Shell(ctx, b.spec.BoxID, []string{"mkdir", "-p", parent}); err != nil {
			return 0, fmt.Errorf("create guest directory %s: %w%s", parent, err, suffix(stderr))
		}
		staged := filepath.Join(stage, f.rel)
		if err := b.m.lima.Copy(ctx, []string{staged}, b.spec.BoxID+":"+filepath.Join(staging, f.rel)); err != nil {
			return 0, fmt.Errorf("copy %s into the guest: %w", f.log, err)
		}
	}
	install := fmt.Sprintf("set -eu; sudo -n install -d -m %s -o root -g root '%s'; "+
		"sudo -n cp -a '%s/.' '%s/'; sudo -n chown -R root:root '%s'; "+
		"sudo -n chmod -R u+rwX,go-rwx '%s'; sudo -n rm -rf '%s'",
		dirMode, destDirInGuest, staging, destDirInGuest, destDirInGuest, destDirInGuest, staging)
	if _, stderr, err := b.m.lima.Shell(ctx, b.spec.BoxID, []string{"bash", "-euo", "pipefail", "-c", install}); err != nil {
		return 0, fmt.Errorf("install the task files into %s: %w%s", destDirInGuest, err, suffix(stderr))
	}
	return len(files), nil
}

// stageTaskFiles copies the task files into a host directory the runtime user
// owns and returns it. The caller removes it; the files keep their relative path
// below the task's local/ and secrets/ roots.
func (b *Box) stageTaskFiles(files []taskFile) (string, error) {
	id := b.m.opts.Identity
	// The staging root sits inside the Lima home but under a name Lima ignores:
	// every other directory there is an instance, and Lima would report a
	// directory without a lima.yaml as a broken instance. Lima skips the names
	// that start with an underscore, like its own _config and the driver's
	// default _image-cache.
	root := filepath.Join(b.m.opts.LimaHome, "_staging")
	if err := id.EnsureDir(root, 0o700); err != nil {
		return "", fmt.Errorf("create the file staging directory: %w", err)
	}
	stage, err := os.MkdirTemp(root, "files-"+b.spec.BoxID+"-")
	if err != nil {
		return "", fmt.Errorf("create the file staging directory: %w", err)
	}
	for _, f := range files {
		dst := filepath.Join(stage, f.rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			os.RemoveAll(stage)
			return "", fmt.Errorf("create staging directory for %s: %w", f.rel, err)
		}
		if err := copyFile(f.host, dst); err != nil {
			os.RemoveAll(stage)
			return "", fmt.Errorf("stage task file %s: %w", f.rel, err)
		}
	}
	if err := id.ChownTree(stage); err != nil {
		os.RemoveAll(stage)
		return "", fmt.Errorf("give the staged task files to %s: %w", id.Name, err)
	}
	return stage, nil
}

// taskFile is one file of a task directory as it appears in the guest.
type taskFile struct {
	host string
	// rel is the path below the task's local/ or secrets/ root.
	rel string
	// log names the file for messages ("secrets/prime/auth.json").
	log string
}

// copyFile copies one regular file, keeping it private to its owner.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func suffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}

// OrphanReason reports why an instance looks abandoned: its disk symlink target
// is gone, or its disk is not on a volume at all. Empty string means healthy.
func (m *Manager) OrphanReason(inst *limactl.Instance) string {
	disk := filepath.Join(inst.InstanceDir(m.opts.LimaHome), "disk")
	fi, err := os.Lstat(disk)
	if err != nil {
		if os.IsNotExist(err) {
			return "no disk file"
		}
		return "disk unreadable"
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "disk is not on a volume"
	}
	target, err := os.Readlink(disk)
	if err != nil {
		return "disk symlink unreadable"
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(disk), target)
	}
	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			return "disk volume file missing"
		}
		return "disk volume file unreadable"
	}
	return ""
}

// List returns every lima instance visible in this home directory.
func (m *Manager) List(ctx context.Context) ([]*limactl.Instance, error) {
	return m.lima.List(ctx)
}

func (m *Manager) diskExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Alive reports whether the VM and its hostagent process are both up.
//
// lima reports an instance as Broken when the hostagent does not answer its API
// within three seconds, which happens on a busy host while the guest is booting.
// A dead hostagent process is the reliable signal, so the process is what counts
// here; the status only has to say "not stopped".
func (m *Manager) Alive(inst *limactl.Instance) bool {
	if inst == nil {
		return false
	}
	if inst.IsBroken() {
		// A broken instance is only usable while its hostagent process is alive.
		return inst.HostAgentPID > 0 && processAlive(inst.HostAgentPID)
	}
	if !inst.IsRunning() {
		return false
	}
	if inst.HostAgentPID > 0 && !processAlive(inst.HostAgentPID) {
		return false
	}
	return true
}

// brokenReason returns a non-empty reason when the instance is in a state that
// needs a forced stop before it can be started again.
func (m *Manager) brokenReason(ctx context.Context, inst *limactl.Instance) string {
	if inst.IsBroken() && !m.Alive(inst) {
		return "lima lost control of the VM and its hostagent is gone"
	}
	if inst.IsRunning() && inst.HostAgentPID > 0 && !processAlive(inst.HostAgentPID) {
		return "hostagent is gone"
	}
	return ""
}

// ReattachReason reports why a recovered instance cannot be re-attached to a
// running task ("" means it can: the VM is up and its hostagent is alive).
func (m *Manager) ReattachReason(inst *limactl.Instance) string {
	if inst == nil {
		return "the lima instance does not exist"
	}
	if !m.Alive(inst) {
		if reason := m.brokenReason(context.Background(), inst); reason != "" {
			return fmt.Sprintf("the lima instance is %s: %s", inst.Status, reason)
		}
		return "the lima instance is " + inst.StatusMessage()
	}
	return ""
}

// configDiff describes how the instance differs from the requested
// configuration ("" when it matches).
func (m *Manager) configDiff(inst *limactl.Instance, spec Spec) string {
	var parts []string
	if inst.CPUs != 0 && inst.CPUs != spec.CPUs {
		parts = append(parts, fmt.Sprintf("cpus %d -> %d", inst.CPUs, spec.CPUs))
	}
	if want := int64(spec.MemoryMiB) * 1024 * 1024; inst.Memory != 0 && inst.Memory != want {
		parts = append(parts, fmt.Sprintf("memory %d MiB -> %d MiB", inst.Memory/(1024*1024), spec.MemoryMiB))
	}
	if want := int64(spec.DiskGiB) * 1024 * 1024 * 1024; inst.Disk != 0 && inst.Disk != want {
		parts = append(parts, fmt.Sprintf("disk %d GiB -> %d GiB", inst.Disk/(1024*1024*1024), spec.DiskGiB))
	}
	return strings.Join(parts, ", ")
}

// moveFile renames src to dst, falling back to a copy when they are on
// different filesystems (the volume may be a separate mount). The disk ends up
// belonging to the runtime user either way: a rename keeps the owner of the file
// limactl wrote, a copy has to be given away.
func (m *Manager) moveFile(src, dst string) error {
	if err := m.opts.Identity.EnsureDir(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return m.opts.Identity.Chown(dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := m.opts.Identity.Chown(dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// startDirect boots the instance in the caller's context, with no memory cap.
func (m *Manager) startDirect(ctx context.Context, b *Box) (string, error) {
	b.event("start", fmt.Sprintf("Starting box %s (%d vCPU, %d MiB RAM)", b.spec.BoxID, b.spec.CPUs, b.spec.MemoryMiB))
	if _, stderr, err := m.lima.RunLong(ctx, "start", "--tty=false", b.spec.BoxID); err != nil {
		return "none", fmt.Errorf("start box %s: %w%s", b.spec.BoxID, err, suffix(stderr))
	}
	return "none", nil
}
