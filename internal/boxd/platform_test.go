package boxd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBundle writes a platform bundle manifest into dir.
func writeBundle(t *testing.T, dir, manifest string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

// TestReadPlatformReadsTheManifest is what "boxctl info" reports about the
// bundle the node mounted: its version, the boxd it carries and its tools.
func TestReadPlatformReadsTheManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "platform")
	writeBundle(t, dir, `{"version":"2026.02.1","boxd":"1.2.3","tools":{"boxctl":"1.2.3","git":"2.43.0"}}`)

	got := readPlatform(dir)
	if !got.Mounted {
		t.Fatalf("a bundle with a manifest must report itself mounted: %+v", got)
	}
	if got.Path != dir {
		t.Fatalf("path = %q, want %q", got.Path, dir)
	}
	if got.Version != "2026.02.1" {
		t.Fatalf("version = %q", got.Version)
	}
	if got.Boxd != "1.2.3" {
		t.Fatalf("boxd = %q", got.Boxd)
	}
	if got.Tools["boxctl"] != "1.2.3" || got.Tools["git"] != "2.43.0" {
		t.Fatalf("tools = %v", got.Tools)
	}
	if got.Reason != "" {
		t.Fatalf("a mounted bundle needs no reason: %q", got.Reason)
	}
}

