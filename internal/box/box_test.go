package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

func collectEvents() (func(kind, message string), *[]string) {
	var events []string
	return func(kind, message string) {
		events = append(events, kind+": "+message)
	}, &events
}

// TestEnsureCreatesFreshBox covers the "instance absent, volume empty" branch:
// create, move the new disk into the volume, symlink it, start.
func TestEnsureCreatesFreshBox(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	spec := testSpec(t, vol)
	rep, events := collectEvents()

	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.True(t, st.Created)
	require.False(t, st.Adopted)
	require.Equal(t, "Running", st.Instance.Status)
	require.Equal(t, filepath.Join(vol, "box-box1.qcow2"), st.DiskPath)

	// The disk lima created in the instance directory now lives in the volume and
	// the instance directory only holds a symlink to it.
	require.Equal(t, "FRESH-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")))
	require.Equal(t, filepath.Join(vol, "box-box1.qcow2"), symlinkTarget(t, f.DiskPath("box1")))

	require.True(t, f.HasCall(t, "create --name box1 --tty=false"))
	require.True(t, f.HasCall(t, "start --tty=false box1"))
	require.Equal(t, "Running", f.State(t)["box1"]["status"])
	require.Contains(t, strings.Join(*events, "\n"), "create: No disk for box box1")
	require.Contains(t, strings.Join(*events, "\n"), "start: Box box1 is running")
}

// TestEnsureAdoptsRunningBox covers the "instance already running" branch: no
// create, no start, no resize.
func TestEnsureAdoptsRunningBox(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{
		"status": "Running", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024,
	})

	spec := testSpec(t, vol)
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.True(t, st.Adopted)
	require.False(t, st.Created)
	require.False(t, st.Resized)
	require.Empty(t, f.CallsWith(t, "create"))
	require.Empty(t, f.CallsWith(t, "start"))
	require.Empty(t, f.CallsWith(t, "edit"))
	require.Equal(t, "OLD-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")))
	require.Contains(t, strings.Join(*events, "\n"), "already running with the requested configuration")
}

// TestEnsureResizesStoppedBox covers stop -> edit -> start for a config change.
func TestEnsureResizesStoppedBox(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{
		"status": "Stopped", "cpus": 1, "memory": int64(1024) * 1024 * 1024, "disk": int64(4) * 1024 * 1024 * 1024,
	})

	spec := testSpec(t, vol)
	spec.CPUs = 2
	spec.MemoryMiB = 2048
	spec.DiskGiB = 8
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.True(t, st.Resized)
	edits := f.CallsWith(t, "edit")
	require.Len(t, edits, 1)
	require.Contains(t, edits[0], "--cpus 2")
	require.Contains(t, edits[0], "--memory 2")
	require.Contains(t, edits[0], "--disk 8")
	require.True(t, f.HasCall(t, "start --tty=false box1"))
	require.Equal(t, "Running", f.State(t)["box1"]["status"])
	require.Contains(t, strings.Join(*events, "\n"), "resize: Resizing box box1 offline")
}

// TestEnsureResizesRunningBox covers the replacement-allocation case: the new
// allocation finds the old VM still running with a different configuration.
func TestEnsureResizesRunningBox(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{
		"status": "Running", "cpus": 1, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024,
	})

	spec := testSpec(t, vol)
	spec.CPUs = 4
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.True(t, st.Resized)
	require.True(t, st.Adopted)
	require.Equal(t, "Running", st.Instance.Status)
	require.True(t, f.HasCall(t, "stop box1"))
	require.True(t, f.HasCall(t, "edit --tty=false --cpus 4"))
	require.Equal(t, 4, int(f.State(t)["box1"]["cpus"].(float64)))
	require.Contains(t, strings.Join(*events, "\n"), "runs an older configuration")
	require.Equal(t, "OLD-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")))
}

// TestEnsureResizeNeverShrinksDisk keeps guest data safe when disk_gib shrinks.
func TestEnsureResizeNeverShrinksDisk(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{
		"status": "Stopped", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024,
	})

	spec := testSpec(t, vol) // cpus 2, memory 1024, disk 8 -> only disk differs (4)
	spec.DiskGiB = 4
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.False(t, st.Resized)
	require.Empty(t, f.CallsWith(t, "edit"))
	require.True(t, f.HasCall(t, "start --tty=false box1"))
	require.Contains(t, strings.Join(*events, "\n"), "disks can only grow")
}

