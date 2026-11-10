package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// The platform bundle is a field of the box job: an image whose index entry says
// "platform": "mount" gets one mounted at /opt/box, every other image runs the
// boxd it carries and its job has no platform block at all.

func TestParseImageIndexWithoutPlatforms(t *testing.T) {
	index, err := ParseImageIndex([]byte(indexJSON))
	require.NoError(t, err, "an index published before there were bundles is valid")
	require.NotNil(t, index.Platforms)
	require.Empty(t, index.Platforms)
	require.Empty(t, index.PlatformVersions())
	require.Empty(t, index.PlatformArtifacts())
	require.Equal(t, []string{"1.0.0"}, index.Versions(), "the images still parse")

	artifact, err := index.Resolve("1.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "", artifact.Platform, "an image without the field runs its own boxd")
}

func TestParseImageIndexPlatforms(t *testing.T) {
	index, err := ParseImageIndex([]byte(platformIndexJSON))
	require.NoError(t, err)
	require.Equal(t, []string{"1.10.0", "1.9.0"}, index.PlatformVersions())

	bundle, err := index.ResolvePlatform("1.10.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "1.10.0", bundle.Version)
	require.Equal(t, "amd64", bundle.Arch)
	require.Equal(t, "https://images.test/box-platform/1.10.0/box-platform-1.10.0-linux-amd64.tar.gz", bundle.URL)
	require.Equal(t, "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0", bundle.SHA256)
	require.Equal(t, int64(50331648), bundle.Size)
	require.Equal(t, "2026-11-02T12:00:00Z", bundle.BuiltAt)
	require.Equal(t, "0.5.0", bundle.Boxd)
	require.Equal(t, map[string]string{"editor": "3.0.1", "opencode": "1.3.0"}, bundle.Tools)

	// The image entry's own "platform" field is what decides whether a bundle
	// is mounted.
	mount, err := index.Resolve("1.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "mount", mount.Platform)
	plain, err := index.Resolve("2.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "", plain.Platform)

	// The x86_64 spelling of an architecture is normalised like an image's.
	spelled := `{"images": {"1": {"amd64": {"url": "u", "sha256": "s"}}},
	  "platforms": {"1.0.0": {"x86_64": {"url": "p", "sha256": "s"}}}}`
	index, err = ParseImageIndex([]byte(spelled))
	require.NoError(t, err)
	bundle, err = index.ResolvePlatform("1.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "amd64", bundle.Arch)
}

func TestParseImageIndexPlatformsToleratesNoise(t *testing.T) {
	// A platforms entry that is not an object, and a tools value that is not a
	// string, must not fail the whole index: one broken bundle does not hide
	// the images beside it.
	body := `{
	  "images": {"1.0.0": {"amd64": {"url": "u", "sha256": "s", "platform": "mount"}}},
	  "platforms": {
	    "1.0.0": {
	      "built_at": "2026-11-02T12:00:00Z",
	      "amd64": {"url": "p", "sha256": "s", "boxd": "0.5.0", "tools": {"a": "1", "b": {"nested": true}, "c": "3"}}
	    },
	    "2.0.0": "not an object"
	  }
	}`
	index, err := ParseImageIndex([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"1.0.0"}, index.PlatformVersions(), "the entry that is not an object is skipped")
	bundle, err := index.ResolvePlatform("1.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "0.5.0", bundle.Boxd)
	require.Equal(t, map[string]string{"a": "1", "c": "3"}, bundle.Tools, "a tool that is not a string is skipped")

	// "platforms" that is not an object of versions is no bundles, not an error.
	odd := `{"images": {"1.0.0": {"amd64": {"url": "u", "sha256": "s"}}}, "platforms": ["nonsense"]}`
	index, err = ParseImageIndex([]byte(odd))
	require.NoError(t, err)
	require.Empty(t, index.Platforms)
}

func TestPlatformArtifactsOrder(t *testing.T) {
	index, err := ParseImageIndex([]byte(platformIndexJSON))
	require.NoError(t, err)
	artifacts := index.PlatformArtifacts()
	require.Len(t, artifacts, 3)
	require.Equal(t, "1.10.0", artifacts[0].Version, "the newest version first")
	require.Equal(t, "amd64", artifacts[0].Arch, "amd64 before arm64")
	require.Equal(t, "arm64", artifacts[1].Arch)
	require.Equal(t, "1.9.0", artifacts[2].Version)
}

func TestResolvePlatform(t *testing.T) {
	index, err := ParseImageIndex([]byte(platformIndexJSON))
	require.NoError(t, err)

	// "latest" is the newest version that has the architecture, ordered the way
	// the images are ordered: 1.10.0 comes after 1.9.0.
	latest, err := index.ResolvePlatform("latest", "amd64")
	require.NoError(t, err)
	require.Equal(t, "1.10.0", latest.Version)
	latest, err = index.ResolvePlatform("", "arm64")
	require.NoError(t, err)
	require.Equal(t, "1.10.0", latest.Version)
	require.Equal(t, "arm64", latest.Arch)

	named, err := index.ResolvePlatform("1.9.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "1.9.0", named.Version)

	_, err = index.ResolvePlatform("9.9.9", "amd64")
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), `there is no platform bundle version "9.9.9"`)
	require.Contains(t, err.Error(), "published versions: 1.10.0, 1.9.0")

	_, err = index.ResolvePlatform("1.9.0", "arm64")
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), "is not published for architecture arm64")
	require.Contains(t, err.Error(), "it has: amd64")

	_, err = index.ResolvePlatform("latest", "riscv64")
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), "no platform bundle is published for architecture riscv64")

	// An index without bundles says so instead of naming versions.
	plain, err := ParseImageIndex([]byte(indexJSON))
	require.NoError(t, err)
	_, err = plain.ResolvePlatform("latest", "amd64")
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), "lists no platform bundles yet")
}

