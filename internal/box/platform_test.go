package box

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"go.yaml.in/yaml/v3"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// bundleEntry is one entry of a test bundle: a file, a directory or a symlink.
type bundleEntry struct {
	name string
	body string
	link string
	mode int64
	// typeflag is 0 for "a regular file", tar.TypeDir and tar.TypeSymlink for the
	// others; a test that checks the refusals sets it to anything the contract
	// does not allow (tar.TypeChar, a ".." name, an absolute link).
	typeflag byte
}

// bundleFiles is the content of a well-formed bundle: a manifest, boxd, the
// boxctl symlink and one tool with its own directory next to a symlink into it.
func bundleFiles(version string) []bundleEntry {
	manifest, _ := json.Marshal(map[string]any{
		"schema":   1,
		"version":  version,
		"arch":     "amd64",
		"boxd":     "0.4.0",
		"tools":    map[string]string{"toolb": "2.1.0"},
		"built_at": "2026-11-02T09:00:00Z",
	})
	return []bundleEntry{
		{name: "manifest.json", body: string(manifest), mode: 0o644},
		{name: "bin", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/boxd", body: "#!/bin/sh\necho boxd " + version + "\n", mode: 0o755},
		{name: "bin/boxctl", link: "boxd", mode: 0o777},
		{name: "bin/toolb", link: "../tools/toolb/toolb", mode: 0o777},
		{name: "tools/toolb", typeflag: tar.TypeDir, mode: 0o755},
		{name: "tools/toolb/toolb", body: "#!/bin/sh\necho toolb\n", mode: 0o755},
	}
}

// writeBundle writes a gzip'd tar of the entries into dir and returns its path
// and sha256.
func writeBundle(t *testing.T, dir string, entries []bundleEntry) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "bundle.tar.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 && e.link != "" {
			flag = tar.TypeSymlink
		}
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: flag, Linkname: e.link}
		switch flag {
		case tar.TypeDir:
			hdr.Mode = 0o755
		case tar.TypeSymlink, tar.TypeChar, tar.TypeLink:
		default:
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
	require.NoError(t, f.Close())
	sum, err := fileSHA(path)
	require.NoError(t, err)
	return path, sum
}

// fileSHA hashes a file.
func fileSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// bundleServer serves one bundle and counts the requests for it.
func bundleServer(t *testing.T, path string, hits *int64) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/box-platform-1.0.0-linux-amd64.tar.gz"
}

// testPlatform writes a bundle of version into a temporary directory and serves
// the archive; it returns the spec and the archive path.
func testPlatform(t *testing.T, version string, hits *int64) (PlatformSpec, string) {
	t.Helper()
	path, sum := writeBundle(t, t.TempDir(), bundleFiles(version))
	url := bundleServer(t, path, hits)
	return PlatformSpec{Version: version, URL: url, SHA256: sum}, path
}

// TestEnsurePlatformUnpacksAndReusesBundle covers the happy path: a verified
// download, a complete directory in the cache, and a second start that reuses it
// without asking the store again.
func TestEnsurePlatformUnpacksAndReusesBundle(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	var hits int64
	spec, _ := testPlatform(t, "1.0.0", &hits)
	rep, events := collectEvents()
	b, err := m.Box(testSpec(t, volumeDir(t)), rep)
	require.NoError(t, err)

	dir, err := b.EnsurePlatform(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.Options().ImageCacheDir, "platform", spec.SHA256), dir)
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))

	// What the guest sees at /opt/box.
	manifest, err := ReadPlatformManifest(dir)
	require.NoError(t, err)
	require.Equal(t, "1.0.0", manifest.Version)
	require.Equal(t, "0.4.0", manifest.Boxd)
	require.Equal(t, map[string]string{"toolb": "2.1.0"}, manifest.Tools)
	require.Equal(t, []string{"toolb 2.1.0"}, manifest.PlatformTools())
	require.Equal(t, "#!/bin/sh\necho boxd 1.0.0\n", readFile(t, filepath.Join(dir, "bin", "boxd")))
	require.Equal(t, "boxd", symlinkTarget(t, filepath.Join(dir, "bin", "boxctl")))
	require.Equal(t, "../tools/toolb/toolb", symlinkTarget(t, filepath.Join(dir, "bin", "toolb")))
	// The marker is what proves the unpack finished; it carries the digest.
	require.Equal(t, spec.SHA256+"\n", readFile(t, filepath.Join(dir, platformMarker)))
	st, err := os.Stat(filepath.Join(dir, "bin", "boxd"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), st.Mode().Perm())
	// The bundle is a system directory of the guest: readable, not private.
	root, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), root.Mode().Perm())

	// A second start reuses the directory: no second download.
	dir2, err := b.EnsurePlatform(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, dir, dir2)
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))
	require.Contains(t, strings.Join(*events, "\n"), "Using cached platform bundle 1.0.0")
	require.Contains(t, strings.Join(*events, "\n"), "Unpacked platform bundle 1.0.0")
}