// TestEnsureRecoversBrokenBox covers the "hostagent gone" branch: stop -f, start.
func TestEnsureRecoversBrokenBox(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{
		"status": "Broken", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024,
		"hostAgentPID": deadPID(t),
	})

	spec := testSpec(t, vol)
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.Equal(t, "Running", st.Instance.Status)
	require.True(t, f.HasCall(t, "stop -f box1"))
	require.True(t, f.HasCall(t, "start --tty=false box1"))
	require.Contains(t, strings.Join(*events, "\n"), "forcing a stop")
	require.Equal(t, "OLD-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")))
}

// TestEnsureRecreatesInstanceAroundExistingDisk covers the "instance directory
// lost, disk still in the volume" branch: never overwrite the disk.
func TestEnsureRecreatesInstanceAroundExistingDisk(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")

	spec := testSpec(t, vol)
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)

	require.False(t, st.Created)
	require.True(t, st.Adopted)
	require.Equal(t, "OLD-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")), "the existing disk must survive")
	require.Equal(t, filepath.Join(vol, "box-box1.qcow2"), symlinkTarget(t, f.DiskPath("box1")))
	require.True(t, f.HasCall(t, "create --name box1"))
	require.True(t, f.HasCall(t, "start --tty=false box1"))
	require.Contains(t, strings.Join(*events, "\n"), "recreating the instance around the existing disk")
}

// TestEnsureRefusesTwoDisks never guesses which disk is the box.
func TestEnsureRefusesTwoDisks(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "VOLUME-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.WriteFile(f.DiskPath("box1"), []byte("INSTANCE-DISK"), 0o644))
	f.SetState(t, "box1", map[string]any{"status": "Stopped", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024})

	b, err := m.Box(testSpec(t, vol), nil)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "two disks")
	require.Equal(t, "VOLUME-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")))
	require.Equal(t, "INSTANCE-DISK", readFile(t, f.DiskPath("box1")))
	require.Empty(t, f.CallsWith(t, "start"))
}

// TestEnsureRepointsDanglingSymlink heals a disk symlink left behind by a moved
// volume without touching any real file.
func TestEnsureRepointsDanglingSymlink(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{"status": "Stopped", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024})

	b, err := m.Box(testSpec(t, vol), nil)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, filepath.Join(vol, "box-box1.qcow2"), symlinkTarget(t, f.DiskPath("box1")))
}

// TestEnsureReportsStartFailure surfaces limactl's own message.
func TestEnsureReportsStartFailure(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	f.SetMode(t, map[string]any{"fail_start": true})

	b, err := m.Box(testSpec(t, vol), nil)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "start box box1")
	require.Contains(t, err.Error(), "start failed")
}

// TestEnsureValidatesSpec covers the task-config errors the driver must reject.
func TestEnsureValidatesSpec(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)

	cases := map[string]func(*Spec){
		"bad box id":   func(s *Spec) { s.BoxID = "Box_1" },
		"empty box id": func(s *Spec) { s.BoxID = "" },
		"no volume":    func(s *Spec) { s.VolumeDir = "" },
		"no image url": func(s *Spec) { s.ImageURL = "" },
		"no sha":       func(s *Spec) { s.ImageSHA256 = "" },
		"zero cpus":    func(s *Spec) { s.CPUs = 0 },
		"zero memory":  func(s *Spec) { s.MemoryMiB = 0 },
		"zero disk":    func(s *Spec) { s.DiskGiB = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := testSpec(t, volumeDir(t))
			mutate(&spec)
			b, err := m.Box(spec, nil)
			if err == nil {
				_, err = b.Ensure(context.Background())
			}
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidSpec) || strings.Contains(err.Error(), "volume directory"), "unexpected error: %v", err)
		})
	}
}

