// Package driver implements the Nomad task driver "box": one Lima VM per task,
// with the VM disk on a Nomad dynamic host volume so that the scheduler pins the
// box to the node that holds the disk.
package driver

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
	"github.com/shady2k/nomad-driver-lima/internal/version"
)

const (
	// pluginName is the registered name of the task driver.
	pluginName = "box"

	// fingerprintPeriod is how often the driver reports node attributes.
	fingerprintPeriod = 30 * time.Second

	// orphanGrace is how long a Lima instance may exist without a task before
	// the driver reports it as an orphan.
	orphanGrace = 10 * time.Minute

	// taskHandleVersion is the version of the persisted task handle.
	taskHandleVersion = 1

	// watchInterval is how often the driver checks whether the VM stopped.
	watchInterval = 2 * time.Second

	// limaVersionTTL caches `limactl --version` for the fingerprint.
	limaVersionTTL = 5 * time.Minute
)

var (
	// pluginInfo describes the plugin to the Nomad client.
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     version.Version,
		Name:              pluginName,
	}

	// driverCapabilities tells Nomad what this driver can do. MountConfigs must
	// be supported: the box disk arrives as a mount configuration.
	driverCapabilities = &drivers.Capabilities{
		SendSignals: false,
		Exec:        true,
		FSIsolation: fsisolation.None,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeHost,
		},
		MountConfigs: drivers.MountConfigSupportAll,
	}
)

// Driver is the "box" task driver.
type Driver struct {
	*eventer.Eventer

	config      Config
	nomadConfig *base.ClientDriverConfig
	compute     cpustats.Compute

	tasks  *taskStore
	ctx    context.Context
	cancel context.CancelFunc
	logger hclog.Logger
	boxm   *box.Manager

	mu            sync.Mutex
	limaVersion   string
	limaVersionAt time.Time
	unattached    map[string]time.Time
}

// NewDriver returns a driver bound to ctx (the plugin's lifetime).
func NewDriver(ctx context.Context, logger hclog.Logger) *Driver {
	d := &Driver{
		Eventer:    eventer.NewEventer(ctx, logger),
		tasks:      newTaskStore(),
		logger:     logger,
		unattached: map[string]time.Time{},
	}
	d.ctx, d.cancel = context.WithCancel(ctx)
	return d
}

// PluginInfo implements base.BasePlugin.
func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) { return pluginInfo, nil }

// ConfigSchema implements base.BasePlugin.
func (d *Driver) ConfigSchema() (*hclspec.Spec, error) { return configSpec, nil }

// SetConfig implements base.BasePlugin.
func (d *Driver) SetConfig(cfg *base.Config) error {
	var c Config
	if cfg != nil && len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &c); err != nil {
			return fmt.Errorf("decode plugin config: %w", err)
		}
	}
	if err := c.validate(); err != nil {
		return err
	}
	d.config = c
	if cfg != nil && cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
		d.compute = cfg.AgentConfig.Compute()
	}
	if c.identityErr != nil {
		// A root client without a usable runtime_user must not stop the plugin
		// from loading: the fingerprint reports it in plain words and every start
		// fails with the same message, so the node stays visible in the cluster
		// with the one thing that has to be fixed.
		d.logger.Error("the box driver cannot run Lima", "error", c.identityErr)
		return nil
	}
	// The Lima home and the image cache hold everything Lima touches; the
	// runtime user owns both, so limactl (as that user) can write them.
	if err := c.identity.EnsureDir(c.LimaHome, 0o700); err != nil {
		return fmt.Errorf("prepare lima_home: %w", err)
	}
	if err := c.identity.EnsureDir(c.ImageCacheDir, 0o755); err != nil {
		return fmt.Errorf("prepare image_cache_dir: %w", err)
	}
	m, err := box.New(box.Options{
		Limactl:           c.LimactlPath,
		LimaHome:          c.LimaHome,
		ImageCacheDir:     c.ImageCacheDir,
		MemoryOverheadMiB: c.MemoryOverheadMiB,
		VMType:            c.VMType,
		MemoryLimitOff:    c.MemoryLimit == box.MemoryLimitOff,
		Identity:          c.identity,
	})
	if err != nil {
		return err
	}
	d.boxm = m
	d.logger.Info("box driver configured",
		"limactl", c.LimactlPath, "lima_home", c.LimaHome, "image_cache_dir", c.ImageCacheDir,
		"memory_overhead_mib", c.MemoryOverheadMiB, "memory_limit", c.MemoryLimit, "vm_type", c.VMType,
		"runtime_user", c.identity.String(), "drops_privileges", c.identity.Drop)
	return nil
}

