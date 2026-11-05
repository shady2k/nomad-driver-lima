package driver

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// newTestDriver returns a configured driver bound to a fake limactl.
func newTestDriver(t *testing.T, f *fakelima.Fake) *Driver {
	t.Helper()
	d := NewDriver(context.Background(), hclog.NewNullLogger())
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	cfg := Config{
		LimactlPath:       f.Bin,
		LimaHome:          f.Home,
		FilesRoot:         filepath.Join(t.TempDir(), "box-files"),
		ImageCacheDir:     filepath.Join(f.Home, "_images"),
		MemoryOverheadMiB: 150,
		VMType:            "qemu",
	}
	var encoded []byte
	require.NoError(t, base.MsgPackEncode(&encoded, cfg))
	require.NoError(t, d.SetConfig(&base.Config{PluginConfig: encoded}))
	return d
}

// taskConfig returns a task that mounts a volume named "disk".
func taskConfig(t *testing.T, volumeDir string, mutate ...func(*drivers.TaskConfig)) *drivers.TaskConfig {
	t.Helper()
	allocDir := t.TempDir()
	logDir := filepath.Join(allocDir, "alloc", "logs")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	tc := &drivers.TaskConfig{
		ID:         "task1",
		Name:       "box",
		AllocID:    "alloc1",
		AllocDir:   allocDir,
		StdoutPath: filepath.Join(logDir, "stdout"),
		StderrPath: filepath.Join(logDir, "stderr"),
		Mounts: []*drivers.MountConfig{
			{RequestName: "disk", TaskPath: "/disk", HostPath: volumeDir},
		},
	}
	for _, m := range mutate {
		m(tc)
	}
	return tc
}

func startConfig(t *testing.T, url, sha string) TaskConfig {
	t.Helper()
	return TaskConfig{
		BoxID:           "box1",
		Image:           ImageConfig{URL: url, SHA256: sha},
		CPUs:            2,
		MemoryMiB:       1024,
		DiskGiB:         8,
		GuestLogCommand: []string{"true"},
		FilesDirInGuest: "/etc/box",
	}
}

func TestConfigValidate(t *testing.T) {
	c := Config{}
	require.NoError(t, c.validate())
	require.Equal(t, "limactl", c.LimactlPath)
	require.NotEmpty(t, c.LimaHome)
	require.Equal(t, box.DefaultVMType(), c.VMType)
	require.Equal(t, 512, c.MemoryOverheadMiB)

	require.Equal(t, box.DefaultMemoryLimit(), c.MemoryLimit)

	bad := Config{VMType: "vmware"}
	require.Error(t, bad.validate())

	// The memory limit is an operator decision with two values; anything else is
	// a typo that would otherwise silently disable the cap.
	require.NoError(t, (&Config{MemoryLimit: box.MemoryLimitOff}).validate())
	require.Error(t, (&Config{MemoryLimit: "disabled"}).validate())

	// memory_limit = "enforce" needs cgroup v2, which only Linux has.
	enforce := Config{MemoryLimit: box.MemoryLimitEnforce}
	if runtime.GOOS == "linux" {
		require.NoError(t, enforce.validate())
	} else {
		require.Error(t, enforce.validate())
	}
}

func TestTaskConfigApplyDefaults(t *testing.T) {
	c := TaskConfig{BoxID: "box1", Image: ImageConfig{URL: "http://x/y", SHA256: strings.Repeat("a", 64)}}
	require.NoError(t, c.applyDefaults())
	require.Equal(t, defaultCPUs, c.CPUs)
	require.Equal(t, defaultMemoryMiB, c.MemoryMiB)
	require.Equal(t, defaultDiskGiB, c.DiskGiB)
	require.Equal(t, defaultGuestLogCommand, c.GuestLogCommand)
	require.Equal(t, defaultFilesDir, c.FilesDirInGuest)

	cases := map[string]TaskConfig{
		"no box id": {Image: ImageConfig{URL: "u", SHA256: "s"}},
		"no image":  {BoxID: "box1"},
		"no url":    {BoxID: "box1", Image: ImageConfig{SHA256: "s"}},
		"no sha":    {BoxID: "box1", Image: ImageConfig{URL: "u"}},
		"bad sizes": {BoxID: "box1", Image: ImageConfig{URL: "u", SHA256: "s"}, CPUs: -1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, c.applyDefaults())
		})
	}
}