// TestEnsureImageCacheHit verifies the sha256 cache: one download, then reuse.
func TestEnsureImageCacheHit(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	var hits int64
	payload := "IMAGE-BYTES"
	sum := sha256.Sum256([]byte(payload))
	url := imageURL(t, payload, &hits)
	spec := testSpec(t, vol)
	spec.ImageURL = url
	spec.ImageSHA256 = hex.EncodeToString(sum[:])

	b, err := m.Box(spec, nil)
	require.NoError(t, err)
	path, err := b.EnsureImage(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)
	require.Equal(t, payload, readFile(t, path))
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))

	second, err := b.EnsureImage(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)
	require.Equal(t, path, second)
	require.Equal(t, int64(1), atomic.LoadInt64(&hits), "a cached image must not be downloaded again")

	// A corrupted cache entry is detected and replaced.
	require.NoError(t, os.WriteFile(path, []byte("TAMPERED"), 0o644))
	third, err := b.EnsureImage(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)
	require.Equal(t, path, third)
	require.Equal(t, int64(2), atomic.LoadInt64(&hits))
	require.Equal(t, payload, readFile(t, third))
}

// TestEnsureImageRejectsWrongDigest never installs an image it cannot verify.
func TestEnsureImageRejectsWrongDigest(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	var hits int64
	url := imageURL(t, "ACTUAL", &hits)
	spec := testSpec(t, volumeDir(t))
	b, err := m.Box(spec, nil)
	require.NoError(t, err)
	_, err = b.EnsureImage(context.Background(), url, strings.Repeat("0", 64), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sha256")
	entries, err := os.ReadDir(m.Options().ImageCacheDir)
	require.NoError(t, err)
	require.Empty(t, entries, "no image file may be left in the cache")
}

// TestEnsureImageRejectsBadSHA covers input validation.
func TestEnsureImageRejectsBadSHA(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	b, err := m.Box(testSpec(t, volumeDir(t)), nil)
	require.NoError(t, err)
	_, err = b.EnsureImage(context.Background(), "http://example.invalid/x", "nothex", nil)
	require.Error(t, err)
	_, err = b.EnsureImage(context.Background(), "http://example.invalid/x", strings.Repeat("z", 64), nil)
	require.Error(t, err)
}

// TestCopyTaskFiles copies rendered templates and secrets into the guest: the
// job files onto the box disk, the credentials onto the guest's tmpfs.
func TestCopyTaskFiles(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{"status": "Running", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024})

	b, err := m.Box(testSpec(t, vol), nil)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.NoError(t, err)

	local := t.TempDir()
	secrets := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(local, "config.yaml"), []byte("a: b"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(local, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(local, "nested", "extra.conf"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "token"), []byte("s3cret"), 0o600))

	n, err := b.CopyTaskFiles(context.Background(), local, secrets, "/etc/box", DefaultSecretsDirInGuest)
	require.NoError(t, err)
	require.Equal(t, 3, n)

	copies := f.Copies(t)
	require.Len(t, copies, 3)
	joined := strings.Join(copies, "\n")
	// Everything is staged on the tmpfs, so a secret never passes the disk.
	require.Contains(t, joined, "box1:/run/box/stage-box1-a1b2c3d4/config.yaml")
	require.Contains(t, joined, "box1:/run/box/stage-box1-a1b2c3d4/nested/extra.conf")
	require.Contains(t, joined, "box1:/run/box/stage-box1-a1b2c3d4/token")

	var installs []string
	for _, c := range f.CallsWith(t, "shell box1 --") {
		if strings.Contains(c, "cp -a") {
			installs = append(installs, c)
		}
	}
	require.Len(t, installs, 2, "one install for the job files, one for the credentials")
	localInstall, secretInstall := installs[0], installs[1]
	require.Contains(t, localInstall, "install -d -m 0755 -o root -g root '/etc/box/local'")
	require.Contains(t, localInstall, "chmod -R u+rwX,go-rwx")
	require.NotContains(t, localInstall, DefaultSecretsDirInGuest)

	require.Contains(t, secretInstall, "install -d -m 0700 -o root -g root '"+DefaultSecretsDirInGuest+"'")
	require.Contains(t, secretInstall, "chown -R root:root '"+DefaultSecretsDirInGuest+"'")
	require.Contains(t, secretInstall, "chmod -R u+rwX,go-rwx '"+DefaultSecretsDirInGuest+"'")

	// A box without credentials still gets the (empty, root-only) directory, so
	// boxd reads a directory and not an error.
	f.ResetCalls(t)
	n, err = b.CopyTaskFiles(context.Background(), t.TempDir(), t.TempDir(), "/etc/box", DefaultSecretsDirInGuest)
	require.NoError(t, err)
	require.Zero(t, n)
	shells := f.CallsWith(t, "shell")
	require.Len(t, shells, 1, "only the secrets directory is created: %v", shells)
	require.Contains(t, shells[0], "install -d -m 0700 -o root -g root '"+DefaultSecretsDirInGuest+"'")
	require.Empty(t, f.Copies(t), "no file to copy")
}