// TaskConfigSchema implements drivers.DriverPlugin.
func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) { return taskConfigSpec, nil }

// Capabilities implements drivers.DriverPlugin.
func (d *Driver) Capabilities() (*drivers.Capabilities, error) { return driverCapabilities, nil }

// Fingerprint implements drivers.DriverPlugin.
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(fingerprintPeriod)
		defer ticker.Stop()
		for {
			fp := d.buildFingerprint(ctx)
			select {
			case ch <- fp:
			case <-ctx.Done():
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ch, nil
}

func (d *Driver) buildFingerprint(ctx context.Context) *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        map[string]*structs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}
	set := func(key, value string) {
		fp.Attributes[key] = structs.NewStringAttribute(value)
	}
	set("driver.box.version", version.Version)
	set("driver.box.arch", box.LimaArch())
	set("driver.box.os", runtime.GOOS)
	set("driver.box.vm_type", d.config.VMType)
	set("driver.box.guests", guestKinds())
	// Whether the memory budget of a box is enforced on this node: a node with
	// memory_limit = "off" runs boxes without a cap, and that is visible here.
	set("driver.box.memory_limit", d.config.MemoryLimit)
	if d.config.identity != nil {
		set("driver.box.runtime_user", d.config.identity.Name)
	}

	if d.config.identityErr != nil {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = d.config.identityErr.Error()
		set("driver.box.lima_version", "unknown")
		set("driver.box.instances", "0")
		set("driver.box.orphans", "")
		setDiskUsage(fp, nil)
		return fp
	}

	if d.boxm == nil {
		fp.Health = drivers.HealthStateUndetected
		fp.HealthDescription = "box driver is not configured"
		return fp
	}

	limaVersion, err := d.limaVersionCached(ctx)
	if err != nil {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = fmt.Sprintf("limactl is not usable: %v", err)
		set("driver.box.lima_version", "unknown")
		set("driver.box.instances", "0")
		set("driver.box.orphans", "")
		setDiskUsage(fp, nil)
		return fp
	}
	set("driver.box.lima_version", limaVersion)

	instances, err := d.boxm.List(ctx)
	if err != nil {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = fmt.Sprintf("cannot list lima instances: %v", err)
		set("driver.box.instances", "0")
		set("driver.box.orphans", "")
		setDiskUsage(fp, nil)
		return fp
	}
	set("driver.box.instances", strconv.Itoa(len(instances)))
	set("driver.box.orphans", joinOrphanList(d.orphans(instances)))
	// What each box disk really occupies on this host, per box and in total.
	setDiskUsage(fp, instances)
	// Every live VM with its size and whether a task owns it: `box capacity`
	// counts the ones Nomad does not.
	setInstanceAttributes(fp, instanceInfos(instances, d.tasks.BoxIDs()))
	return fp
}

// setDiskUsage publishes the disk usage of every box on the node.
//
// The numbers come from one stat per disk, so this is cheap enough to run on
// every fingerprint. Stopped boxes and orphans are included: their disks hold
// the work of the box and the space of the host just the same.
func setDiskUsage(fp *drivers.Fingerprint, instances []*limactl.Instance) {
	var totalUsed, totalVirtual int64
	for _, disk := range boxDiskUsages(instances) {
		totalUsed += disk.UsedBytes
		totalVirtual += disk.VirtualBytes
		prefix := diskAttributePrefix + disk.BoxID + "."
		fp.Attributes[prefix+"used_bytes"] = structs.NewStringAttribute(strconv.FormatInt(disk.UsedBytes, 10))
		fp.Attributes[prefix+"virtual_bytes"] = structs.NewStringAttribute(strconv.FormatInt(disk.VirtualBytes, 10))
	}
	fp.Attributes[diskTotalUsed] = structs.NewStringAttribute(strconv.FormatInt(totalUsed, 10))
	fp.Attributes[diskTotalVirtual] = structs.NewStringAttribute(strconv.FormatInt(totalVirtual, 10))
}

// guestKinds reports which guest operating systems this node can run.
func guestKinds() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "linux,macos"
	}
	return "linux"
}

func joinOrphanList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}