// TestEnsurePlatformVerifiesSHA starts no unpack for an archive that does not
// match the digest the task pinned.
func TestEnsurePlatformVerifiesSHA(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	spec, _ := testPlatform(t, "1.0.0", nil)
	spec.SHA256 = strings.Repeat("a", 64)
	rep, events := collectEvents()
	b, err := m.Box(testSpec(t, volumeDir(t)), rep)
	require.NoError(t, err)

	_, err = b.EnsurePlatform(context.Background(), spec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has sha256")
	require.Contains(t, err.Error(), "want "+strings.Repeat("a", 64))
	require.NoDirExists(t, m.PlatformDir(spec.SHA256))
	require.Contains(t, strings.Join(*events, "\n"), "Downloading platform bundle 1.0.0")
}

// TestEnsurePlatformRefusesBadSHAAndURL rejects a task that names an unusable
// digest before anything touches the network.
func TestEnsurePlatformRefusesBadSHAAndURL(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	b, err := m.Box(testSpec(t, volumeDir(t)), nil)
	require.NoError(t, err)

	_, err = b.EnsurePlatform(context.Background(), PlatformSpec{Version: "1.0.0", URL: "http://127.0.0.1:1/x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "sha256 must be 64 hex characters")

	_, err = b.EnsurePlatform(context.Background(), PlatformSpec{Version: "1.0.0", SHA256: strings.Repeat("b", 64)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no url")

	// Spec.Validate refuses the same things for the driver.
	spec := testSpec(t, volumeDir(t))
	spec.Platform = &PlatformSpec{URL: "http://127.0.0.1:1/x", SHA256: "not-a-digest"}
	_, err = m.Box(spec, nil)
	require.ErrorContains(t, err, "platform.sha256")
	spec.Platform = &PlatformSpec{SHA256: strings.Repeat("c", 64)}
	_, err = m.Box(spec, nil)
	require.ErrorContains(t, err, "platform.url is required")
}

// TestEnsurePlatformRefusesHostileArchives covers every entry the contract does
// not allow: a path that escapes the bundle, an absolute path, an absolute
// symlink, a symlink pointing outside and a device.
func TestEnsurePlatformRefusesHostileArchives(t *testing.T) {
	cases := map[string]bundleEntry{
		"parent path":          {name: "../evil", body: "x", mode: 0o644},
		"deep parent path":     {name: "bin/../../evil", body: "x", mode: 0o644},
		"absolute path":        {name: "/etc/evil", body: "x", mode: 0o644},
		"absolute symlink":     {name: "bin/abs", link: "/etc/passwd", mode: 0o777},
		"symlink outside":      {name: "bin/out", link: "../../outside", mode: 0o777},
		"device":               {name: "bin/null", typeflag: tar.TypeChar, mode: 0o666},
		"hard link outside":    {name: "bin/hard", link: "../outside", typeflag: tar.TypeLink, mode: 0o644},
		"symlink up one level": {name: "bin/up", link: "../..", mode: 0o777},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			f := fakelima.New(t)
			m := newTestManager(t, f)
			entries := append(bundleFiles("1.0.0"), entry)
			path, sum := writeBundle(t, t.TempDir(), entries)
			spec := PlatformSpec{Version: "1.0.0", URL: bundleServer(t, path, nil), SHA256: sum}
			b, err := m.Box(testSpec(t, volumeDir(t)), nil)
			require.NoError(t, err)

			_, err = b.EnsurePlatform(context.Background(), spec)
			require.Error(t, err)
			require.NoDirExists(t, m.PlatformDir(sum))
			// Nothing was left behind in the cache directory either.
			left, err := os.ReadDir(filepath.Join(m.Options().ImageCacheDir, "platform"))
			require.NoError(t, err)
			require.Empty(t, left)
		})
	}
}

// TestEnsurePlatformMakesDirectoriesReadable: an archive that names a file
// without listing its directory entries still gives the guest a readable tree,
// whatever the umask of the process that unpacked it.
func TestEnsurePlatformMakesDirectoriesReadable(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	entries := []bundleEntry{
		{name: "manifest.json", body: `{"schema":1,"version":"1.0.0"}`, mode: 0o600},
		{name: "tools/toolb/toolb", body: "#!/bin/sh\n", mode: 0o755},
	}
	path, sum := writeBundle(t, t.TempDir(), entries)
	spec := PlatformSpec{Version: "1.0.0", URL: bundleServer(t, path, nil), SHA256: sum}
	b, err := m.Box(testSpec(t, volumeDir(t)), nil)
	require.NoError(t, err)

	dir, err := b.EnsurePlatform(context.Background(), spec)
	require.NoError(t, err)
	for _, d := range []string{dir, filepath.Join(dir, "tools"), filepath.Join(dir, "tools", "toolb")} {
		st, err := os.Stat(d)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), st.Mode().Perm(), d)
	}
	// Files keep the mode the archive gives them.
	st, err := os.Stat(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// TestEnsurePlatformRejectsArchiveWithoutManifest keeps a bundle that cannot be
// read out of the cache.
func TestEnsurePlatformRejectsArchiveWithoutManifest(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	path, sum := writeBundle(t, t.TempDir(), []bundleEntry{{name: "bin/boxd", body: "x", mode: 0o755}})
	spec := PlatformSpec{Version: "1.0.0", URL: bundleServer(t, path, nil), SHA256: sum}
	b, err := m.Box(testSpec(t, volumeDir(t)), nil)
	require.NoError(t, err)

	_, err = b.EnsurePlatform(context.Background(), spec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest.json")
	require.NoDirExists(t, m.PlatformDir(sum))
}

// TestEnsurePlatformReunpacksWithoutMarker: a directory whose unpack never
// finished (no marker) is not mounted; it is unpacked again.
func TestEnsurePlatformReunpacksWithoutMarker(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	spec, _ := testPlatform(t, "1.0.0", nil)
	b, err := m.Box(testSpec(t, volumeDir(t)), nil)
	require.NoError(t, err)
	dir, err := b.EnsurePlatform(context.Background(), spec)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, platformMarker)))
	// Half an unpack: the binary is gone too.
	require.NoError(t, os.Remove(filepath.Join(dir, "bin", "boxd")))

	again, err := b.EnsurePlatform(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, dir, again)
	require.FileExists(t, filepath.Join(dir, "bin", "boxd"))
	require.FileExists(t, filepath.Join(dir, platformMarker))
}

// TestRenderLimaYAMLPlatformMount: the bundle is one more read-only mount, with
// a 9p cache on qemu (the directory never changes) and virtiofs on vz, while the
// task files keep their uncached 9p view.
func TestRenderLimaYAMLPlatformMount(t *testing.T) {
	spec := testSpec(t, volumeDir(t))
	spec.Platform = &PlatformSpec{Version: "1.0.0", URL: "http://example.com/b.tar.gz", SHA256: strings.Repeat("d", 64)}
	m, err := New(Options{LimaHome: t.TempDir(), ImageCacheDir: t.TempDir(), VMType: VMTypeQEMU})
	require.NoError(t, err)
	dir := m.PlatformDir(spec.Platform.SHA256)

	content, err := renderLimaYAML(spec, "/images/base.img", m.guestMounts(spec, dir))
	require.NoError(t, err)
	var doc limaYAML
	require.NoError(t, yaml.Unmarshal(content, &doc))
	require.Equal(t, "9p", doc.MountType)
	byPoint := map[string]limaMount{}
	for _, mnt := range doc.Mounts {
		byPoint[mnt.MountPoint] = mnt
	}
	require.Len(t, doc.Mounts, 3)
	platform, ok := byPoint[PlatformMountPoint]
	require.True(t, ok, "no mount at %s in %s", PlatformMountPoint, content)
	require.Equal(t, dir, platform.Location)
	require.False(t, platform.Writable)
	require.NotNil(t, platform.NineP)
	require.Equal(t, CacheFscache, platform.NineP.Cache)
	require.Equal(t, CacheNone, byPoint["/etc/box/local"].NineP.Cache)
	require.Equal(t, CacheNone, byPoint[DefaultSecretsDirInGuest].NineP.Cache)

	// vz shares it over virtiofs, which has no 9p options.
	spec.VMType = VMTypeVZ
	content, err = renderLimaYAML(spec, "/images/base.img", m.guestMounts(spec, dir))
	require.NoError(t, err)
	var vzDoc limaYAML
	require.NoError(t, yaml.Unmarshal(content, &vzDoc))
	require.Equal(t, "virtiofs", vzDoc.MountType)
	for _, mnt := range vzDoc.Mounts {
		require.Nil(t, mnt.NineP)
		if mnt.MountPoint == PlatformMountPoint {
			require.Equal(t, dir, mnt.Location)
			require.False(t, mnt.Writable)
		}
	}

	// Without a platform block the box mounts what it always mounted.
	plain := testSpec(t, volumeDir(t))
	content, err = renderLimaYAML(plain, "/images/base.img", m.guestMounts(plain, ""))
	require.NoError(t, err)
	var plainDoc limaYAML
	require.NoError(t, yaml.Unmarshal(content, &plainDoc))
	require.Len(t, plainDoc.Mounts, 2)
}

// TestEnsureMountsPlatformBundle drives the driver's path: the bundle reaches
// the instance's lima.yaml before the VM boots, and a task event names it.
func TestEnsureMountsPlatformBundle(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	platform, _ := testPlatform(t, "1.0.0", nil)
	spec := testSpec(t, vol)
	spec.Platform = &platform
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)

	st, err := b.Ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, m.PlatformDir(platform.SHA256), st.PlatformDir)

	var doc limaYAML
	require.NoError(t, yaml.Unmarshal([]byte(readFile(t, filepath.Join(f.InstanceDir("box1"), "lima.yaml"))), &doc))
	platformMount := ""
	for _, mnt := range doc.Mounts {
		if mnt.MountPoint == PlatformMountPoint {
			platformMount = mnt.Location
		}
	}
	require.Equal(t, m.PlatformDir(platform.SHA256), platformMount)
	require.Contains(t, strings.Join(*events, "\n"), "platform: Mounted platform bundle 1.0.0 read-only at /opt/box (boxd 0.4.0; tools toolb 2.1.0)")

	// A box without the block mounts nothing at /opt/box.
	plainSpec := testSpec(t, volumeDir(t))
	plainSpec.BoxID = "box2"
	plain, err := m.Box(plainSpec, nil)
	require.NoError(t, err)
	st, err = plain.Ensure(context.Background())
	require.NoError(t, err)
	require.Empty(t, st.PlatformDir)
	var plainDoc limaYAML
	require.NoError(t, yaml.Unmarshal([]byte(readFile(t, filepath.Join(f.InstanceDir("box2"), "lima.yaml"))), &plainDoc))
	for _, mnt := range plainDoc.Mounts {
		require.NotEqual(t, PlatformMountPoint, mnt.MountPoint)
	}
}

