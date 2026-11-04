package driver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// platformBundle builds a platform bundle archive and serves it, returning its
// URL and sha256. The bundle holds a manifest, boxd and one tool; no name here
// comes from a harness, because the repository is harness-agnostic.
func platformBundle(t *testing.T, version string) (string, string) {
	t.Helper()
	manifest, err := json.Marshal(map[string]any{
		"schema":  1,
		"version": version,
		"arch":    "amd64",
		"boxd":    "0.4.0",
		"tools":   map[string]string{"toolb": "2.1.0"},
	})
	require.NoError(t, err)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name, body, link string
		mode             int64
		typeflag         byte
	}{
		{name: "manifest.json", body: string(manifest), mode: 0o644},
		{name: "bin/boxd", body: "#!/bin/sh\necho boxd\n", mode: 0o755},
		{name: "bin/boxctl", link: "boxd", mode: 0o777, typeflag: tar.TypeSymlink},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typeflag, Linkname: e.link}
		if e.typeflag == 0 {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Size > 0 {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/box-platform-" + version + "-linux-amd64.tar.gz", hex.EncodeToString(sum[:])
}

// TestTaskConfigPlatformBlock: the platform block is optional and validated
// like the image block, and the task schema cannot reach node settings.
func TestTaskConfigPlatformBlock(t *testing.T) {
	base := TaskConfig{BoxID: "box1", Image: ImageConfig{URL: "http://x/y", SHA256: strings.Repeat("a", 64)}}

	// Without a block nothing changes.
	plain := base
	require.NoError(t, plain.applyDefaults())
	require.Nil(t, plain.Platform)

	// With one, all three fields are usable.
	full := base
	digest := strings.Repeat("B", 64)
	full.Platform = &PlatformConfig{Version: "1.0.0", URL: "http://x/b.tar.gz", SHA256: "sha256:" + digest}
	require.NoError(t, full.applyDefaults())
	require.Equal(t, "1.0.0", full.Platform.Version)

	cases := map[string]*PlatformConfig{
		"no sha256":  {Version: "1.0.0", URL: "http://x/b.tar.gz"},
		"no url":     {Version: "1.0.0", SHA256: digest},
		"short sha":  {Version: "1.0.0", URL: "http://x/b.tar.gz", SHA256: "abc"},
		"not hex":    {Version: "1.0.0", URL: "http://x/b.tar.gz", SHA256: strings.Repeat("z", 64)},
		"only a tag": {Version: "1.0.0"},
	}
	for name, platform := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Platform = platform
			require.Error(t, cfg.applyDefaults())
		})
	}

	// The schema carries the block, marks it optional and requires url+sha256.
	obj, ok := taskConfigSpec.Block.(*hclspec.Spec_Object)
	require.True(t, ok)
	block, ok := obj.Object.Attributes["platform"]
	require.True(t, ok, "the task config has no platform block")
	blockValue, ok := block.Block.(*hclspec.Spec_BlockValue)
	require.True(t, ok, "platform is a block, not an attribute")
	require.False(t, blockValue.BlockValue.Required, "the platform block is optional")
	require.Equal(t, "platform", blockValue.BlockValue.Name)
	inner, ok := blockValue.BlockValue.Nested.Block.(*hclspec.Spec_Object)
	require.True(t, ok)
	attrRequired := func(key string) bool {
		spec, ok := inner.Object.Attributes[key]
		require.True(t, ok, "the platform block has no %q", key)
		attr, ok := spec.Block.(*hclspec.Spec_Attr)
		require.True(t, ok, "%q is an attribute", key)
		return attr.Attr.Required
	}
	require.False(t, attrRequired("version"))
	require.True(t, attrRequired("url"))
	require.True(t, attrRequired("sha256"))

	// And it cannot name a node setting.
	for _, forbidden := range []string{"limactl", "lima_home", "image_cache_dir", "runtime_user"} {
		require.NotContains(t, inner.Object.Attributes, forbidden)
	}
}