// limaVersionCached returns `limactl --version`, cached for a few minutes.
func (d *Driver) limaVersionCached(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.limaVersion != "" && time.Since(d.limaVersionAt) < limaVersionTTL {
		return d.limaVersion, nil
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	v, err := d.boxm.Lima().Version(probe)
	if err != nil {
		return "", err
	}
	d.limaVersion = v
	d.limaVersionAt = time.Now()
	return v, nil
}

// orphans returns the lima instances the driver considers abandoned: their disk
// symlink target is gone, or no task has owned them for orphanGrace.
func (d *Driver) orphans(instances []*limactl.Instance) []string {
	attached := d.tasks.BoxIDs()
	now := time.Now()
	seen := make(map[string]bool, len(instances))
	var out []string

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unattached == nil {
		d.unattached = map[string]time.Time{}
	}
	for _, inst := range instances {
		seen[inst.Name] = true
		if d.boxm.OrphanReason(inst) != "" {
			out = append(out, inst.Name)
			continue
		}
		if _, ok := attached[inst.Name]; ok {
			delete(d.unattached, inst.Name)
			continue
		}
		first, ok := d.unattached[inst.Name]
		if !ok {
			d.unattached[inst.Name] = now
			continue
		}
		if now.Sub(first) > orphanGrace {
			out = append(out, inst.Name)
		}
	}
	for name := range d.unattached {
		if !seen[name] {
			delete(d.unattached, name)
		}
	}
	return out
}

// emit records a task event so the Nomad UI shows what happened to the VM.
func (d *Driver) emit(tc *drivers.TaskConfig, kind, message string) {
	d.logger.Info(message, "task", tc.ID, "box_event", kind)
	if tc == nil {
		return
	}
	err := d.EmitEvent(&drivers.TaskEvent{
		TaskID:      tc.ID,
		TaskName:    tc.Name,
		AllocID:     tc.AllocID,
		Timestamp:   time.Now(),
		Message:     message,
		Annotations: map[string]string{"kind": kind},
	})
	if err != nil {
		d.logger.Debug("cannot emit task event", "task", tc.ID, "error", err)
	}
}

// StartTask creates or adopts the box and leaves it running.
func (d *Driver) StartTask(tc *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if tc == nil {
		return nil, nil, fmt.Errorf("nil task config")
	}
	if d.config.identityErr != nil {
		return nil, nil, d.config.identityErr
	}
	if d.boxm == nil {
		return nil, nil, fmt.Errorf("driver is not configured")
	}
	var cfg TaskConfig
	if err := tc.DecodeDriverConfig(&cfg); err != nil {
		return nil, nil, fmt.Errorf("decode task config: %w", err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, nil, err
	}
	volumeDir, err := diskVolumePath(tc)
	if err != nil {
		return nil, nil, err
	}

	if old := d.tasks.Get(tc.ID); old != nil {
		d.emit(tc, "restart", "A previous box task with this ID is still tracked; detaching from it")
		d.stopGuestLogs(old)
		d.stopWatcher(old)
		old.finish(&drivers.ExitResult{ExitCode: 1, Err: fmt.Errorf("task handle superseded by a new start")})
		d.tasks.Delete(tc.ID)
	}

	h := newTaskHandle(tc, cfg)
	rep := func(kind, message string) { d.emit(tc, kind, message) }
	b, err := d.boxFor(h, volumeDir, rep)
	if err != nil {
		return nil, nil, err
	}

	st, err := b.Ensure(d.ctx)
	if err != nil {
		return nil, nil, err
	}
	h.setBoxDetails(boxState{
		VolumeDir:   volumeDir,
		DiskPath:    st.DiskPath,
		InstanceDir: b.InstanceDir(),
		CgroupMode:  st.CgroupMode,
	})
	h.markRunning(time.Now())

	if _, err := b.CopyTaskFiles(d.ctx, tc.TaskDir().LocalDir, tc.TaskDir().SecretsDir, cfg.FilesDirInGuest, cfg.SecretsDirInGuest); err != nil {
		rep("files", fmt.Sprintf("Could not install the task files in the guest: %v", err))
		// The VM is up but its task can never run: take it away, so a retry
		// starts cleanly instead of adopting an instance whose disk symlink may
		// point at a volume that was deleted in the meantime.
		if discardErr := b.Discard(d.ctx); discardErr != nil {
			rep("files", fmt.Sprintf("Could not remove the failed box %s: %v", cfg.BoxID, discardErr))
		}
		return nil, nil, err
	}

	// The guest now has these files; a later re-render (a rotated credential)
	// changes this digest and the watcher copies them again.
	h.setFilesDigest(taskFilesDigest(tc))

	d.startGuestLogs(d.ctx, h)
	d.startWatcher(h, b)
	d.tasks.Add(h)
	rep("ready", fmt.Sprintf("Box %s is ready: VM %s with %d vCPU and %d MiB RAM; task files in %s, credentials on the tmpfs in %s",
		cfg.BoxID, st.Instance.StatusMessage(), cfg.CPUs, cfg.MemoryMiB, cfg.FilesDirInGuest, cfg.SecretsDirInGuest))

	stateData, err := h.encodeState()
	if err != nil {
		return nil, nil, err
	}
	handle := &drivers.TaskHandle{Version: taskHandleVersion, Config: tc, State: drivers.TaskStateRunning}
	handle.DriverState = stateData
	return handle, nil, nil
}

// diskVolumePath returns the host path of the dynamic host volume the task
// mounts for the box disk.
//
// Nomad names a host volume mount after the volume itself (the DHV name, for
// example "boxdisk"), while a CSI or static volume request keeps the name from
// the jobspec. A box has exactly one disk, so a single mount is unambiguous; with
// several mounts the disk one must be named "disk".
func diskVolumePath(tc *drivers.TaskConfig) (string, error) {
	var names []string
	for _, m := range tc.Mounts {
		if m.RequestName == defaultDiskVolumeRequest {
			if m.HostPath == "" {
				return "", fmt.Errorf("the mount named %q has an empty host path", defaultDiskVolumeRequest)
			}
			return m.HostPath, nil
		}
		names = append(names, fmt.Sprintf("%s (%s)", m.RequestName, m.HostPath))
	}
	if len(tc.Mounts) == 0 {
		return "", fmt.Errorf("the box task needs a volume mount to hold its disk, but the task has no mounts")
	}
	if len(tc.Mounts) == 1 {
		if tc.Mounts[0].HostPath == "" {
			return "", fmt.Errorf("the mount %q has an empty host path", tc.Mounts[0].RequestName)
		}
		return tc.Mounts[0].HostPath, nil
	}
	return "", fmt.Errorf("the box task needs one volume mount to hold its disk (named %q); found: %s", defaultDiskVolumeRequest, joinStrings(names))
}

func joinStrings(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// boxFor rebuilds the box handle from a task handle.
func (d *Driver) boxFor(h *taskHandle, volumeDir string, rep box.Reporter) (*box.Box, error) {
	spec := box.Spec{
		BoxID:       h.boxID,
		VolumeDir:   volumeDir,
		ImageURL:    h.cfg.Image.URL,
		ImageSHA256: h.cfg.Image.SHA256,
		CPUs:        h.cfg.CPUs,
		MemoryMiB:   h.cfg.MemoryMiB,
		DiskGiB:     h.cfg.DiskGiB,
	}
	return d.boxm.Box(spec, rep)
}

// startWatcher finishes the task handle when the VM stops.
func (d *Driver) startWatcher(h *taskHandle, b *box.Box) {
	ctx, cancel := context.WithCancel(d.ctx)
	h.mu.Lock()
	h.watchStop = cancel
	h.mu.Unlock()
	go func() {
		defer cancel()
		ticker := time.NewTicker(watchInterval)
		defer ticker.Stop()
		// The task files are re-read every fileRefreshEvery ticks: Nomad re-renders
		// a template whose Variable changed without restarting the VM, and a
		// rotated credential has to reach the guest's tmpfs.
		const fileRefreshEvery = 5
		ticks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.done:
				return
			case <-ticker.C:
			}
			ticks++
			if ticks%fileRefreshEvery == 0 {
				d.refreshTaskFiles(h, b)
			}
			// Fast path: the pid files lima writes say whether the VM and its
			// hostagent are still there. `limactl list` is only needed when they
			// are gone, to tell a clean stop from a crash.
			if live := b.Liveness(); live.Known {
				if live.DriverAlive && live.HostAgentAlive {
					continue
				}
				d.stopGuestLogs(h)
				reason := "its qemu process is gone"
				if live.DriverAlive && !live.HostAgentAlive {
					reason = "its lima hostagent is gone"
				}
				d.emit(h.tc, "exit", fmt.Sprintf("Box %s stopped: %s", h.boxID, reason))
				h.finish(&drivers.ExitResult{ExitCode: 1, Err: fmt.Errorf("box %s stopped: %s", h.boxID, reason)})
				return
			}
			checkCtx, cancelCheck := context.WithTimeout(ctx, 15*time.Second)
			inst, err := b.Status(checkCtx)
			cancelCheck()
			if err != nil {
				d.logger.Warn("cannot check box state", "task", h.tc.ID, "error", err)
				continue
			}
			if d.boxm.Alive(inst) {
				continue
			}
			d.stopGuestLogs(h)
			res := &drivers.ExitResult{}
			status := "gone"
			if inst != nil {
				status = inst.StatusMessage()
				if inst.Status != limactl.StatusStopped {
					res.ExitCode = 1
					res.Err = fmt.Errorf("box %s ended in state %s", h.boxID, status)
				}
			} else {
				res.ExitCode = 1
				res.Err = fmt.Errorf("box %s disappeared", h.boxID)
			}
			d.emit(h.tc, "exit", fmt.Sprintf("Box %s stopped: %s", h.boxID, status))
			h.finish(res)
			return
		}
	}()
}

func (d *Driver) stopWatcher(h *taskHandle) {
	h.mu.Lock()
	cancel := h.watchStop
	h.watchStop = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// RecoverTask re-attaches to a box that is still running, and refuses to hand
// back a fake handle when it is not.
func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil || handle.Config == nil {
		return fmt.Errorf("cannot recover a task without a handle")
	}
	st, err := decodeDriverState(handle.DriverState)
	if err != nil {
		return err
	}
	// The driver config bytes are not part of the handle Nomad persists, so the
	// task config comes from the state the driver wrote itself.
	cfg := st.Config
	if cfg.BoxID == "" {
		if err := handle.Config.DecodeDriverConfig(&cfg); err != nil {
			return fmt.Errorf("decode task config from the handle: %w", err)
		}
	}
	if err := cfg.applyDefaults(); err != nil {
		return err
	}
	if cfg.BoxID != st.BoxID {
		return fmt.Errorf("task handle names box %q but the task config names %q", st.BoxID, cfg.BoxID)
	}
	if d.boxm == nil {
		return fmt.Errorf("driver is not configured")
	}

	h := newTaskHandle(handle.Config, cfg)
	h.setBoxDetails(boxState{
		VolumeDir:   st.VolumeDir,
		DiskPath:    st.DiskPath,
		InstanceDir: st.InstanceDir,
		CgroupMode:  st.CgroupMode,
	})

	b, err := d.boxFor(h, st.VolumeDir, func(kind, message string) { d.emit(handle.Config, kind, message) })
	if err != nil {
		return err
	}
	inst, err := b.Status(d.ctx)
	if err != nil {
		return fmt.Errorf("check box %s: %w", st.BoxID, err)
	}
	if reason := d.boxm.ReattachReason(inst); reason != "" {
		return fmt.Errorf("cannot re-attach to box %s: %s", st.BoxID, reason)
	}
	h.markRunning(time.Now())
	d.startGuestLogs(d.ctx, h)
	d.startWatcher(h, b)
	d.tasks.Add(h)
	d.emit(handle.Config, "recover", fmt.Sprintf("Re-attached to the running box %s (pid %d); the VM was not restarted", st.BoxID, inst.DriverPID))
	return nil
}

// WaitTask blocks until the VM stops.
func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	h := d.tasks.Get(taskID)
	if h == nil {
		return nil, fmt.Errorf("task %q is not running", taskID)
	}
	return h.wait(), nil
}