// TestCopyTaskFilesErrorIsReported fails the start when the guest copy fails.
func TestCopyTaskFilesErrorIsReported(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	b, err := m.Box(testSpec(t, vol), nil)
	require.NoError(t, err)
	local := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(local, "f"), []byte("x"), 0o644))
	f.SetMode(t, map[string]any{"fail_copy": true})
	_, err = b.CopyTaskFiles(context.Background(), local, "", "/etc/box", DefaultSecretsDirInGuest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "copy")
}

// TestStopEscalatesToForce covers a graceful stop that fails.
func TestStopEscalatesToForce(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	writeDisk(t, vol, "box1", "OLD-DISK")
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	f.SetState(t, "box1", map[string]any{"status": "Running", "cpus": 2, "memory": int64(1024) * 1024 * 1024, "disk": int64(8) * 1024 * 1024 * 1024})
	f.SetMode(t, map[string]any{"fail_stop": true})

	b, err := m.Box(testSpec(t, vol), nil)
	require.NoError(t, err)
	require.NoError(t, b.Stop(context.Background(), 5e9))
	require.True(t, f.HasCall(t, "stop box1"))
	require.True(t, f.HasCall(t, "stop -f box1"))
	require.Equal(t, "Stopped", f.State(t)["box1"]["status"])
}

// TestOrphanReason describes the two orphan cases the fingerprint reports.
func TestOrphanReason(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)

	healthy := &limactl.Instance{Name: "box1", Dir: f.InstanceDir("box1")}
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	writeDisk(t, vol, "box1", "DISK")
	require.NoError(t, os.Symlink(filepath.Join(vol, "box-box1.qcow2"), f.DiskPath("box1")))
	require.Empty(t, m.OrphanReason(healthy))

	require.NoError(t, os.Remove(filepath.Join(vol, "box-box1.qcow2")))
	require.Equal(t, "disk volume file missing", m.OrphanReason(healthy))

	require.NoError(t, os.Remove(f.DiskPath("box1")))
	require.NoError(t, os.WriteFile(f.DiskPath("box1"), []byte("LOCAL"), 0o644))
	require.Equal(t, "disk is not on a volume", m.OrphanReason(healthy))

	require.NoError(t, os.Remove(f.DiskPath("box1")))
	require.Equal(t, "no disk file", m.OrphanReason(healthy))
}

// TestReattachReason decides whether RecoverTask may re-attach.
func TestReattachReason(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)

	require.Contains(t, m.ReattachReason(nil), "does not exist")
	stopped := &limactl.Instance{Name: "box1", Status: limactl.StatusStopped}
	require.Contains(t, m.ReattachReason(stopped), "Stopped")
	// Broken with a dead hostagent cannot be re-attached ...
	broken := &limactl.Instance{Name: "box1", Status: limactl.StatusBroken, HostAgentPID: deadPID(t)}
	require.Contains(t, m.ReattachReason(broken), "lost control")
	// ... but Broken with a live hostagent can: lima reports Broken whenever its
	// hostagent API does not answer within three seconds, which is transient.
	degraded := &limactl.Instance{Name: "box1", Status: limactl.StatusBroken, HostAgentPID: os.Getppid()}
	require.Empty(t, m.ReattachReason(degraded))
	// A broken instance that does not even report a hostagent pid is gone.
	require.Contains(t, m.ReattachReason(&limactl.Instance{Name: "box1", Status: limactl.StatusBroken}), "lost control")
	// A running instance whose hostagent pid is alive is re-attachable.
	running := &limactl.Instance{Name: "box1", Status: limactl.StatusRunning, HostAgentPID: os.Getppid()}
	require.Empty(t, m.ReattachReason(running))
	// A running instance with a dead hostagent is not.
	dead := &limactl.Instance{Name: "box1", Status: limactl.StatusRunning, HostAgentPID: 1 << 22}
	require.Contains(t, m.ReattachReason(dead), "hostagent")
}