// TestStartTaskMountsPlatformBundle drives a start through the driver: the
// bundle is downloaded into the image cache, mounted read-only at /opt/box for
// the VM, and reported as part of the task.
func TestStartTaskMountsPlatformBundle(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	imageURL := fakelima.ImageServer(t, "IMAGE")
	bundleURL, bundleSHA := platformBundle(t, "1.0.0")

	cfg := startConfig(t, imageURL, sha256Hex("IMAGE"))
	cfg.Platform = &PlatformConfig{Version: "1.0.0", URL: bundleURL, SHA256: bundleSHA}
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(cfg))

	handle, _, err := d.StartTask(tc)
	require.NoError(t, err)
	require.Equal(t, drivers.TaskStateRunning, handle.State)
	require.Equal(t, "Running", f.State(t)["box1"]["status"])

	// The bundle is unpacked in the cache directory the plugin block names.
	dir := filepath.Join(d.config.ImageCacheDir, "platform", bundleSHA)
	require.FileExists(t, filepath.Join(dir, "manifest.json"))
	require.Equal(t, "boxd", symlinkOf(t, filepath.Join(dir, "bin", "boxctl")))

	// The VM mounts it read-only at /opt/box, with a 9p cache on qemu.
	data, err := os.ReadFile(filepath.Join(f.InstanceDir("box1"), "lima.yaml"))
	require.NoError(t, err)
	var doc struct {
		MountType string `yaml:"mountType"`
		Mounts    []struct {
			Location   string         `yaml:"location"`
			MountPoint string         `yaml:"mountPoint"`
			Writable   bool           `yaml:"writable"`
			NineP      map[string]any `yaml:"9p"`
		} `yaml:"mounts"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	require.Equal(t, "9p", doc.MountType)
	found := false
	for _, mnt := range doc.Mounts {
		if mnt.MountPoint != "/opt/box" {
			continue
		}
		found = true
		require.Equal(t, dir, mnt.Location)
		require.False(t, mnt.Writable)
		require.Equal(t, "fscache", mnt.NineP["cache"])
	}
	require.True(t, found, "no /opt/box mount in %s", data)

	// And the task says which bundle it mounts.
	status, err := d.InspectTask("task1")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", status.DriverAttributes["platform"])

	// The handle carries the block, so a client restart recovers it.
	st, err := decodeDriverState(handle.DriverState)
	require.NoError(t, err)
	require.NotNil(t, st.Config.Platform)
	require.Equal(t, bundleSHA, st.Config.Platform.SHA256)
	require.Equal(t, "1.0.0", platformName(st.Config.Platform))
}

// TestStartTaskWithoutPlatformKeepsTheImageOwnBoxd: a job without the block
// mounts nothing and reads no bundle.
func TestStartTaskWithoutPlatformKeepsTheImageOwnBoxd(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	imageURL := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, imageURL, sha256Hex("IMAGE"))))

	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(f.InstanceDir("box1"), "lima.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(data), "/opt/box")
	require.NoDirExists(t, filepath.Join(d.config.ImageCacheDir, "platform"))

	status, err := d.InspectTask("task1")
	require.NoError(t, err)
	require.NotContains(t, status.DriverAttributes, "platform")
}

// TestStartTaskRefusesBadPlatformDigest fails the task before any VM work.
func TestStartTaskRefusesBadPlatformDigest(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	imageURL := fakelima.ImageServer(t, "IMAGE")
	cfg := startConfig(t, imageURL, sha256Hex("IMAGE"))
	cfg.Platform = &PlatformConfig{Version: "1.0.0", URL: "http://127.0.0.1:1/b.tar.gz", SHA256: "nope"}
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(cfg))

	_, _, err := d.StartTask(tc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "platform.sha256")
	require.False(t, f.HasCall(t, "create"))
}

// TestRecoverTaskKeepsPlatformBlock re-attaches to a box that mounts a bundle.
func TestRecoverTaskKeepsPlatformBlock(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	imageURL := fakelima.ImageServer(t, "IMAGE")
	bundleURL, bundleSHA := platformBundle(t, "1.0.0")
	cfg := startConfig(t, imageURL, sha256Hex("IMAGE"))
	cfg.Platform = &PlatformConfig{Version: "1.0.0", URL: bundleURL, SHA256: bundleSHA}
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(cfg))
	handle, _, err := d.StartTask(tc)
	require.NoError(t, err)

	// A new plugin process re-attaches to the same VM.
	d2 := newTestDriver(t, f)
	require.NoError(t, d2.RecoverTask(handle))
	status, err := d2.InspectTask("task1")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", status.DriverAttributes["platform"])
	require.NoError(t, d2.DestroyTask("task1", true))
}

// symlinkOf returns the target of a symlink.
func symlinkOf(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	require.NoError(t, err)
	return target
}