// StopTask stops the VM, first gracefully and then by force.
func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	h := d.tasks.Get(taskID)
	if h == nil {
		return fmt.Errorf("task %q is not running", taskID)
	}
	// Nomad uses the detach signal to hand the box to a replacement allocation:
	// stop watching it, but leave the VM and its disk untouched.
	if signal == drivers.DetachSignal {
		d.stopGuestLogs(h)
		d.stopWatcher(h)
		d.emit(h.tc, "detach", fmt.Sprintf("Detached from box %s; the VM keeps running", h.boxID))
		return nil
	}
	// A box is shared by every allocation that names it. When Nomad stops a
	// superseded allocation (a job update, a reconnect after the node was marked
	// down), that allocation must not take the VM away from the one that still
	// uses it: detach and leave the VM and its disk alone.
	if others := d.tasks.OthersForBox(h.boxID, h.tc.ID); len(others) > 0 {
		d.stopGuestLogs(h)
		d.stopWatcher(h)
		d.emit(h.tc, "detach", fmt.Sprintf("Not stopping box %s: %d other allocation(s) still use it", h.boxID, len(others)))
		h.finish(&drivers.ExitResult{})
		return nil
	}
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	b, err := d.boxFor(h, h.volumeDir, func(kind, message string) { d.emit(h.tc, kind, message) })
	if err != nil {
		return err
	}
	return b.Stop(d.ctx, timeout)
}