// TestRenderLimaYAML keeps the generated config inside the fields Lima 2.2
// accepts: an unknown field only warns, but every limactl call then prints the
// warning into the task output.
func TestRenderLimaYAML(t *testing.T) {
	spec := Spec{BoxID: "box1", CPUs: 2, MemoryMiB: 1024, DiskGiB: 8, VMType: VMTypeQEMU}
	doc, err := renderLimaYAML(spec, "/cache/image.img")
	require.NoError(t, err)
	text := string(doc)
	for _, want := range []string{"vmType: qemu", "cpus: 2", "memory: 1024MiB", "disk: 8GiB", "location: /cache/image.img", "mounts: []", "containerd:", "system: false"} {
		require.Contains(t, text, want)
	}
	require.NotContains(t, text, "hostname", "lima 2.2 rejects the hostname field")
	require.NotContains(t, text, "mounts:\n  - /home", "the guest must not see host directories")

	// The VM type and arch follow the platform.
	spec.VMType = ""
	doc, err = renderLimaYAML(spec, "/cache/image.img")
	require.NoError(t, err)
	require.Contains(t, string(doc), "vmType: "+DefaultVMType())
	require.Contains(t, string(doc), "arch: "+LimaArch())
}

// deadPID returns a pid that is certainly not a live process.
func deadPID(t *testing.T) int {
	t.Helper()
	const candidate = 1 << 22 // above the default pid_max on Linux
	require.False(t, processAlive(candidate), "pid %d is unexpectedly alive", candidate)
	return candidate
}

// TestStartFailsWithoutTheMemoryScope is B5: when the systemd user scope cannot
// be created the task fails with the reason. There is no fallback to an
// unlimited VM, because a box without its memory limit is not the reservation
// the task asked for.
func TestStartFailsWithoutTheMemoryScope(t *testing.T) {
	f := fakelima.New(t)
	m, err := New(Options{
		Limactl:           f.Bin,
		LimaHome:          f.Home,
		ImageCacheDir:     filepath.Join(f.Home, "_image-cache"),
		MemoryOverheadMiB: 150,
		VMType:            VMTypeQEMU,
		// A systemd-run that cannot be started stands in for a node without a
		// reachable user manager.
		SystemdRun: filepath.Join(t.TempDir(), "no-systemd-run-here"),
	})
	require.NoError(t, err)
	vol := volumeDir(t)
	spec := testSpec(t, vol)
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)

	_, err = b.Ensure(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "memory cgroup")
	require.Contains(t, err.Error(), `memory_limit = "off"`)
	require.Empty(t, f.CallsWith(t, "start"), "the VM must not be started without its memory limit")
	require.NotContains(t, strings.Join(*events, "\n"), "starts without a memory limit")
}

// TestStartWithoutTheMemoryLimitNeedsTheOperatorSwitch is the other half: with
// memory_limit = "off" the box starts, and the event says the cap is not
// enforced.
func TestStartWithoutTheMemoryLimitNeedsTheOperatorSwitch(t *testing.T) {
	f := fakelima.New(t)
	vol := volumeDir(t)
	spec := testSpec(t, vol)
	rep, events := collectEvents()

	m, err := New(Options{
		Limactl:           f.Bin,
		LimaHome:          f.Home,
		ImageCacheDir:     filepath.Join(f.Home, "_image-cache"),
		MemoryOverheadMiB: 150,
		VMType:            VMTypeQEMU,
		MemoryLimitOff:    true,
	})
	require.NoError(t, err)
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.NoError(t, err)
	require.Contains(t, strings.Join(*events, "\n"), `memory_limit is "off"`)
	require.True(t, f.HasCall(t, "start --tty=false box1"))
}
