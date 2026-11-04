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
	f := fakelima.New(t)
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

// bakeTaskConfig is a task that bakes the disk of box1 out of its volume.
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

// TestBakeTaskWritesTheImage is the bake of a sealed box: the disk becomes a file
// in the image cache named by its digest, with a record beside it.
func TestBakeTaskWritesTheImage(t *testing.T) {
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	vol := t.TempDir()
	disk := filepath.Join(vol, box.DiskFileName("box1"))
	diskBytes := []byte("QFI\xfb\x00\x00\x00\x03 a qcow2 header and some clusters\n")
	require.NoError(t, os.WriteFile(disk, diskBytes, 0o644))

	tc, _ := bakeTaskConfig(t, vol)
	handle, net, err := d.StartTask(tc)
	require.NoError(t, err)
	require.Nil(t, net)
	require.NotNil(t, handle)

	res := waitExit(t, d, "task1", 30*time.Second)
	require.True(t, res.Successful(), "the bake exited %+v", res)

	sum := sha256.Sum256(diskBytes)
	digest := hex.EncodeToString(sum[:])
	image := filepath.Join(cache, digest+".img")
	data, err := os.ReadFile(image)
	require.NoError(t, err, "the image is in the cache under its digest, which is where EnsureImage looks")
	require.Equal(t, diskBytes, data)

	recordPath := filepath.Join(cache, "baked", "project-1.amd64.json")
	b, err := os.ReadFile(recordPath)
	require.NoError(t, err)
	var rec box.BakedRecord
	require.NoError(t, json.Unmarshal(b, &rec))
	require.Equal(t, "project-1", rec.Version)
	require.Equal(t, "amd64", rec.Arch)
	require.Equal(t, "project", rec.Name)
	require.Equal(t, "1.0.0", rec.Parent)
	require.Equal(t, digest, rec.SHA256)
	require.Equal(t, int64(len(diskBytes)), rec.Size)
	require.Equal(t, "mount", rec.Platform)
	require.NotEmpty(t, rec.BuiltAt)
	require.Equal(t, "IyEvYmluL2Jhc2gK", rec.Recipe)

	// The task reports what it baked, which is what `nomad alloc status` shows
	// and what `box bake` reads.
	status, err := d.InspectTask("task1")
	require.NoError(t, err)
	require.Equal(t, drivers.TaskStateExited, status.State)
	require.True(t, status.ExitResult.Successful())
}

// TestBakeTaskWithoutQEMUImgSaysSo: a node without qemu-img cannot bake, and the
// task fails with a sentence that names the tool.
func TestBakeTaskWithoutQEMUImg(t *testing.T) {
	d, _ := driverWithQEMUImg(t, filepath.Join(t.TempDir(), "absent", "qemu-img"))
	vol := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(vol, box.DiskFileName("box1")), []byte("QFI\xfb\x00\x00\x00\x03"), 0o644))

	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
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

// TestStopBakeLeavesNoTemporaryFile: a bake that is stopped removes what it was
// writing, so the image cache never holds half an image.
func TestStopBakeLeavesNoTemporaryFile(t *testing.T) {
	// A conversion that never ends, so the stop has something to stop.
	slow := `#!/usr/bin/env bash
set -euo pipefail
args=("$@")
dst=${args[${#args[@]}-1]}
echo converting > "$dst"
sleep 300
`
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, slow))
	vol := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(vol, box.DiskFileName("box1")), []byte("QFI\xfb\x00\x00\x00\x03"), 0o644))

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
	}, 20*time.Second, 50*time.Millisecond, "the bake never started writing")

	require.NoError(t, d.StopTask("task1", 5*time.Second, ""))
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".part", "a stopped bake left %s in the image cache", e.Name())
	}
}

// TestFingerprintCarriesTheBakedImages: a draft exists on one node, so the node
// says which images it baked and what they are.
func TestFingerprintCarriesTheBakedImages(t *testing.T) {
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	vol := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(vol, box.DiskFileName("box1")), []byte("QFI\xfb\x00\x00\x00\x03 disk\n"), 0o644))
	tc, _ := bakeTaskConfig(t, vol)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	require.True(t, waitExit(t, d, "task1", 30*time.Second).Successful())

	records, err := d.boxm.BakedRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)

	fp := d.buildFingerprint(context.Background())
	require.Equal(t, "amd64", attrValue(t, fp, "driver.box.baked.project-1.arch"))
	require.Equal(t, "project", attrValue(t, fp, "driver.box.baked.project-1.name"))
	require.Equal(t, records[0].SHA256, attrValue(t, fp, "driver.box.baked.project-1.sha256"))
	require.Equal(t, "1.0.0", attrValue(t, fp, "driver.box.baked.project-1.parent"))
	require.Equal(t, "mount", attrValue(t, fp, "driver.box.baked.project-1.platform"))
	require.Equal(t, fmt.Sprintf("%d", records[0].Size), attrValue(t, fp, "driver.box.baked.project-1.size"))
	require.NotEmpty(t, attrValue(t, fp, "driver.box.baked.project-1.built_at"))
	require.Equal(t, "IyEvYmluL2Jhc2gK", attrValue(t, fp, "driver.box.baked.project-1.recipe"))

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
	// putStatus refuses a PUT with this status.
	putStatus int
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
	key := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		if s.putStatus != 0 {
			w.WriteHeader(s.putStatus)
			return
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