// DestroyTask stops the VM if needed. It never deletes the Lima instance or the
// disk: those are the box, and they live on the host volume.
func (d *Driver) DestroyTask(taskID string, force bool) error {
	h := d.tasks.Get(taskID)
	if h == nil {
		return nil
	}
	d.stopGuestLogs(h)
	d.stopWatcher(h)
	defer d.tasks.Delete(taskID)

	if others := d.tasks.OthersForBox(h.boxID, h.tc.ID); len(others) > 0 {
		d.emit(h.tc, "detach", fmt.Sprintf("Not destroying box %s: %d other allocation(s) still use it", h.boxID, len(others)))
		h.finish(&drivers.ExitResult{})
		return nil
	}
	b, err := d.boxFor(h, h.volumeDir, func(kind, message string) { d.emit(h.tc, kind, message) })
	if err != nil {
		d.logger.Warn("cannot rebuild box handle for destroy", "task", taskID, "error", err)
		h.finish(&drivers.ExitResult{})
		return nil
	}
	inst, err := b.Status(d.ctx)
	if err != nil {
		d.logger.Warn("cannot check box state during destroy", "task", taskID, "error", err)
	} else if inst != nil && inst.IsRunning() {
		timeout := defaultStopTimeout
		if force {
			timeout = 10 * time.Second
		}
		if err := b.Stop(d.ctx, timeout); err != nil {
			if !force {
				return fmt.Errorf("stop box %s: %w", h.boxID, err)
			}
			d.logger.Warn("box is still running after a forced destroy", "task", taskID, "error", err)
		}
	}
	h.finish(&drivers.ExitResult{})
	return nil
}