func TestPlatformArtifactValidate(t *testing.T) {
	digest := strings.Repeat("a1b2c3d4", 8)
	good := &PlatformArtifact{Version: "1.0.0", Arch: "amd64", URL: "u", SHA256: "SHA256:" + strings.ToUpper(digest)}
	require.NoError(t, good.Validate())
	require.Equal(t, digest, good.SHA256, "the prefix and the case are normalised")

	for name, bundle := range map[string]*PlatformArtifact{
		"no url":    {Version: "1.0.0", Arch: "amd64", SHA256: digest},
		"no sha256": {Version: "1.0.0", Arch: "amd64", URL: "u"},
		"short":     {Version: "1.0.0", Arch: "amd64", URL: "u", SHA256: "abc"},
		"not hex":   {Version: "1.0.0", Arch: "amd64", URL: "u", SHA256: "zz" + digest[2:]},
	} {
		err := bundle.Validate()
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "platform bundle 1.0.0/amd64", name)
	}
}

func TestRenderJobPlatformBlock(t *testing.T) {
	spec := goodSpec()
	spec.PlatformVersion = "1.10.0"
	spec.PlatformURL = "https://images.test/box-platform/1.10.0/box-platform-1.10.0-linux-amd64.tar.gz"
	spec.PlatformSHA256 = "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0"

	job, err := RenderJob(spec)
	require.NoError(t, err)
	config := job.TaskGroups[0].Tasks[0].Config
	platform, ok := config["platform"].(map[string]any)
	require.True(t, ok, "the task config carries the platform block")
	require.Equal(t, "1.10.0", platform["version"])
	require.Equal(t, spec.PlatformURL, platform["url"])
	require.Equal(t, spec.PlatformSHA256, platform["sha256"])

	back, err := SpecFromJob(job)
	require.NoError(t, err)
	require.Equal(t, "1.10.0", back.PlatformVersion)
	require.Equal(t, spec.PlatformURL, back.PlatformURL)
	require.Equal(t, spec.PlatformSHA256, back.PlatformSHA256)

	// Without a bundle the job is the job it was before bundles existed.
	job, err = RenderJob(goodSpec())
	require.NoError(t, err)
	require.NotContains(t, job.TaskGroups[0].Tasks[0].Config, "platform")
	back, err = SpecFromJob(job)
	require.NoError(t, err)
	require.Equal(t, "", back.PlatformVersion)
	require.Equal(t, "", back.PlatformURL)
	require.Equal(t, "", back.PlatformSHA256)
}