func TestDiskVolumePath(t *testing.T) {
	path, err := diskVolumePath(taskConfig(t, "/vol"))
	require.NoError(t, err)
	require.Equal(t, "/vol", path)

	// Nomad names a host volume mount after the volume, so a single mount is
	// used whatever its name.
	path, err = diskVolumePath(taskConfig(t, "/vol", func(tc *drivers.TaskConfig) {
		tc.Mounts = []*drivers.MountConfig{{RequestName: "boxdisk", HostPath: "/vol"}}
	}))
	require.NoError(t, err)
	require.Equal(t, "/vol", path)

	// A mount explicitly named "disk" wins when there are several.
	path, err = diskVolumePath(taskConfig(t, "/vol", func(tc *drivers.TaskConfig) {
		tc.Mounts = []*drivers.MountConfig{
			{RequestName: "other", HostPath: "/other"},
			{RequestName: "disk", HostPath: "/vol"},
		}
	}))
	require.NoError(t, err)
	require.Equal(t, "/vol", path)

	_, err = diskVolumePath(taskConfig(t, "/vol", func(tc *drivers.TaskConfig) { tc.Mounts = nil }))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no mounts")

	_, err = diskVolumePath(taskConfig(t, "/vol", func(tc *drivers.TaskConfig) {
		tc.Mounts = []*drivers.MountConfig{
			{RequestName: "data", HostPath: "/vol"},
			{RequestName: "logs", HostPath: "/logs"},
		}
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "found: data (/vol), logs (/logs)")
}

// TestStartTaskLifecycle drives create -> start -> inspect -> stop -> wait ->
// destroy through the real command boundary of a fake limactl.
func TestStartTaskLifecycle(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")

	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))

	handle, net, err := d.StartTask(tc)
	require.NoError(t, err)
	require.Nil(t, net)
	require.NotNil(t, handle)
	require.Equal(t, drivers.TaskStateRunning, handle.State)
	require.NotEmpty(t, handle.DriverState)

	inst := f.State(t)["box1"]
	require.Equal(t, "Running", inst["status"])
	require.True(t, f.HasCall(t, "create --name box1"))
	linkTarget, err := os.Readlink(f.DiskPath("box1"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(vol, "box-box1.qcow2"), linkTarget)

	status, err := d.InspectTask("task1")
	require.NoError(t, err)
	require.Equal(t, drivers.TaskStateRunning, status.State)
	require.Equal(t, "box1", status.DriverAttributes["box_id"])
	require.Equal(t, filepath.Join(vol, "box-box1.qcow2"), status.DriverAttributes["disk_path"])

	waitCh, err := d.WaitTask(context.Background(), "task1")
	require.NoError(t, err)

	require.NoError(t, d.StopTask("task1", 5*time.Second, ""))
	require.Equal(t, "Stopped", f.State(t)["box1"]["status"])

	select {
	case res := <-waitCh:
		require.True(t, res.Successful(), "a clean stop is exit 0, got %+v", res)
	case <-time.After(15 * time.Second):
		require.Fail(t, "WaitTask did not report the stopped VM")
	}

	// Destroy keeps the instance and the disk.
	require.NoError(t, d.DestroyTask("task1", false))
	_, err = d.InspectTask("task1")
	require.Error(t, err)
	require.Equal(t, "Stopped", f.State(t)["box1"]["status"])
	diskInfo, err := os.Stat(filepath.Join(vol, "box-box1.qcow2"))
	require.NoError(t, err)
	require.True(t, diskInfo.Mode().IsRegular())
	require.Empty(t, f.CallsWith(t, "delete"))
}

func TestStartTaskRequiresDiskVolume(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	tc := taskConfig(t, "/vol", func(tc *drivers.TaskConfig) { tc.Mounts = nil })
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, "http://x/y", strings.Repeat("a", 64))))
	_, _, err := d.StartTask(tc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "needs a volume mount")
}

