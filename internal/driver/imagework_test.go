package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// A bake and a publish are tasks of the driver that run no VM: the first turns a
// sealed box's disk into an image in the node's cache, the second puts that image
// into the operator's store. Both are tested through StartTask and StopTask, the
// boundary Nomad uses.

// fakeQEMUImg writes a script that stands in for qemu-img: it copies its input to
// its output, which is what a conversion of a small file is.
func fakeQEMUImg(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qemu-img")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// okQEMUImg copies the last argument to the one before it, like
// "qemu-img convert -c -O qcow2 <src> <dst>".
const okQEMUImg = `#!/usr/bin/env bash
set -euo pipefail
args=("$@")
src=${args[${#args[@]}-2]}
dst=${args[${#args[@]}-1]}
cp "$src" "$dst"
`

// driverWithQEMUImg builds the driver over a fake limactl and a given qemu-img.
func driverWithQEMUImg(t *testing.T, qemuImg string) (*Driver, string) {
	t.Helper()
	return driverWithQEMUImgAt(t, fakelima.New(t), qemuImg)
}

// driverWithQEMUImgAt builds the driver over a given fake limactl: a bake drives
// the instances of that fake, so a test needs to look at them.
func driverWithQEMUImgAt(t *testing.T, f *fakelima.Fake, qemuImg string) (*Driver, string) {
	t.Helper()
	d := NewDriver(context.Background(), hclog.NewNullLogger())
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	cache := filepath.Join(f.Home, "_images")
	cfg := Config{
		LimactlPath:       f.Bin,
		LimaHome:          f.Home,
		FilesRoot:         filepath.Join(t.TempDir(), "box-files"),
		ImageCacheDir:     cache,
		QEMUImg:           qemuImg,
		MemoryOverheadMiB: 150,
		VMType:            "qemu",
	}
	var encoded []byte
	require.NoError(t, base.MsgPackEncode(&encoded, cfg))
	require.NoError(t, d.SetConfig(&base.Config{PluginConfig: encoded}))
	return d, cache
}

// bakePlatformSHA is the digest of the platform bundle a bake's clone mounts.
const bakePlatformSHA = "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0"

// bakePlatform seeds the node's platform cache with an unpacked bundle, which is
// what a box start downloads and unpacks: a bake mounts it and runs the bundle's
// boxctl.
func bakePlatform(t *testing.T, cache string) string {
	t.Helper()
	dir := filepath.Join(cache, "platform", bakePlatformSHA)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"1.10.0"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".box-platform-complete"), []byte(bakePlatformSHA+"\n"), 0o444))
	return dir
}