func TestRenderJobPlatformBlockMustBeComplete(t *testing.T) {
	for name, change := range map[string]func(*BoxSpec){
		"version only": func(s *BoxSpec) { s.PlatformVersion = "1.10.0" },
		"url only":     func(s *BoxSpec) { s.PlatformURL = "https://images.test/p.tar.gz" },
		"no sha256":    func(s *BoxSpec) { s.PlatformVersion = "1.10.0"; s.PlatformURL = "https://images.test/p.tar.gz" },
		"no url": func(s *BoxSpec) {
			s.PlatformVersion = "1.10.0"
			s.PlatformSHA256 = strings.Repeat("a1b2c3d4", 8)
		},
	} {
		spec := goodSpec()
		change(&spec)
		_, err := RenderJob(spec)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "needs both a url and a sha256", name)
		require.Equal(t, ExitUsage, ExitCode(err), name)
	}
}

// mountBox creates a box from the image that takes a platform bundle, so the
// upgrade tests start from a job that already has one.
func mountBox(t *testing.T, ta *testApp, args ...string) {
	t.Helper()
	base := []string{"create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "4"}
	require.Equal(t, ExitOK, ta.run(t, append(base, args...)...), ta.errOut.String())
}

func TestCreateMountsTheNewestPlatformBundle(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta)

	config := boxTaskConfig(t, ta, "box-dev1")
	platform, ok := config["platform"].(map[string]any)
	require.True(t, ok, "an image that says \"platform\": \"mount\" gets a bundle")
	require.Equal(t, "1.10.0", platform["version"], "the newest published bundle")
	require.Equal(t, "https://images.test/box-platform/1.10.0/box-platform-1.10.0-linux-amd64.tar.gz", platform["url"])
	require.Equal(t, "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0", platform["sha256"])
	require.Contains(t, ta.errOut.String(), "platform 1.10.0 (boxd 0.5.0, sha256 502629c71242)")
}

func TestCreateMountsANamedPlatformBundle(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta, "--platform", "1.9.0")

	platform := boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)
	require.Equal(t, "1.9.0", platform["version"])
	require.Equal(t, "425210d28bc8b720946a9f17403d27bece85b3a131d958538bed3dea2edea986", platform["sha256"])
}

func TestCreatePlatformJSON(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta, "--json")

	var doc createOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.NotNil(t, doc.Platform)
	require.Equal(t, "1.10.0", doc.Platform.Version)
	require.Equal(t, "amd64", doc.Platform.Arch)
	require.Equal(t, "0.5.0", doc.Platform.Boxd)
	require.Equal(t, map[string]string{"editor": "3.0.1", "opencode": "1.3.0"}, doc.Platform.Tools)
}

func TestCreateImageWithoutPlatform(t *testing.T) {
	// An image that carries its own boxd renders exactly as it did before
	// bundles existed: no platform key at all.
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "2.0.0", "--cpus", "1",
		"--memory-mib", "512", "--disk-gib", "4"), ta.errOut.String())
	require.NotContains(t, boxTaskConfig(t, ta, "box-dev1"), "platform")
	require.NotContains(t, ta.errOut.String(), "platform")

	// --platform does not apply to such an image, and the refusal says why.
	ta = newTestApp(t, platformIndexJSON)
	rc := ta.run(t, "create", "dev1", "--image", "2.0.0", "--platform", "1.10.0", "--cpus", "1",
		"--memory-mib", "512", "--disk-gib", "4")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "the image 2.0.0 does not take a platform bundle")
	require.Contains(t, ta.errOut.String(), "runs the boxd it carries")
	require.Empty(t, ta.cluster.jobs)
}

func TestCreateMountImageWithoutPublishedBundles(t *testing.T) {
	body := editIndex(t, platformIndexJSON, func(doc map[string]any) { delete(doc, "platforms") })
	ta := newTestApp(t, body)
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "4")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "the image 1.0.0 needs a platform bundle")
	require.Contains(t, ta.errOut.String(), "lists none yet")
	require.Contains(t, ta.errOut.String(), "http://images.test/box-base/index.json")
	require.Empty(t, ta.cluster.jobs)
	require.Empty(t, ta.cluster.volumes)
}

func TestCreateWithoutABundleForTheArchitecture(t *testing.T) {
	// Bundles exist, but only for the other architecture: that is a not-found,
	// and it names what is published.
	body := editIndex(t, platformIndexJSON, func(doc map[string]any) {
		platforms := jsonObject(t, doc, "platforms")
		for _, version := range []string{"1.9.0", "1.10.0"} {
			delete(jsonObject(t, platforms, version), "amd64")
		}
	})
	ta := newTestApp(t, body)
	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--arch", "amd64", "--cpus", "1",
		"--memory-mib", "512", "--disk-gib", "4")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "the image 1.0.0 needs a platform bundle and none is published for architecture amd64")
	require.Contains(t, ta.errOut.String(), "published: 1.10.0")
	require.Empty(t, ta.cluster.jobs)
}