func TestStartTaskRejectsInvalidBoxID(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	cfg := startConfig(t, "http://x/y", strings.Repeat("a", 64))
	cfg.BoxID = "Box_1"
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(cfg))
	_, _, err := d.StartTask(tc)
	require.Error(t, err)
	require.False(t, f.HasCall(t, "create"))
}

// TestRecoverTaskRefusesStoppedBox never hands back a fake handle.
func TestRecoverTaskRefusesStoppedBox(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	handle, _, err := d.StartTask(tc)
	require.NoError(t, err)

	// The VM went away while the client was down.
	f.SetState(t, "box1", map[string]any{"status": "Stopped"})
	require.Error(t, d.RecoverTask(handle))

	// Broken with a dead hostagent must not be faked into a handle ...
	f.SetState(t, "box1", map[string]any{"status": "Broken", "hostAgentPID": 1 << 22})
	require.Error(t, d.RecoverTask(handle))

	// ... while Broken with a live hostagent is re-attachable: lima reports
	// Broken whenever its hostagent API is slow, which happens on a busy host.
	f.SetState(t, "box1", map[string]any{"status": "Broken", "hostAgentPID": os.Getppid()})
	require.NoError(t, d.RecoverTask(handle))
}

// TestRecoverTaskReattachesRunningBox covers the good path: same VM, same disk.
func TestRecoverTaskReattachesRunningBox(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	handle, _, err := d.StartTask(tc)
	require.NoError(t, err)

	// A second driver (a restarted plugin) recovers the same handle.
	d2 := newTestDriver(t, f)
	f.ResetCalls(t)
	require.NoError(t, d2.RecoverTask(handle))
	status, err := d2.InspectTask("task1")
	require.NoError(t, err)
	require.Equal(t, drivers.TaskStateRunning, status.State)
	require.Empty(t, f.CallsWith(t, "start"), "recovery must not restart the VM")

	// Nomad does not persist the driver config bytes, so recovery must work from
	// the driver's own state alone.
	stripped := &drivers.TaskHandle{Version: handle.Version, State: handle.State, DriverState: handle.DriverState, Config: &drivers.TaskConfig{
		ID:         tc.ID,
		Name:       tc.Name,
		AllocID:    tc.AllocID,
		AllocDir:   tc.AllocDir,
		StdoutPath: tc.StdoutPath,
		StderrPath: tc.StderrPath,
	}}
	d3 := newTestDriver(t, f)
	require.NoError(t, d3.RecoverTask(stripped))

	// A handle without state is refused.
	require.Error(t, d2.RecoverTask(&drivers.TaskHandle{Config: tc}))
	// A handle for a different box id is refused.
	other := *handle
	other.DriverState = []byte(`{"version":1,"box_id":"box2"}`)
	require.Error(t, d2.RecoverTask(&other))
}