// TestEnsureUpgradesRunningBoxToNewBundle is `box upgrade`: the re-registered job
// names another bundle, the running box is stopped, the mount is rewritten and
// the VM comes back on the new bundle.
func TestEnsureUpgradesRunningBoxToNewBundle(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	first, _ := testPlatform(t, "1.0.0", nil)
	second, _ := testPlatform(t, "1.1.0", nil)

	spec := testSpec(t, vol)
	spec.Platform = &first
	rep, events := collectEvents()
	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Running", f.State(t)["box1"]["status"])

	// The new allocation carries the new block while the old VM still runs.
	upgraded := testSpec(t, vol)
	upgraded.Platform = &second
	rep2, events2 := collectEvents()
	b2, err := m.Box(upgraded, rep2)
	require.NoError(t, err)
	st, err := b2.Ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, m.PlatformDir(second.SHA256), st.PlatformDir)
	require.Equal(t, "Running", st.Instance.Status)
	joined := strings.Join(*events2, "\n")
	require.Contains(t, joined, "resize: Box box1 runs an older configuration (platform sha256:"+first.SHA256[:12]+" -> sha256:"+second.SHA256[:12]+")")
	require.Contains(t, joined, "platform: Box box1 takes platform bundle 1.1.0 into its lima configuration")
	require.Contains(t, joined, "platform: Mounted platform bundle 1.1.0 read-only at /opt/box")
	require.NotContains(t, strings.Join(*events, "\n"), "platform bundle 1.1.0")

	var doc limaYAML
	require.NoError(t, yaml.Unmarshal([]byte(readFile(t, filepath.Join(f.InstanceDir("box1"), "lima.yaml"))), &doc))
	location := ""
	for _, mnt := range doc.Mounts {
		if mnt.MountPoint == PlatformMountPoint {
			location = mnt.Location
		}
	}
	require.Equal(t, m.PlatformDir(second.SHA256), location)

	// The box keeps its disk through the upgrade.
	require.Equal(t, "FRESH-DISK", readFile(t, filepath.Join(vol, "box-box1.qcow2")))
	// The old bundle stays in the cache: another box may still mount it.
	require.DirExists(t, m.PlatformDir(first.SHA256))
}