func TestUpgradeMovesToTheNewestBundle(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta, "--platform", "1.9.0")
	require.Equal(t, "1.9.0", boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)["version"])
	before := callCount(ta, "RegisterJob box-dev1")

	require.Equal(t, ExitOK, ta.run(t, "upgrade", "dev1"), ta.errOut.String())
	require.Equal(t, before+1, callCount(ta, "RegisterJob box-dev1"), "the job was re-registered once")
	require.Contains(t, ta.errOut.String(), "upgrade: box dev1 is running")
	require.Contains(t, ta.errOut.String(), "platform 1.9.0 -> 1.10.0")
	require.Contains(t, ta.errOut.String(), "its disk and its sessions' transcripts are kept")

	platform := boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)
	require.Equal(t, "1.10.0", platform["version"])
	require.Equal(t, "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0", platform["sha256"])
}

func TestUpgradeMovesToANamedBundle(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta) // 1.10.0

	require.Equal(t, ExitOK, ta.run(t, "upgrade", "dev1", "--platform", "1.9.0"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "platform 1.10.0 -> 1.9.0")
	platform := boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)
	require.Equal(t, "1.9.0", platform["version"])
	require.Equal(t, "425210d28bc8b720946a9f17403d27bece85b3a131d958538bed3dea2edea986", platform["sha256"])
}

func TestUpgradeAddsTheFirstBundle(t *testing.T) {
	// A box created before there were bundles has none; upgrading it onto the
	// newest one is the migration path, and the change says "none".
	ta := newTestApp(t, platformIndexJSON)
	ta.addBox(t, "dev1")
	require.NotContains(t, boxTaskConfig(t, ta, "box-dev1"), "platform")
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }

	require.Equal(t, ExitOK, ta.run(t, "upgrade", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "platform none -> 1.10.0")
	require.Equal(t, "1.10.0", boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)["version"])
}

func TestUpgradeSameVersionChangesNothing(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta) // 1.10.0, the newest
	before := callCount(ta, "RegisterJob")

	require.Equal(t, ExitOK, ta.run(t, "upgrade", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "nothing to change: the box already mounts platform bundle 1.10.0")
	require.Equal(t, before, callCount(ta, "RegisterJob"), "the job is not registered again")
}

func TestUpgradeRefusesAnImageWithoutPlatform(t *testing.T) {
	// With and without --platform: the image runs the boxd it carries, and a
	// bundle cannot be mounted over it.
	for _, args := range [][]string{
		{"upgrade", "dev1"},
		{"upgrade", "dev1", "--platform", "1.10.0"},
	} {
		ta := newTestApp(t, platformIndexJSON)
		ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
		require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "2.0.0", "--cpus", "1",
			"--memory-mib", "512", "--disk-gib", "4"), ta.errOut.String())
		ta.errOut.Reset()
		before := callCount(ta, "RegisterJob")

		rc := ta.run(t, args...)
		require.Equal(t, ExitUsage, rc, strings.Join(args, " "))
		require.Contains(t, ta.errOut.String(), "the image 2.0.0 does not take a platform bundle")
		require.Contains(t, ta.errOut.String(), "has to be rebuilt for bundles first")
		require.Equal(t, before, callCount(ta, "RegisterJob"), "nothing was registered")
	}
}

func TestUpgradeUnpublishedVersion(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta)

	rc := ta.run(t, "upgrade", "dev1", "--platform", "9.9.9")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), `there is no platform bundle version "9.9.9"`)
	require.Contains(t, ta.errOut.String(), "published versions: 1.10.0, 1.9.0")
}

func TestUpgradeWithoutABundleForTheArchitecture(t *testing.T) {
	body := editIndex(t, platformIndexJSON, func(doc map[string]any) {
		delete(jsonObject(t, jsonObject(t, doc, "platforms"), "1.10.0"), "amd64")
		delete(jsonObject(t, jsonObject(t, doc, "platforms"), "1.9.0"), "amd64")
	})
	ta := newTestApp(t, body)
	ta.addBox(t, "dev1")
	rc := ta.run(t, "upgrade", "dev1")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "no platform bundle is published for architecture amd64")
}