// TestFingerprintReportsInstancesAndOrphans covers the node attributes.
func TestFingerprintReportsInstancesAndOrphans(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)

	ch, err := d.Fingerprint(context.Background())
	require.NoError(t, err)
	fp := <-ch
	require.Equal(t, drivers.HealthStateHealthy, fp.Health)
	require.Equal(t, "2.2.0", attrValue(t, fp, "driver.box.lima_version"))
	require.Equal(t, "1", attrValue(t, fp, "driver.box.instances"))
	require.Equal(t, "linux", attrValue(t, fp, "driver.box.guests"))
	require.Equal(t, "qemu", attrValue(t, fp, "driver.box.vm_type"))
	require.NotEmpty(t, attrValue(t, fp, "driver.box.version"))
	require.Empty(t, attrValue(t, fp, "driver.box.orphans"), "a box with a task is not an orphan")
	require.Equal(t, box.DefaultMemoryLimit(), attrValue(t, fp, "driver.box.memory_limit"))

	// Every live instance is described with its size and its owner, so capacity
	// accounting can subtract the VMs Nomad does not cover.
	require.Equal(t, "2", attrValue(t, fp, "driver.box.instance.box1.cpus"))
	require.Equal(t, "1024", attrValue(t, fp, "driver.box.instance.box1.memory_mib"))
	require.Equal(t, "8", attrValue(t, fp, "driver.box.instance.box1.disk_gib"))
	require.Equal(t, "running", attrValue(t, fp, "driver.box.instance.box1.task"))

	// Lose the volume: the box becomes an orphan and is only reported.
	require.NoError(t, os.Remove(filepath.Join(vol, "box-box1.qcow2")))
	ch, err = d.Fingerprint(context.Background())
	require.NoError(t, err)
	fp = <-ch
	require.Equal(t, "box1", attrValue(t, fp, "driver.box.orphans"))
}

// TestFingerprintReportsMissingLimactl keeps the node unhealthy, not the process.
func TestFingerprintReportsMissingLimactl(t *testing.T) {
	d := NewDriver(context.Background(), hclog.NewNullLogger())
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	require.NoError(t, d.SetConfig(&base.Config{}))
	ch, err := d.Fingerprint(context.Background())
	require.NoError(t, err)
	fp := <-ch
	require.Equal(t, drivers.HealthStateUnhealthy, fp.Health)
	require.Contains(t, fp.HealthDescription, "limactl")
}

func TestCapabilitiesAndSchemas(t *testing.T) {
	d := NewDriver(context.Background(), hclog.NewNullLogger())
	caps, err := d.Capabilities()
	require.NoError(t, err)
	require.Equal(t, drivers.MountConfigSupportAll, caps.MountConfigs)
	require.True(t, caps.Exec)
	// An exec runs in the guest: Nomad must ask for alloc-exec, not for
	// alloc-node-exec, which it asks for when a driver has no isolation.
	require.Equal(t, fsisolation.Image, caps.FSIsolation)
	_, err = d.TaskConfigSchema()
	require.NoError(t, err)
	_, err = d.ConfigSchema()
	require.NoError(t, err)
	info, err := d.PluginInfo()
	require.NoError(t, err)
	require.Equal(t, "box", info.Name)
	require.Equal(t, base.PluginTypeDriver, info.Type)
	require.Equal(t, []string{drivers.ApiVersion010}, info.PluginApiVersions)
}

func TestDestroyUnknownTaskIsHarmless(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	require.NoError(t, d.DestroyTask("nope", true))
}

func TestStopUnknownTaskFails(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	require.Error(t, d.StopTask("nope", time.Second, ""))
	_, err := d.WaitTask(context.Background(), "nope")
	require.Error(t, err)
}

// TestStopAndDestroyLeaveASharedBoxAlone covers a stale allocation: after a node
// was marked down and came back, Nomad restores every allocation of the job and
// resumes their stop requests. One of them must not stop the VM that another
// allocation is still using.
func TestStopAndDestroyLeaveASharedBoxAlone(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")

	start := func(id string) *drivers.TaskConfig {
		tc := taskConfig(t, vol, func(tc *drivers.TaskConfig) { tc.ID = id; tc.Name = "box" })
		require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
		_, _, err := d.StartTask(tc)
		require.NoError(t, err)
		return tc
	}
	first := start("task1")
	f.ResetCalls(t)

	// A second allocation for the same box (the replacement after a reconnect).
	second := start("task2")
	require.Equal(t, "Running", f.State(t)["box1"]["status"])

	// The stale allocation is stopped: the VM must stay up for the live one.
	f.ResetCalls(t)
	require.NoError(t, d.StopTask("task1", 5*time.Second, ""))
	require.Empty(t, f.CallsWith(t, "stop"), "a superseded allocation must not stop the shared box")
	require.Equal(t, "Running", f.State(t)["box1"]["status"])

	// Same for a destroy.
	require.NoError(t, d.DestroyTask("task1", true))
	require.Empty(t, f.CallsWith(t, "stop"))
	require.Equal(t, "Running", f.State(t)["box1"]["status"])
	_, err := d.InspectTask("task1")
	require.Error(t, err)

	// The last allocation still stops the box.
	require.NoError(t, d.StopTask("task2", 5*time.Second, ""))
	require.NotEmpty(t, f.CallsWith(t, "stop"))
	require.Equal(t, "Stopped", f.State(t)["box1"]["status"])
	_ = first
	_ = second
}