// TestReadPlatformWithoutABundle covers the two ways a box has no usable
// bundle: nothing mounted, and a manifest that cannot be used. Both say which
// path was looked at.
func TestReadPlatformWithoutABundle(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "platform")
	got := readPlatform(missing)
	if got.Mounted {
		t.Fatalf("no directory, but the bundle reports itself mounted: %+v", got)
	}
	if got.Path != missing {
		t.Fatalf("path = %q, want %q", got.Path, missing)
	}
	if want := "no bundle is mounted at " + missing; got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}

	for name, manifest := range map[string]string{
		"invalid":    `{"version":`,
		"no version": `{"boxd":"1.2.3"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "platform")
			writeBundle(t, dir, manifest)
			got := readPlatform(dir)
			if got.Mounted {
				t.Fatalf("an unusable manifest must not report the bundle mounted: %+v", got)
			}
			if got.Version != "" || got.Boxd != "" || got.Tools != nil {
				t.Fatalf("an unusable manifest must report nothing: %+v", got)
			}
			// The reason names the path and says what is wrong with the manifest.
			if !strings.HasPrefix(got.Reason, "the bundle at "+dir+" has no readable manifest.json: ") {
				t.Fatalf("reason = %q", got.Reason)
			}
		})
	}
}

// TestPlatformDirIsOptBoxByDefault keeps the daemon's own default honest: a real
// box mounts the bundle at /opt/box and its addons next to it.
func TestPlatformDirIsOptBoxByDefault(t *testing.T) {
	cfg := DaemonConfig{}.withDefaults()
	if got := cfg.platformDir(); got != "/opt/box" {
		t.Fatalf("platform dir = %q, want /opt/box", got)
	}
	if got := cfg.platformBinDir(); got != "/opt/box/bin" {
		t.Fatalf("platform bin dir = %q, want /opt/box/bin", got)
	}
	if got := cfg.addonsDir(); got != "/opt/box/addons" {
		t.Fatalf("addons dir = %q, want /opt/box/addons", got)
	}

	custom := DaemonConfig{PlatformDir: "/mnt/bundle"}
	if got := custom.platformBinDir(); got != "/mnt/bundle/bin" {
		t.Fatalf("platform bin dir = %q, want /mnt/bundle/bin", got)
	}
	if got := custom.addonsDir(); got != "/mnt/bundle/addons" {
		t.Fatalf("addons dir = %q, want /mnt/bundle/addons", got)
	}
	if got := (DaemonConfig{PlatformDir: "/mnt/bundle", AddonsDir: "/addons"}).addonsDir(); got != "/addons" {
		t.Fatalf("an explicit addons dir must win, got %q", got)
	}
}

// TestExecutorEnvPutsTheBundleFirstOnPATH is the reason the bundle carries the
// executors: an executor the box mounts runs before anything the image
// installed, and what the inherited PATH has stays behind it.
func TestExecutorEnvPutsTheBundleFirstOnPATH(t *testing.T) {
	cfg := testConfig(t)
	if _, err := NewManager(cfg); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	dirs := cfg.dirsOf("prime")
	t.Setenv("PATH", "/usr/bin:/bin")

	env, err := executorEnv("", dirs, cfg.platformBinDir(), nil)
	if err != nil {
		t.Fatalf("executorEnv: %v", err)
	}
	sep := string(filepath.ListSeparator)
	want := strings.Join([]string{
		filepath.Join(cfg.PlatformDir, "bin"),
		"/usr/bin",
		"/bin",
		filepath.Join(dirs.home, "go", "bin"),
		filepath.Join(dirs.home, ".local", "bin"),
	}, sep)
	if got := envValue(env, "PATH"); got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if got := envValue(env, "HOME"); got != dirs.home {
		t.Fatalf("HOME = %q, want the executor's own home %q", got, dirs.home)
	}
	// The home directories come once: appendFiltered and withPath do not repeat
	// what is already there.
	for _, dir := range []string{filepath.Join(dirs.home, "go", "bin"), filepath.Join(dirs.home, ".local", "bin")} {
		if n := strings.Count(envValue(env, "PATH"), dir); n != 1 {
			t.Fatalf("%s appears %d times on PATH %q", dir, n, envValue(env, "PATH"))
		}
	}

	// The probe runs where the daemon would run the executor: the bundle's bin
	// first, the executor's HOME as its sessions get it.
	probe := probeEnv(dirs, cfg.platformBinDir())
	if got := strings.Split(envValue(probe, "PATH"), sep)[0]; got != filepath.Join(cfg.PlatformDir, "bin") {
		t.Fatalf("the probe PATH starts with %q, want the bundle's bin first", got)
	}
	if got := envValue(probe, "HOME"); got != dirs.home {
		t.Fatalf("the probe HOME = %q, want %q", got, dirs.home)
	}
}

// TestWithPathFront checks the helper itself: front order as given, no
// duplicates, and a PATH-less environment keeps the usual default behind the
// bundle.
func TestWithPathFront(t *testing.T) {
	sep := string(filepath.ListSeparator)
	if got, want := withPathFront("/usr/bin:/bin", "/opt/box/bin"), "/opt/box/bin"+sep+"/usr/bin:/bin"; got != want {
		t.Fatalf("withPathFront = %q, want %q", got, want)
	}
	if got, want := withPathFront("/usr/bin", "/opt/box/bin"), "/opt/box/bin"+sep+"/usr/bin"; got != want {
		t.Fatalf("withPathFront = %q, want %q", got, want)
	}
	// Already on PATH: left where it is, not added twice.
	if got, want := withPathFront("/usr/bin:/opt/box/bin", "/opt/box/bin"), "/usr/bin:/opt/box/bin"; got != want {
		t.Fatalf("withPathFront = %q, want %q", got, want)
	}
	if got, want := withPathFront("/usr/bin", ""), "/usr/bin"; got != want {
		t.Fatalf("withPathFront = %q, want no change for an empty dir (%q)", got, want)
	}
	if got, want := withPathFront("", "/opt/box/bin"), "/opt/box/bin"+sep+defaultPath; got != want {
		t.Fatalf("withPathFront = %q, want %q", got, want)
	}
}

// TestInfoCarriesThePlatform is the "boxctl info" half of the bundle: the
// document always has a platform object, mounted or not.
func TestInfoCarriesThePlatform(t *testing.T) {
	cfg := testConfig(t)
	writeBundle(t, cfg.PlatformDir, `{"version":"2026.02.1","boxd":"1.2.3","tools":{"boxctl":"1.2.3"}}`)
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	info := d.Info(t.Context())
	if info.Platform.Path != cfg.PlatformDir {
		t.Fatalf("platform path = %q, want %q", info.Platform.Path, cfg.PlatformDir)
	}
	if !info.Platform.Mounted {
		t.Fatalf("the bundle must report itself mounted: %+v", info.Platform)
	}
	if info.Platform.Version != "2026.02.1" || info.Platform.Boxd != "1.2.3" {
		t.Fatalf("platform = %+v", info.Platform)
	}
	if info.Platform.Tools["boxctl"] != "1.2.3" {
		t.Fatalf("platform tools = %v", info.Platform.Tools)
	}
	// The start line says the same thing: which bundle this box runs.
	if line := d.startLine(t.Context()); !strings.Contains(line, "; platform 2026.02.1 (boxd 1.2.3)") {
		t.Fatalf("start line = %q", line)
	}

	// A box whose image has no bundle: the document says so, with the path.
	plain := testConfig(t)
	d, err = NewDaemon(plain)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	info = d.Info(t.Context())
	if info.Platform.Mounted {
		t.Fatalf("no bundle, but the platform reports itself mounted: %+v", info.Platform)
	}
	if info.Platform.Path != plain.PlatformDir {
		t.Fatalf("platform path = %q, want %q", info.Platform.Path, plain.PlatformDir)
	}
	if want := "no bundle is mounted at " + plain.PlatformDir; info.Platform.Reason != want {
		t.Fatalf("platform reason = %q, want %q", info.Platform.Reason, want)
	}
	if line := d.startLine(t.Context()); !strings.Contains(line, "; no bundle mounted at "+plain.PlatformDir) {
		t.Fatalf("start line = %q", line)
	}
}