// TestEnsureAddsPlatformToRunningBox: a box created before bundles gets one from
// the next start, without a new instance.
func TestEnsureAddsPlatformToRunningBox(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	vol := volumeDir(t)
	spec := testSpec(t, vol)
	b, err := m.Box(spec, nil)
	require.NoError(t, err)
	_, err = b.Ensure(context.Background())
	require.NoError(t, err)

	platform, _ := testPlatform(t, "1.0.0", nil)
	spec.Platform = &platform
	b, err = m.Box(spec, nil)
	require.NoError(t, err)
	st, err := b.Ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, m.PlatformDir(platform.SHA256), st.PlatformDir)
	require.Contains(t, readFile(t, filepath.Join(f.InstanceDir("box1"), "lima.yaml")), PlatformMountPoint)
}

// TestApplyPlatformMountKeepsLimaFields: Lima adds fields of its own to
// lima.yaml, and writing the mount must not lose them.
func TestApplyPlatformMountKeepsLimaFields(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	spec := testSpec(t, volumeDir(t))
	b, err := m.Box(spec, nil)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(f.InstanceDir("box1"), 0o755))
	path := filepath.Join(f.InstanceDir("box1"), "lima.yaml")
	original := `images:
    - location: /images/base.img
      arch: x86_64
      digest: sha256:abc
cpus: 2
memory: 1024MiB
mountType: 9p
mounts:
    - location: /run/box-files/box1/local
      mountPoint: /etc/box/local
      writable: false
      "9p":
        cache: none
ssh:
    localPort: 60022
    loadDotSSHPubKeys: false
hostResolver:
    enabled: true
`
	// Lima writes its own lima.yaml read-only; the driver must replace it
	// anyway, also when it runs as that file's own user.
	require.NoError(t, os.WriteFile(path, []byte(original), 0o444))

	inst := &limactl.Instance{Name: "box1", Status: limactl.StatusStopped}
	require.NoError(t, b.applyPlatformMount(inst, m.PlatformDir(strings.Repeat("e", 64))))

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(readFile(t, path)), &doc))
	// Everything Lima added is still there.
	ssh, ok := doc["ssh"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 60022, ssh["localPort"])
	require.Equal(t, true, doc["hostResolver"].(map[string]any)["enabled"])
	images := doc["images"].([]any)
	require.Equal(t, "sha256:abc", images[0].(map[string]any)["digest"])
	require.EqualValues(t, 2, doc["cpus"])
	require.Equal(t, "9p", doc["mountType"])
	mounts := doc["mounts"].([]any)
	require.Len(t, mounts, 2)
	require.Equal(t, "/etc/box/local", mounts[0].(map[string]any)["mountPoint"])
	require.Equal(t, PlatformMountPoint, mounts[1].(map[string]any)["mountPoint"])
	require.Equal(t, m.PlatformDir(strings.Repeat("e", 64)), mounts[1].(map[string]any)["location"])
	require.Equal(t, map[string]any{"cache": CacheFscache}, mounts[1].(map[string]any)["9p"])

	// Writing the same mount again changes nothing.
	before, err := fileSHA(path)
	require.NoError(t, err)
	require.NoError(t, b.applyPlatformMount(inst, m.PlatformDir(strings.Repeat("e", 64))))
	after, err := fileSHA(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// A start with no platform block removes the mount and keeps the rest.
	require.NoError(t, b.applyPlatformMount(inst, ""))
	require.NoError(t, yaml.Unmarshal([]byte(readFile(t, path)), &doc))
	require.Len(t, doc["mounts"].([]any), 1)
	require.Contains(t, readFile(t, path), "localPort: 60022")
}