// TestConfigResolvesRuntimeUser checks that the plugin block's runtime_user is
// resolved into the identity every Lima child runs as.
func TestConfigResolvesRuntimeUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process runs as root; the empty runtime_user is an error there")
	}
	c := Config{}
	require.NoError(t, c.validate())
	require.NoError(t, c.identityErr)
	require.NotNil(t, c.identity)
	require.Equal(t, os.Geteuid(), c.identity.UID)
	require.False(t, c.identity.Drop)

	// A user other than the driver's own is only possible for a root driver.
	c = Config{RuntimeUser: "root"}
	require.NoError(t, c.validate())
	require.Error(t, c.identityErr)
	require.Contains(t, c.identityErr.Error(), "root")
	require.Nil(t, c.identity)

	c = Config{RuntimeUser: "box-no-such-user-1c4a"}
	require.NoError(t, c.validate())
	require.Error(t, c.identityErr)
}

// TestConfigSpecOffersRuntimeUser keeps the HCL schema in step with the struct:
// a setting Nomad cannot decode would silently stay empty.
func TestConfigSpecOffersRuntimeUser(t *testing.T) {
	obj, ok := configSpec.Block.(*hclspec.Spec_Object)
	require.True(t, ok, "the plugin config is an object spec")
	require.Contains(t, obj.Object.Attributes, "runtime_user")
	// memory_limit is the operator switch B5 asks for: a setting Nomad cannot
	// decode would silently leave the cap enforced.
	require.Contains(t, obj.Object.Attributes, "memory_limit")
}

// TestConfigSpecLeavesPlatformDefaultsToValidate keeps a platform-dependent
// default out of the HCL schema: Nomad fills a schema default in before the
// driver sees the block, so a literal "enforce" there made a macOS node without
// memory_limit refuse the plugin instead of getting "off".
func TestConfigSpecLeavesPlatformDefaultsToValidate(t *testing.T) {
	obj, ok := configSpec.Block.(*hclspec.Spec_Object)
	require.True(t, ok, "the plugin config is an object spec")
	for _, key := range []string{"memory_limit", "vm_type"} {
		def := obj.Object.Attributes[key].GetDefault()
		require.NotNil(t, def, key)
		require.Equal(t, `""`, def.GetDefault().GetLiteral().GetValue(), key)
	}

	c := Config{}
	require.NoError(t, c.validate())
	require.Equal(t, box.DefaultMemoryLimit(), c.MemoryLimit)
}