func TestUpgradeNeedsTheIndex(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBox(t, "dev1")
	ta.images.status = http.StatusNotFound
	rc := ta.run(t, "upgrade", "dev1")
	require.Equal(t, ExitError, rc)
	require.Contains(t, ta.errOut.String(), "404")
}

func TestUpgradeWithoutWaiting(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) {
		// The new allocation has not started yet.
		c.allocSeq++
		c.allocs["alloc-pending"] = &api.Allocation{
			ID: "alloc-pending", JobID: derefStr(job.ID, ""), NodeID: "node-1", NodeName: "node-1",
			ClientStatus: "pending", CreateIndex: uint64(c.allocSeq),
			TaskStates: map[string]*api.TaskState{boxTaskName: {State: "pending"}},
		}
	}
	mountBox(t, ta, "--platform", "1.9.0", "--wait=false")

	rc := ta.run(t, "upgrade", "dev1", "--wait=false")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the box is starting on platform bundle 1.10.0")
	require.Equal(t, "1.10.0", boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)["version"])
}

func TestUpgradeJSON(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta, "--platform", "1.9.0")

	require.Equal(t, ExitOK, ta.run(t, "upgrade", "dev1", "--json"), ta.errOut.String())
	var doc lifecycleOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "upgrade", doc.Action)
	require.Equal(t, "dev1", doc.Box.ID)
	require.Equal(t, []string{"platform 1.9.0 -> 1.10.0"}, doc.Changes)
	require.Contains(t, doc.Warn, "the box restarted to mount the new bundle")
}

func TestStatusShowsThePlatformBundle(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta)

	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "platform 1.10.0 (read-only mount at /opt/box)")

	ta.errOut.Reset()
	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"), ta.errOut.String())
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "1.10.0", doc.Spec.Platform)
}

func TestStatusOfABoxWithoutABundle(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "read-only mount")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"))
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "", doc.Spec.Platform)
}

func TestImagesShowsPlatformBundles(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	require.Equal(t, ExitOK, ta.run(t, "images"), ta.errOut.String())
	table := ta.out.String()
	require.Contains(t, table, "VERSION", "the image table keeps its version column")
	require.Contains(t, table, "BOXD")
	require.Contains(t, table, "TOOLS")
	require.Contains(t, table, "1.10.0")
	require.Contains(t, table, "0.5.0")
	require.Contains(t, table, "editor 3.0.1, opencode 1.3.0", "the tools are sorted by name")
	require.Contains(t, ta.errOut.String(), "they are not images for --image")
	require.Contains(t, ta.errOut.String(), "3 platform bundles from http://images.test/box-base/index.json")
	require.NotContains(t, ta.errOut.String(), "no platform bundles are published yet")

	// The bundle table's first column is BUNDLE, not VERSION: a reader must not
	// mistake a bundle version for an image version.
	lines := strings.Split(strings.TrimSpace(table), "\n")
	require.True(t, strings.HasPrefix(lines[0], "VERSION "), "the first table is the images: %q", lines[0])
	bundleHeader := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "BUNDLE ") {
			bundleHeader = i
			break
		}
	}
	require.NotEqual(t, -1, bundleHeader, "the bundle table has a BUNDLE column: %q", table)
	require.True(t, strings.HasPrefix(lines[bundleHeader], "BUNDLE "))

	// The tail of the output names the image to use, not a bundle version.
	errLines := strings.Split(strings.TrimSpace(ta.errOut.String()), "\n")
	last := errLines[len(errLines)-1]
	require.True(t, strings.HasPrefix(last, "create a box with: box create <id> --image 2.0.0"),
		"the last line must name the newest image, got %q", last)

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "images", "--json"))
	var doc imagesOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Platforms, 3)
	require.Equal(t, "1.10.0", doc.Platforms[0].Version)
	require.Equal(t, "amd64", doc.Platforms[0].Arch)
	require.Equal(t, "0.5.0", doc.Platforms[0].Boxd)
}