// TestPlatformNameAndDiff covers the wording of the events and of the diff.
func TestPlatformNameAndDiff(t *testing.T) {
	require.Equal(t, "1.2.0", PlatformSpec{Version: "1.2.0"}.String())
	require.Equal(t, "sha256:"+strings.Repeat("f", 12), PlatformSpec{SHA256: strings.Repeat("f", 64)}.String())
	require.Equal(t, "platform mount added (sha256:"+strings.Repeat("a", 12)+")",
		platformDiff("", "/cache/platform/"+strings.Repeat("a", 64)+"/"))
	require.Equal(t, "platform mount removed", platformDiff("/cache/platform/x", ""))
	require.Equal(t, "platform sha256:"+strings.Repeat("a", 12)+" -> sha256:"+strings.Repeat("b", 12),
		platformDiff(strings.Repeat("a", 64), strings.Repeat("b", 64)))

	sum, err := normalizeSHA("sha256:" + strings.Repeat("A", 64))
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("a", 64), sum)
	_, err = normalizeSHA("xyz")
	require.Error(t, err)
}

// A symlink placed by an earlier entry must not carry a later entry out of the
// bundle: each link looks fine alone, and only their chain leaves it.
func TestEnsurePlatformRefusesSymlinkChains(t *testing.T) {
	cases := map[string][]bundleEntry{
		"link created through a link": {
			{name: "a", typeflag: tar.TypeDir, mode: 0o755},
			{name: "a/b", link: "..", mode: 0o777},
			{name: "a/b/c", link: "../victim", mode: 0o777},
		},
		"file written through a link": {
			{name: "a", typeflag: tar.TypeDir, mode: 0o755},
			{name: "a/b", link: "..", mode: 0o777},
			{name: "a/b/evil", body: "x", mode: 0o644},
		},
		"file replacing a link": {
			{name: "bin", typeflag: tar.TypeDir, mode: 0o755},
			{name: "bin/boxd", body: "real", mode: 0o755},
			{name: "bin/boxctl", link: "boxd", mode: 0o777},
			{name: "bin/boxctl", body: "over", mode: 0o755},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t, fakelima.New(t))
			entries = append(bundleFiles("1.0.0")[:1:1], entries...)
			archive, sum := writeBundle(t, t.TempDir(), entries)
			dest := m.PlatformDir(sum)
			victim := filepath.Join(filepath.Dir(dest), "victim")
			require.NoError(t, os.MkdirAll(victim, 0o755))
			b := &Box{m: m}
			require.Error(t, b.unpackPlatform(archive, dest, sum))
			got, err := os.ReadDir(victim)
			require.NoError(t, err)
			require.Empty(t, got)
			_, err = os.Stat(dest)
			require.True(t, os.IsNotExist(err), "a refused bundle leaves no directory")
		})
	}
}

// A second start that unpacked the same bundle must not remove the directory
// the first one is already running from.
func TestUnpackPlatformKeepsCompleteDirectory(t *testing.T) {
	m := newTestManager(t, fakelima.New(t))
	archive, sum := writeBundle(t, t.TempDir(), bundleFiles("1.0.0"))
	dest := m.PlatformDir(sum)
	b := &Box{m: m}
	require.NoError(t, b.unpackPlatform(archive, dest, sum))
	inUse := filepath.Join(dest, "in-use")
	require.NoError(t, os.WriteFile(inUse, []byte("mounted"), 0o644))
	require.NoError(t, b.unpackPlatform(archive, dest, sum))
	require.FileExists(t, inUse)
}