// TestFingerprintReportsMissingRuntimeUser checks the root case: the plugin
// loads, and the fingerprint says in plain words what has to be set.
func TestFingerprintReportsMissingRuntimeUser(t *testing.T) {
	d := NewDriver(context.Background(), hclog.NewNullLogger())
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	cfg := Config{
		LimactlPath:   "/nonexistent/limactl",
		LimaHome:      filepath.Join(t.TempDir(), "lima"),
		ImageCacheDir: filepath.Join(t.TempDir(), "images"),
		VMType:        "qemu",
		RuntimeUser:   "root", // refused: Lima does not run as root
	}
	var encoded []byte
	require.NoError(t, base.MsgPackEncode(&encoded, cfg))
	require.NoError(t, d.SetConfig(&base.Config{PluginConfig: encoded}),
		"an unusable runtime_user is reported by the fingerprint, not by a plugin that fails to load")
	require.Nil(t, d.boxm)

	fp := d.buildFingerprint(context.Background())
	require.Equal(t, drivers.HealthStateUnhealthy, fp.Health)
	require.Contains(t, fp.HealthDescription, "runtime_user")

	// A start fails with the same message instead of a nil dereference.
	_, _, err := d.StartTask(taskConfig(t, t.TempDir()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "runtime_user")
}

// TestSetConfigOwnsTheDriverDirectories checks that the Lima home and the image
// cache are prepared for the runtime user, and that a start works afterwards.
func TestSetConfigOwnsTheDriverDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the directories are chowned, which needs a second user")
	}
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	require.NotNil(t, d.boxm)
	require.Equal(t, os.Geteuid(), d.boxm.Identity().UID)
	st, err := os.Stat(d.config.LimaHome)
	require.NoError(t, err)
	require.True(t, st.IsDir())
	st, err = os.Stat(d.config.ImageCacheDir)
	require.NoError(t, err)
	require.True(t, st.IsDir())

	fp := d.buildFingerprint(context.Background())
	require.Equal(t, d.boxm.Identity().Name, attrValue(t, fp, "driver.box.runtime_user"))
}