// bakeSourceBox puts the stopped instance of box1 into the fake Lima home with
// the disk the volume holds, symlinked the way the driver keeps it.
func bakeSourceBox(t *testing.T, f *fakelima.Fake, volumeDir, diskContent string) string {
	t.Helper()
	disk := filepath.Join(volumeDir, box.DiskFileName("box1"))
	require.NoError(t, os.MkdirAll(volumeDir, 0o755))
	require.NoError(t, os.WriteFile(disk, []byte(diskContent), 0o644))
	dir := f.InstanceDir("box1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Symlink(disk, f.DiskPath("box1")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte(`minimumLimaVersion: "2.0.0"
vmType: qemu
arch: x86_64
cpus: 2
memory: 1024MiB
disk: 8GiB
mounts:
  - location: /run/box-files/box1/secrets
    mountPoint: /run/box/secrets
    writable: false
`), 0o644))
	f.SetState(t, "box1", map[string]any{"status": "Stopped", "cpus": 2, "memory": int64(1024 * 1024 * 1024)})
	return disk
}

// bakeTaskConfig is a task that bakes box1 out of its volume.
func bakeTaskConfig(t *testing.T, volumeDir string) (*drivers.TaskConfig, TaskConfig) {
	t.Helper()
	tc := taskConfig(t, volumeDir, func(tc *drivers.TaskConfig) {
		tc.Name = "box"
	})
	cfg := TaskConfig{
		BoxID: "box1",
		Bake: &BakeConfig{
			Name:    "project",
			Version: "project-1",
			Parent:  "1.0.0",
			Arch:    "amd64",
			Recipe:  "IyEvYmluL2Jhc2gK", // "#!/bin/bash\n"
		},
		Platform: &PlatformConfig{
			Version: "1.10.0",
			URL:     "https://images.test/box-platform-1.10.0-linux-amd64.tar.gz",
			SHA256:  bakePlatformSHA,
		},
	}
	require.NoError(t, tc.EncodeConcreteDriverConfig(cfg))
	return tc, cfg
}

// waitExit waits for a task's exit result.
func waitExit(t *testing.T, d *Driver, taskID string, timeout time.Duration) *drivers.ExitResult {
	t.Helper()
	ch, err := d.WaitTask(context.Background(), taskID)
	require.NoError(t, err)
	select {
	case res := <-ch:
		return res
	case <-time.After(timeout):
		require.FailNow(t, "the task did not exit")
		return nil
	}
}

// bakeSetup builds the driver, a volume with box1's disk, the box's instance and
// the platform bundle.
func bakeSetup(t *testing.T, qemuImg, diskContent string) (*Driver, string, *fakelima.Fake, string) {
	t.Helper()
	f := fakelima.New(t)
	d, cache := driverWithQEMUImgAt(t, f, qemuImg)
	vol := t.TempDir()
	bakeSourceBox(t, f, vol, diskContent)
	bakePlatform(t, cache)
	return d, cache, f, vol
}

// TestBakeClonesSealsAndDeletesTheClone is the whole bake: the box is cloned, the
// clone is sealed from inside, its disk becomes the image, and the clone goes.
func TestBakeClonesSealsAndDeletesTheClone(t *testing.T) {
	diskBytes := "QFI\xfb\x00\x00\x00\x03 a qcow2 header and some clusters\n"
	d, cache, f, vol := bakeSetup(t, fakeQEMUImg(t, okQEMUImg), diskBytes)

	tc, _ := bakeTaskConfig(t, vol)
	handle, net, err := d.StartTask(tc)
	require.NoError(t, err)
	require.Nil(t, net)
	require.NotNil(t, handle)

	res := waitExit(t, d, "task1", 60*time.Second)
	require.True(t, res.Successful(), "the bake exited %+v", res)

	// The clone was made, from the box, with its own mounts, and it sealed
	// itself from the bundle's boxctl.
	require.True(t, f.HasCall(t, "clone --tty=false --mount-none box1 box1-bake-1"), f.Calls(t))
	seals := f.SealCalls(t)
	require.Len(t, seals, 1, "the seal ran once: %v", seals)
	require.True(t, strings.HasPrefix(seals[0], "box1-bake-1\t"), "the seal ran in the clone: %q", seals[0])

	// The image is the content of the clone's disk, which is the content of the
	// box's disk: a copy, and the box keeps it.
	sum := sha256.Sum256([]byte(diskBytes))
	digest := hex.EncodeToString(sum[:])
	data, err := os.ReadFile(filepath.Join(cache, digest+".img"))
	require.NoError(t, err, "the image is in the cache under its digest")
	require.Equal(t, diskBytes, string(data))
	boxData, err := os.ReadFile(filepath.Join(vol, box.DiskFileName("box1")))
	require.NoError(t, err)
	require.Equal(t, diskBytes, string(boxData), "the box's own disk is not touched")

	recordPath := filepath.Join(cache, "baked", "project-1.amd64.json")
	b, err := os.ReadFile(recordPath)
	require.NoError(t, err)
	var rec box.BakedRecord
	require.NoError(t, json.Unmarshal(b, &rec))
	require.Equal(t, "project-1", rec.Version)
	require.Equal(t, "project", rec.Name)
	require.Equal(t, "1.0.0", rec.Parent)
	require.Equal(t, digest, rec.SHA256)
	require.Equal(t, "IyEvYmluL2Jhc2gK", rec.Recipe)

	// And the clone is gone: no instance, no directory with a copy of the disk.
	require.NotContains(t, f.State(t), "box1-bake-1")
	_, err = os.Stat(f.InstanceDir("box1-bake-1"))
	require.True(t, os.IsNotExist(err), "the clone's directory is still there")

	status, err := d.InspectTask("task1")
	require.NoError(t, err)
	require.Equal(t, drivers.TaskStateExited, status.State)
	require.True(t, status.ExitResult.Successful())
}

// TestBakeRefusesARunningBox: a clone of a running box copies a disk the guest is
// writing, so the bake refuses it and names the stop.
func TestBakeRefusesARunningBox(t *testing.T) {
	d, _, f, vol := bakeSetup(t, fakeQEMUImg(t, okQEMUImg), "QFI\xfb\x00\x00\x00\x03 disk\n")
	f.SetState(t, "box1", map[string]any{"status": "Running", "driverPID": os.Getpid(),
		"hostAgentPID": os.Getpid()})

	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "box1 is running")
	require.Contains(t, res.Err.Error(), "box stop box1")
	require.False(t, f.HasCall(t, "clone"), "nothing is cloned from a running box")
}

// TestBakeRefusesACopyThatDoesNotFit: the clone is a full copy of the disk, so a
// node without the room fails the bake with a sentence before anything is cloned.
func TestBakeRefusesACopyThatDoesNotFit(t *testing.T) {
	d, _, f, vol := bakeSetup(t, fakeQEMUImg(t, okQEMUImg), strings.Repeat("QFI\xfb clusters\n", 1000))
	old := bakeFreeBytes
	bakeFreeBytes = func(string) (int64, error) { return 4096, nil }
	t.Cleanup(func() { bakeFreeBytes = old })

	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "free on the filesystem")
	require.Contains(t, res.Err.Error(), "needs")
	require.False(t, f.HasCall(t, "clone"), "nothing is cloned when the copy does not fit")
}

// TestBakeDeletesTheCloneWhenTheSealFails: a failed seal leaves the box alone and
// takes the clone with it, so no copy of the box stays on the node.
func TestBakeDeletesTheCloneWhenTheSealFails(t *testing.T) {
	d, _, f, vol := bakeSetup(t, fakeQEMUImg(t, okQEMUImg), "QFI\xfb\x00\x00\x00\x03 disk\n")
	f.SetMode(t, map[string]any{"fail_seal": "guest/no-secrets.sh failed in this box: FAIL /root/.prime"})

	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 60*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "the seal of box box1 failed in the clone")
	require.Contains(t, res.Err.Error(), "guest/no-secrets.sh failed in this box")
	require.NotContains(t, res.Err.Error(), "deleted the clone", "the failure is the seal's, not the cleanup's")
	require.NotContains(t, f.State(t), "box1-bake-1", "the clone is deleted")
	_, err = os.Stat(f.InstanceDir("box1-bake-1"))
	require.True(t, os.IsNotExist(err))
}

// TestBakeRemovesACloneLeftByAKilledBake: a clone a killed driver left behind is
// removed by the next bake of the same box, and its name is not reused.
func TestBakeRemovesACloneLeftByAKilledBake(t *testing.T) {
	d, _, f, vol := bakeSetup(t, fakeQEMUImg(t, okQEMUImg), "QFI\xfb\x00\x00\x00\x03 disk\n")
	// The leftover of a bake that was killed: an instance of its own.
	require.NoError(t, os.MkdirAll(f.InstanceDir("box1-bake-1"), 0o755))
	require.NoError(t, os.WriteFile(f.DiskPath("box1-bake-1"), []byte("STALE COPY"), 0o644))
	f.SetState(t, "box1-bake-1", map[string]any{"status": "Stopped"})

	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 60*time.Second)
	require.True(t, res.Successful(), "the bake exited %+v", res)
	require.True(t, f.HasCall(t, "clone --tty=false --mount-none box1 box1-bake-2"),
		"the new clone took a name of its own: %v", f.Calls(t))
	require.NotContains(t, f.State(t), "box1-bake-1", "the leftover was removed")
	require.NotContains(t, f.State(t), "box1-bake-2")
}

// TestBakeTaskWithoutQEMUImgSaysSo: a node without qemu-img cannot bake, and the
// task fails with a sentence that names the tool.
func TestBakeTaskWithoutQEMUImgSaysSo(t *testing.T) {
	d, _, _, vol := bakeSetup(t, filepath.Join(t.TempDir(), "absent", "qemu-img"), "QFI\xfb\x00\x00\x00\x03")

	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 60*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "qemu-img is not installed on this node")
	require.Contains(t, res.Err.Error(), "Install qemu-img")
}

// TestBakeRefusesAVersionThatEscapes: the version names a file below the image
// cache, so a value that could walk out of it is refused before anything runs.
func TestBakeRefusesAVersionThatEscapes(t *testing.T) {
	for _, bad := range []string{"../etc/passwd", "project/1", "Project1", ""} {
		cfg := TaskConfig{BoxID: "box1", Bake: &BakeConfig{Name: "project", Version: bad, Arch: "amd64"}}
		require.Error(t, cfg.applyDefaults(), "version %q was accepted", bad)
	}
	cfg := TaskConfig{BoxID: "box1", Bake: &BakeConfig{Name: "project", Version: "project-1", Arch: "riscv64"}}
	require.Error(t, cfg.applyDefaults())
	// A bake and a publish in one task is neither.
	both := TaskConfig{BoxID: "box1",
		Bake:    &BakeConfig{Name: "project", Version: "project-1", Arch: "amd64"},
		Publish: &PublishConfig{Version: "project-1", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
	require.Error(t, both.applyDefaults())
}

// TestStopBakeLeavesNoTemporaryFileAndNoClone: a bake that is stopped removes what
// it was writing and its clone, so the image cache never holds half an image and no
// copy of the box stays on the node.
func TestStopBakeLeavesNoTemporaryFileAndNoClone(t *testing.T) {
	// A conversion that never ends, so the stop has something to stop.
	slow := `#!/usr/bin/env bash
set -euo pipefail
args=("$@")
dst=${args[${#args[@]}-1]}
echo converting > "$dst"
sleep 300
`
	d, cache, f, vol := bakeSetup(t, fakeQEMUImg(t, slow), "QFI\xfb\x00\x00\x00\x03 disk\n")
	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	// Wait until the conversion is running, so the stop has a temporary file to
	// remove.
	require.Eventually(t, func() bool {
		entries, _ := os.ReadDir(cache)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".part") {
				return true
			}
		}
		return false
	}, 60*time.Second, 50*time.Millisecond, "the bake never started writing")

	require.NoError(t, d.StopTask("task1", 5*time.Second, ""))
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".part", "a stopped bake left %s in the image cache", e.Name())
	}
	require.NotContains(t, f.State(t), "box1-bake-1", "a stopped bake left its clone behind")
}

// TestFingerprintCarriesTheBakedImages: a draft exists on one node, so the node
// says which images it baked and what they are.
func TestFingerprintCarriesTheBakedImages(t *testing.T) {
	d, cache, _, vol := bakeSetup(t, fakeQEMUImg(t, okQEMUImg), "QFI\xfb\x00\x00\x00\x03 disk\n")
	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	require.True(t, waitExit(t, d, "task1", 60*time.Second).Successful())

	records, err := d.boxm.BakedRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)

	fp := d.buildFingerprint(context.Background())
	require.Equal(t, "amd64", attrValue(t, fp, "driver.box.baked.project-1.arch"))
	require.Equal(t, "project", attrValue(t, fp, "driver.box.baked.project-1.name"))
	require.Equal(t, records[0].SHA256, attrValue(t, fp, "driver.box.baked.project-1.sha256"))
	require.Equal(t, "1.0.0", attrValue(t, fp, "driver.box.baked.project-1.parent"))
	require.Equal(t, "mount", attrValue(t, fp, "driver.box.baked.project-1.platform"))
	require.NotEmpty(t, attrValue(t, fp, "driver.box.baked.project-1.built_at"))
	require.Equal(t, "IyEvYmluL2Jhc2gK", attrValue(t, fp, "driver.box.baked.project-1.recipe"))
	// The node also says how much room is left where a bake copies a disk.
	require.NotEmpty(t, attrValue(t, fp, "driver.box.disk.free_bytes"))

	// An image that is no longer in the cache is no longer an attribute: a draft
	// that was pruned must not be offered.
	require.NoError(t, os.Remove(filepath.Join(cache, "baked", "project-1.amd64.json")))
	fp = d.buildFingerprint(context.Background())
	value, ok := fp.Attributes["driver.box.baked.project-1.sha256"]
	require.True(t, ok, "the removed key must be sent empty, or Nomad keeps it for good")
	s, _ := value.GetString()
	require.Equal(t, "", s)
}

// fakeStore is the operator's HTTP PUT store.
type fakeStore struct {
	mu     sync.Mutex
	files  map[string][]byte
	status map[string]int
	// corruptArtifact makes a GET of the image answer different bytes, which is a
	// store that lost what was uploaded.
	corruptArtifact bool
	// authHeaders records the Authorization header of every request.
	authHeaders []string
	// headers records every request's headers, so a test can see which header
	// carried the credential.
	headers []http.Header
	// requests records "METHOD /path" in the order the store saw them.
	requests []string
	// putStatus refuses a PUT with this status.
	putStatus int
	// refuseDeclaredSize answers a PUT of a qcow2 that carries a Content-Length
	// with HTTP 413, the way a proxy in front of a store applies a body-size
	// limit: the publisher has to send the same bytes in chunked form.
	refuseDeclaredSize bool
	// closeOnDeclaredRefusal closes the connection right after the 413, which is
	// what a proxy does: the body is still being written, so the client sees a
	// write error and never the status.
	closeOnDeclaredRefusal bool
	// declaredRefusals counts the PUTs refused for their declared size, and
	// chunkedUploads the PUTs that arrived without a size.
	declaredRefusals int
	chunkedUploads   int
	// requiredHeader and requiredValue are a credential the store insists on: a
	// request that does not carry it is refused with HTTP 401.
	requiredHeader string
	requiredValue  string
}

func newFakeStore() *fakeStore {
	return &fakeStore{files: map[string][]byte{}, status: map[string]int{}}
}

func (s *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := r.Header.Get("Authorization"); h != "" {
		s.authHeaders = append(s.authHeaders, h)
	}
	s.headers = append(s.headers, r.Header.Clone())
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	if s.requiredHeader != "" && r.Header.Get(s.requiredHeader) != s.requiredValue {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	key := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		if s.putStatus != 0 {
			w.WriteHeader(s.putStatus)
			return
		}
		if strings.HasSuffix(key, ".qcow2") {
			if r.ContentLength < 0 {
				s.chunkedUploads++
			} else if s.refuseDeclaredSize {
				s.declaredRefusals++
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				if s.closeOnDeclaredRefusal {
					if hj, ok := w.(http.Hijacker); ok {
						if conn, _, err := hj.Hijack(); err == nil {
							_ = conn.Close()
						}
					}
				}
				return
			}
		}
		body := make([]byte, 0, 1024)
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		s.files[key] = body
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead, http.MethodGet:
		data, ok := s.files[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if s.corruptArtifact && strings.HasSuffix(key, ".qcow2") {
			data = []byte("something else entirely")
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// storeTaskConfig is a publish task for one baked image, with the store in the
// task's secrets the way the templates of a real job render it.
func storeTaskConfig(t *testing.T, version, arch, sha, url, token string) (*drivers.TaskConfig, TaskConfig) {
	t.Helper()
	tc := taskConfig(t, t.TempDir(), func(tc *drivers.TaskConfig) { tc.Name = "publish" })
	secrets := tc.TaskDir().SecretsDir
	require.NoError(t, os.MkdirAll(filepath.Join(secrets, storeDirInSecrets), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(secrets, storeDirInSecrets, "url"), []byte(url+"\n"), 0o600))
	if token != "" {
		require.NoError(t, os.WriteFile(filepath.Join(secrets, storeDirInSecrets, "token"), []byte(token+"\n"), 0o600))
	}
	cfg := TaskConfig{
		BoxID:   "project",
		Publish: &PublishConfig{Version: version, Arch: arch, SHA256: sha},
	}
	require.NoError(t, tc.EncodeConcreteDriverConfig(cfg))
	return tc, cfg
}

// putBakedImage writes a record and its image into the cache, as a bake would.
func putBakedImage(t *testing.T, cache string, rec box.BakedRecord, data []byte) box.BakedRecord {
	t.Helper()
	sum := sha256.Sum256(data)
	rec.SHA256 = hex.EncodeToString(sum[:])
	rec.Size = int64(len(data))
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "baked"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cache, rec.SHA256+".img"), data, 0o644))
	b, err := json.MarshalIndent(rec, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cache, "baked", rec.Version+"."+rec.Arch+".json"), append(b, '\n'), 0o644))
	return rec
}

// TestPublishTaskUploadsAndIndexes: the image arrives in the store under the paths
// images/publish.sh writes, and the index names it, with the identity of a project
// image.
func TestPublishTaskUploadsAndIndexes(t *testing.T) {
	store := newFakeStore()
	srv := httptest.NewServer(store)
	defer srv.Close()

	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	rec := putBakedImage(t, cache, box.BakedRecord{
		Version: "project-1", Arch: "amd64", Name: "project", Parent: "1.0.0",
		BuiltAt: "2026-11-02T12:00:00Z", VirtualBytes: 8 << 30, Platform: "mount",
		Recipe: "IyEvYmluL2Jhc2gK",
	}, []byte("QFI\xfb the image\n"))

	tc, _ := storeTaskConfig(t, "project-1", "amd64", rec.SHA256, srv.URL, "s3cret-token")
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.True(t, res.Successful(), "the publish exited %+v", res)

	base := "/box-base/project-1/amd64"
	artifact := store.files[base+"/box-base-project-1-amd64.qcow2"]
	require.Equal(t, []byte("QFI\xfb the image\n"), artifact)
	require.Contains(t, string(store.files[base+"/box-base-project-1-amd64.qcow2.sha256"]), rec.SHA256)
	require.Contains(t, string(store.files[base+"/box-base-project-1-amd64.json"]), `"version": "project-1"`)

	var index map[string]any
	require.NoError(t, json.Unmarshal(store.files["/box-base/index.json"], &index))
	images, _ := index["images"].(map[string]any)
	arches, _ := images["project-1"].(map[string]any)
	entry, _ := arches["amd64"].(map[string]any)
	require.NotNil(t, entry, "the index names the image: %s", store.files["/box-base/index.json"])
	require.Equal(t, "project", entry["kind"])
	require.Equal(t, "project", entry["name"])
	require.Equal(t, "1.0.0", entry["parent"])
	require.Equal(t, "mount", entry["platform"])
	require.Equal(t, rec.SHA256, entry["sha256"])
	require.Equal(t, "IyEvYmluL2Jhc2gK", entry["recipe"])
	require.Equal(t, float64(8<<30), entry["virtual_bytes"])
	require.Equal(t, srv.URL+base+"/box-base-project-1-amd64.qcow2", entry["url"])
	require.True(t, index["schema"] != nil)

	// The token went in the Authorization header of the requests, and nowhere else.
	require.NotEmpty(t, store.authHeaders)
	require.Equal(t, "Bearer s3cret-token", store.authHeaders[0])
}

// TestPublishRefusesAPublishedVersion: a published version never changes.
func TestPublishRefusesAPublishedVersion(t *testing.T) {
	store := newFakeStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	rec := putBakedImage(t, cache, box.BakedRecord{Version: "project-1", Arch: "amd64", Name: "project", Platform: "mount"},
		[]byte("QFI\xfb the image\n"))
	store.files["/box-base/project-1/amd64/box-base-project-1-amd64.qcow2"] = []byte("already there")

	tc, _ := storeTaskConfig(t, "project-1", "amd64", rec.SHA256, srv.URL, "")
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "already exists")
}

// TestPublishFailedReadBackLeavesTheIndex is the proof the index never names an
// image that did not arrive.
func TestPublishFailedReadBackLeavesTheIndex(t *testing.T) {
	store := newFakeStore()
	store.corruptArtifact = true
	srv := httptest.NewServer(store)
	defer srv.Close()
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	rec := putBakedImage(t, cache, box.BakedRecord{Version: "project-1", Arch: "amd64", Name: "project", Platform: "mount"},
		[]byte("QFI\xfb the image\n"))

	tc, _ := storeTaskConfig(t, "project-1", "amd64", rec.SHA256, srv.URL, "")
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "sha256")
	require.Contains(t, res.Err.Error(), "index.json was not touched")

	_, wrote := store.files["/box-base/index.json"]
	require.False(t, wrote, "the index was written although the image did not arrive")
}

// TestPublishWithoutAStoreSaysSo: the store is a Variable the operator writes, so
// a node without one fails with the command that sets it.
func TestPublishWithoutAStoreSaysSo(t *testing.T) {
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	rec := putBakedImage(t, cache, box.BakedRecord{Version: "project-1", Arch: "amd64", Name: "project", Platform: "mount"},
		[]byte("QFI\xfb the image\n"))
	tc := taskConfig(t, t.TempDir(), func(tc *drivers.TaskConfig) { tc.Name = "publish" })
	require.NoError(t, tc.EncodeConcreteDriverConfig(TaskConfig{
		BoxID:   "project",
		Publish: &PublishConfig{Version: "project-1", Arch: "amd64", SHA256: rec.SHA256},
	}))
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "no image store configured")
	require.Contains(t, res.Err.Error(), "box store set")
}

// TestPublishNeedsTheBakedImage: publishing what this node never baked is refused
// with the node that has to do it.
func TestPublishNeedsTheBakedImage(t *testing.T) {
	store := newFakeStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	d, _ := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	tc, _ := storeTaskConfig(t, "project-1", "amd64", strings.Repeat("a", 64), srv.URL, "")
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "has no image project-1 amd64")
}