func TestCreateWithABundleVersionSaysItIsNotAnImage(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	rc := ta.run(t, "create", "dev1", "--image", "1.10.0", "--cpus", "1", "--memory-mib", "512", "--disk-gib", "4")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(),
		"1.10.0 is a platform bundle, not an image; the newest image is 2.0.0")
	require.Contains(t, ta.errOut.String(), `there is no image version "1.10.0"`)
}

func TestImagesSaysWhenThereAreNoBundles(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "images"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "no platform bundles are published yet")
	require.Contains(t, ta.errOut.String(), "every image runs the boxd it carries")

	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "images", "--json"))
	var doc imagesOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Empty(t, doc.Platforms)

	// The 404 index says both things: no images, and no bundles.
	ta = newTestApp(t, "")
	ta.images.status = http.StatusNotFound
	require.Equal(t, ExitOK, ta.run(t, "images"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "answers 404")
	require.Contains(t, ta.errOut.String(), "no platform bundles are published yet")
}

func TestUpgradeIsRegistered(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	require.Equal(t, ExitOK, ta.run(t, "help"))
	require.Contains(t, ta.errOut.String(), "upgrade")
	require.Contains(t, ta.errOut.String(), "move a box to a newer platform bundle")

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "help", "upgrade"))
	msg := ta.errOut.String()
	require.Contains(t, msg, "box upgrade: move a box to a newer platform bundle")
	require.Contains(t, msg, "--platform")
	require.Contains(t, msg, "--timeout")
	require.Contains(t, msg, "--wait")
}

// editIndex rewrites an index fixture: change gets the decoded document.
func editIndex(t *testing.T, body string, change func(doc map[string]any)) string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &doc))
	change(doc)
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(out)
}

// jsonObject returns a nested object of a decoded index document.
func jsonObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	obj, ok := parent[key].(map[string]any)
	require.True(t, ok, "%q is an object", key)
	return obj
}

// TestCreateDropsABundleWhenOnlyAnotherArchIsChosen is the trap in a mixed
// index: the box's default architecture search resolves a bundle for amd64,
// finds no node there, and then settles on an arm64 image that carries its own
// boxd. That job must have no platform block -- the bundle of the candidate
// that was not chosen must not travel with it.
func TestCreateDropsABundleWhenOnlyAnotherArchIsChosen(t *testing.T) {
	digest := "5dbac55d46a0cae6368a4a820dd2cf610371873e506f8230ab5c961b7cc0ffc1"
	index := `{
  "images": {
    "1.0.0": {"amd64": {"url": "https://images.test/base-1-amd64.qcow2", "sha256": "` + digest + `", "platform": "mount"}},
    "2.0.0": {"arm64": {"url": "https://images.test/base-2-arm64.qcow2", "sha256": "` + digest + `"}}
  },
  "platforms": {
    "1.10.0": {"amd64": {"url": "https://images.test/box-platform-1.10.0-amd64.tar.gz", "sha256": "` + digest + `", "boxd": "0.5.0"}}
  }
}`
	ta := newTestApp(t, index)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	// The amd64 node has no room for 2048 MiB; a second, arm64 node has.
	ta.cluster.nodes["node-1"].NodeResources.Memory.MemoryMB = 1024
	ta.cluster.addNode("node-2", "node-2", "arm64", 12000, 11961, 150000)

	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "latest", "--memory-mib", "2048"))
	require.Equal(t, "arm64", ta.cluster.jobs["box-dev1"].Meta["arch"])
	config := boxTaskConfig(t, ta, "box-dev1")
	require.NotContains(t, config, "platform", "the amd64 candidate's bundle must not follow the arm64 image")
	require.NotContains(t, ta.errOut.String(), "platform ")
}

// TestResizeAndStartKeepThePlatformBlock: a start and a resize re-render the job
// from the spec they read back, so an upgrade must not be undone by the next
// resize -- a box would lose its bundle by being resized.
func TestResizeAndStartKeepThePlatformBlock(t *testing.T) {
	ta := newTestApp(t, platformIndexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	mountBox(t, ta)
	before := boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)

	require.Equal(t, ExitOK, ta.run(t, "resize", "dev1", "--cpus", "2"), ta.errOut.String())
	after := boxTaskConfig(t, ta, "box-dev1")["platform"].(map[string]any)
	require.Equal(t, before, after, "a resize keeps the bundle")

	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"))
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"))
	require.Equal(t, before, boxTaskConfig(t, ta, "box-dev1")["platform"], "a start keeps the bundle")
}