// TestRefreshTaskFilesOnRerender is the rotation path: Nomad re-renders a
// template without restarting the VM, the driver notices, copies the files into
// the guest's tmpfs again and asks boxd to reload its credentials.
func TestRefreshTaskFilesOnRerender(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")

	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	secrets := tc.TaskDir().SecretsDir
	require.NoError(t, os.MkdirAll(filepath.Join(secrets, "prime"), 0o700))
	authPath := filepath.Join(secrets, "prime", "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{"gateway":{"access":"one"}}`), 0o600))

	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	h := d.tasks.Get("task1")
	require.NotNil(t, h)
	b, err := d.boxFor(h, h.volumeDir, nil)
	require.NoError(t, err)

	// The guest mounts the box's host copy of the task files.
	staged := filepath.Join(d.boxm.FilesDir("box1"), "secrets", "prime", "auth.json")
	got, err := os.ReadFile(staged)
	require.NoError(t, err)
	require.Contains(t, string(got), "one")

	// Nothing changed: no reload.
	f.ResetCalls(t)
	d.refreshTaskFiles(h, b)
	require.Empty(t, f.CallsWith(t, "shell box1 -- boxctl reload"), "an unchanged task directory is not put in place again")

	// The Variable changed and Nomad re-rendered the template.
	require.NoError(t, os.WriteFile(authPath, []byte(`{"gateway":{"access":"two"}}`), 0o600))
	f.ResetCalls(t)
	d.refreshTaskFiles(h, b)

	got, err = os.ReadFile(staged)
	require.NoError(t, err)
	require.Contains(t, string(got), "two", "the new secret must reach the mounted directory")
	require.Empty(t, f.Copies(t), "nothing is copied into the guest any more")
	reloads := f.CallsWith(t, "shell box1 -- boxctl reload")
	require.Len(t, reloads, 1, "boxd must be told to reload: %v", f.Calls(t))
}

// TestTaskConfigCannotSetNodeSettings keeps the boundary B4 draws: the task
// config is written by the coordinator, so it must not be able to point the
// driver at another limactl, Lima home, image cache or user. Those come from the
// plugin block only.
func TestTaskConfigCannotSetNodeSettings(t *testing.T) {
	obj, ok := taskConfigSpec.Block.(*hclspec.Spec_Object)
	require.True(t, ok, "the task config is an object spec")
	for _, forbidden := range []string{"limactl", "lima_home", "image_cache_dir", "runtime_user", "memory_limit"} {
		require.NotContains(t, obj.Object.Attributes, forbidden,
			"%q is a setting of the node, not of the task", forbidden)
	}
	// And the plugin block does carry them.
	plugin, ok := configSpec.Block.(*hclspec.Spec_Object)
	require.True(t, ok)
	for _, key := range []string{"limactl", "lima_home", "image_cache_dir", "runtime_user", "memory_limit"} {
		require.Contains(t, plugin.Object.Attributes, key)
	}
}

// TestTaskConfigImageBlockIsOptional: a bake and a publish run no VM, so they
// carry no image block, and a required one refused every bake and publish job
// with "Missing image block" before the driver ran it. A box task still cannot
// start without one (TestTaskConfigValidate).
func TestTaskConfigImageBlockIsOptional(t *testing.T) {
	obj, ok := taskConfigSpec.Block.(*hclspec.Spec_Object)
	require.True(t, ok, "the task config is an object spec")
	image, ok := obj.Object.Attributes["image"]
	require.True(t, ok, "the task config has an image block")
	block, ok := image.Block.(*hclspec.Spec_BlockValue)
	require.True(t, ok, "image is a block")
	require.False(t, block.BlockValue.Required,
		"a bake and a publish carry no image, so the block must be optional")
	// The recipe a box's image carries travels in the same task config, and the
	// spec must accept it: a box created from a project image carries it, and a
	// driver without the attribute refused the whole job with
	// `No argument or block type is named "recipe"`.
	recipe, ok := obj.Object.Attributes["recipe"]
	require.True(t, ok, "the task config carries the image's recipe")
	attr, ok := recipe.Block.(*hclspec.Spec_Attr)
	require.True(t, ok, "recipe is an attribute")
	require.False(t, attr.Attr.Required, "an image without a recipe has none to carry")

	err := (&TaskConfig{BoxID: "box1", CPUs: 1, MemoryMiB: 1, DiskGiB: 1}).applyDefaults()
	require.EqualError(t, err, "image.url is required", "a box task still needs its image")
	require.NoError(t, (&TaskConfig{BoxID: "box1", Bake: &BakeConfig{
		Name: "proj", Version: "proj-1", Arch: "amd64"}}).applyDefaults())
	// A publish is not a box: no box id, and the schema must not demand one (the
	// real driver refused every publish job with `The argument "box_id" is
	// required`).
	boxID, ok := obj.Object.Attributes["box_id"]
	require.True(t, ok)
	boxIDAttr, ok := boxID.Block.(*hclspec.Spec_Attr)
	require.True(t, ok, "box_id is an attribute")
	require.False(t, boxIDAttr.Attr.Required, "a publish job carries no box id")
	require.NoError(t, (&TaskConfig{Publish: &PublishConfig{
		Version: "proj-1", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}).applyDefaults())
	require.EqualError(t, (&TaskConfig{}).applyDefaults(), "box_id is required",
		"a box task still needs its box id")
}

// TestExecReturnsWhenStdinStaysOpen: a caller whose stdin never ends (an
// agent's shell) still gets the exit of a command that does not read it.
func TestExecReturnsWhenStdinStaysOpen(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)

	// The exec is noted in the task's stderr, which the guest logs hold open.
	var notes syncBuffer
	h := d.tasks.Get(tc.ID)
	h.mu.Lock()
	h.logOut = &notes
	h.mu.Unlock()

	stdin, keepOpen := io.Pipe()
	defer keepOpen.Close()
	var out bytes.Buffer
	done := make(chan *drivers.ExitResult, 1)
	go func() {
		res, err := d.ExecTaskStreaming(context.Background(), tc.ID, &drivers.ExecOptions{
			Command: []string{"true"},
			Stdin:   stdin,
			Stdout:  nopWriteCloser{&out},
			Stderr:  nopWriteCloser{io.Discard},
		})
		require.NoError(t, err)
		done <- res
	}()
	select {
	case res := <-done:
		require.Equal(t, 0, res.ExitCode)
		require.Nil(t, res.Err, "an exec whose stdin is still open is not a failure")
		require.Contains(t, out.String(), "fake shell ok")
		require.Contains(t, notes.String(), "started: true")
		require.Contains(t, notes.String(), "exit 0 after")
	case <-time.After(execWaitDelay + 10*time.Second):
		t.Fatal("exec did not return while its stdin stayed open")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// syncBuffer is a bytes.Buffer safe for the writer goroutines of an exec.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