// InspectTask returns the driver's view of the task.
func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	h := d.tasks.Get(taskID)
	if h == nil {
		return nil, fmt.Errorf("task %q is not running", taskID)
	}
	return h.taskStatus(), nil
}

// TaskStats returns periodic resource usage of the VM processes.
func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	h := d.tasks.Get(taskID)
	if h == nil {
		return nil, fmt.Errorf("task %q is not running", taskID)
	}
	ch := make(chan *drivers.TaskResourceUsage, 1)
	statsCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(ch)
		defer cancel()
		d.streamStats(statsCtx, h, interval, ch)
	}()
	return ch, nil
}

// ExecTask runs a command inside the guest and returns its buffered output.
func (d *Driver) ExecTask(taskID string, args []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	h := d.tasks.Get(taskID)
	if h == nil {
		return nil, fmt.Errorf("task %q is not running", taskID)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("exec needs a command")
	}
	ctx := d.ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	stdout, stderr, err := d.boxm.Lima().Shell(ctx, h.boxID, args)
	res := &drivers.ExecTaskResult{
		Stdout:     []byte(stdout),
		Stderr:     []byte(stderr),
		ExitResult: &drivers.ExitResult{},
	}
	if err != nil {
		res.ExitResult.ExitCode = exitCode(err)
		res.ExitResult.Err = err
	}
	return res, nil
}

// exitCode extracts a process exit code from a limactl error.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// SignalTask is not supported: signals are sent to the VM, not to a process.
func (d *Driver) SignalTask(taskID, signal string) error {
	return fmt.Errorf("the box driver does not send signals to the VM (task %q, signal %q)", taskID, signal)
}

// Shutdown stops the driver's background work. Running boxes keep running: they
// are recovered by RecoverTask when the plugin comes back.
func (d *Driver) Shutdown(ctx context.Context) error {
	d.cancel()
	return nil
}

// compile-time proof that the driver satisfies the plugin interfaces.
var (
	_ drivers.DriverPlugin            = (*Driver)(nil)
	_ drivers.DriverShutdowner        = (*Driver)(nil)
	_ drivers.ExecTaskStreamingDriver = (*Driver)(nil)
)
