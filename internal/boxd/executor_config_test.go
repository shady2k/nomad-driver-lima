package boxd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSecrets writes one executor's files into the config's secrets directory.
func writeSecrets(t *testing.T, cfg DaemonConfig, executor string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(cfg.SecretsDir, executor)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// executorInfo finds one executor of the info document.
func executorInfo(t *testing.T, info Info, name string) ExecutorInfo {
	t.Helper()
	for _, ex := range info.Executors {
		if ex.Name == name {
			return ex
		}
	}
	t.Fatalf("the info document has no %q executor: %+v", name, info.Executors)
	return ExecutorInfo{}
}

func TestInfoReportsExecutorsWithoutCredentials(t *testing.T) {
	cfg := testConfig(t)
	newManager(t, cfg)
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	info := d.Info(context.Background())

	prime := executorInfo(t, info, "prime")
	if !prime.Available {
		t.Fatalf("the fake prime-agent is installed, so prime must be available: %+v", prime)
	}
	if prime.Configured == nil || *prime.Configured {
		t.Fatalf("a box without /run/box/secrets/prime must report prime as not configured: %+v", prime)
	}
	if prime.Reason == "" {
		t.Fatal("an unconfigured executor must say why")
	}
}

func TestInfoReportsConfiguredExecutors(t *testing.T) {
	cfg := testConfig(t)
	writeSecrets(t, cfg, "prime", map[string]string{"settings.json": "{}", "auth.json": "{}"})
	writeSecrets(t, cfg, "opencode", map[string]string{"opencode.json": "{}"})
	newManager(t, cfg)
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	info := d.Info(context.Background())

	prime := executorInfo(t, info, "prime")
	if prime.Configured == nil || !*prime.Configured {
		t.Fatalf("prime must be configured with settings.json and auth.json: %+v", prime)
	}
	opencode := executorInfo(t, info, "opencode")
	if opencode.Configured == nil || !*opencode.Configured {
		t.Fatalf("opencode must be configured with opencode.json: %+v", opencode)
	}
}

// TestOpenCodeSeesItsConfigFile checks where the box's opencode.json lands: the
// HOME boxd gives the executor, which is where opencode reads it.
func TestOpenCodeSeesItsConfigFile(t *testing.T) {
	cfg := testConfig(t)
	writeSecrets(t, cfg, "opencode", map[string]string{
		"opencode.json": `{"providers":{"gateway":{"name":"Gateway"}}}`,
		"env":           "GATEWAY_API_KEY=secret\n",
	})
	if _, err := NewManager(cfg); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	config := filepath.Join(cfg.RuntimeDir, "executors", "opencode-home", ".config", "opencode", "opencode.json")
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("opencode.json is not where opencode looks for it: %v", err)
	}
	if string(b) != `{"providers":{"gateway":{"name":"Gateway"}}}` {
		t.Fatalf("opencode.json has the wrong content: %s", b)
	}
	// The credential file is read as environment variables, never copied next to
	// the configuration.
	if _, err := os.Stat(filepath.Join(filepath.Dir(config), "env")); !os.IsNotExist(err) {
		t.Fatalf("the env file must not be copied into the opencode configuration directory: %v", err)
	}
	ex, err := buildExecutor("opencode", cfg.withDefaults())
	if err != nil {
		t.Fatalf("buildExecutor: %v", err)
	}
	if !ex.Configured() {
		t.Fatal("opencode reports itself unconfigured while its files are in place")
	}
}

// TestRuntimeConfigurationIsNotOnTheBoxDisk is C7 in one test: the credentials
// arrive on a tmpfs, and everything boxd derives from them (the mutable copy, the
// executors' HOME, opencode's config) stays in the runtime directory. The state
// directory may hold sessions and transcripts and nothing else.
func TestRuntimeConfigurationIsNotOnTheBoxDisk(t *testing.T) {
	cfg := testConfig(t)
	writeSecrets(t, cfg, "prime", map[string]string{
		"settings.json": `{"providers":{"gateway":{}}}`,
		"auth.json":     `{"gateway":{"access":"secret"}}`,
		"env":           "GATEWAY_API_KEY=secret\n",
	})
	writeSecrets(t, cfg, "opencode", map[string]string{
		"opencode.json": `{"providers":{"gateway":{}}}`,
		"env":           "GATEWAY_API_KEY=secret\n",
	})
	m := newManager(t, cfg)
	if _, err := m.NewSession("prime", "gateway/model", "", "state check"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// The runtime directory has the configuration ...
	for _, rel := range []string{
		"executors/prime/auth.json",
		"executors/prime/settings.json",
		"executors/opencode-home/.config/opencode/opencode.json",
	} {
		if _, err := os.Stat(filepath.Join(cfg.RuntimeDir, rel)); err != nil {
			t.Fatalf("the runtime directory is missing %s: %v", rel, err)
		}
	}

	// ... and the state directory has none of it.
	secrets := map[string]bool{"auth.json": true, "settings.json": true, "opencode.json": true, "env": true}
	err := filepath.Walk(cfg.StateDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if secrets[info.Name()] {
			t.Errorf("%s is on the box disk; a credential may only live on the tmpfs", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", cfg.StateDir, err)
	}
}

// TestVersionProbeDoesNotWriteToTheDaemonsHome is why probeEnv exists: `opencode
// --version` creates its data directory and a log in $HOME, and boxd runs as root
// in the guest, whose home is on the box disk. Every probe therefore runs in the
// executor's own home below the runtime directory (a tmpfs).
func TestVersionProbeDoesNotWriteToTheDaemonsHome(t *testing.T) {
	cfg := testConfig(t)
	bin := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\n" +
		"mkdir -p \"$HOME/.local/share/opencode/log\" && echo probe > \"$HOME/.local/share/opencode/log/opencode.log\"\n" +
		"echo 'opencode v9.9.9'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake opencode: %v", err)
	}
	cfg.OpenCode = bin

	// The home boxd itself runs with: nothing may appear there.
	boxdHome := t.TempDir()
	t.Setenv("HOME", boxdHome)

	if _, err := NewManager(cfg); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ex, err := buildExecutor("opencode", cfg.withDefaults())
	if err != nil {
		t.Fatalf("buildExecutor: %v", err)
	}
	info := ex.Probe(context.Background())
	if info.Version != "opencode v9.9.9" {
		t.Fatalf("the probe did not report the version: %+v", info)
	}

	if _, err := os.Stat(filepath.Join(boxdHome, ".local", "share", "opencode")); !os.IsNotExist(err) {
		t.Fatalf("a version probe wrote into the home of the user boxd runs as, which is on the box disk: %v", err)
	}
	probeLog := filepath.Join(cfg.RuntimeDir, "executors", "opencode-home", ".local", "share", "opencode", "log", "opencode.log")
	if _, err := os.Stat(probeLog); err != nil {
		t.Fatalf("the probe did not run in the executor's home on the runtime directory: %v", err)
	}
}

// TestReloadRefreshesTheExecutors covers the rotation path: new credentials on
// the tmpfs, a reload, and the next turn sees them.
func TestReloadRefreshesTheExecutors(t *testing.T) {
	cfg := testConfig(t)
	writeSecrets(t, cfg, "prime", map[string]string{"auth.json": `{"gateway":{"access":"one"}}`})
	m := newManager(t, cfg)
	copyPath := filepath.Join(cfg.RuntimeDir, "executors", "prime", "auth.json")
	if data, err := os.ReadFile(copyPath); err != nil || !strings.Contains(string(data), `"one"`) {
		t.Fatalf("the first credential is not in the runtime copy: %s %v", data, err)
	}

	// The operator rotates the credential: the driver writes the new file onto the
	// tmpfs and calls reload.
	time.Sleep(10 * time.Millisecond)
	writeSecrets(t, cfg, "prime", map[string]string{"auth.json": `{"gateway":{"access":"two"}}`})
	res, err := m.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if strings.Join(res.Configured, ",") != "prime" {
		t.Fatalf("reload must report prime as configured, got %+v", res)
	}
	if data, err := os.ReadFile(copyPath); err != nil || !strings.Contains(string(data), `"two"`) {
		t.Fatalf("the runtime copy still holds the old credential: %s %v", data, err)
	}
}

// TestReloadDefersARunningTurn keeps a reload from killing work in flight: a
// session with a turn running is told to restart its executor after the turn.
func TestReloadDefersARunningTurn(t *testing.T) {
	cfg := testConfig(t)
	writeSecrets(t, cfg, "prime", map[string]string{"settings.json": "{}"})
	m := newManager(t, cfg)
	s := mustSession(t, m, "prime", "test-model")

	sess, err := m.lookup(s.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// A turn is in flight (the turn machinery itself is covered elsewhere).
	sess.mu.Lock()
	sess.busy = true
	sess.mu.Unlock()

	res, err := m.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if strings.Join(res.Deferred, ",") != s.ID {
		t.Fatalf("a busy session must defer its executor restart, got %+v", res)
	}
	if len(res.Restarted) != 0 {
		t.Fatalf("a busy session must not have its executor stopped under it: %+v", res)
	}
	if !sess.takeReloadPending() {
		t.Fatal("the deferral flag was not set")
	}

	// The session still works afterwards.
	sess.mu.Lock()
	sess.busy = false
	sess.mu.Unlock()
	if _, err := m.Send(s.ID, "hello"); err != nil {
		t.Fatalf("Send after reload: %v", err)
	}
	waitTurn(t, m, s.ID, 1)
}

// TestExecutorHomesExistWithoutCredentials keeps an executor from writing into the
// box user's home (on the disk) when a box has no credentials: HOME always points
// into the runtime directory.
func TestExecutorHomesExistWithoutCredentials(t *testing.T) {
	cfg := testConfig(t)
	if _, err := NewManager(cfg); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, name := range []string{"prime-home", "opencode-home", "claude-home"} {
		dir := filepath.Join(cfg.RuntimeDir, "executors", name)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Fatalf("the %s home must exist even without credentials: %v", name, err)
		}
	}
	ex, err := buildExecutor("prime", cfg.withDefaults())
	if err != nil {
		t.Fatalf("buildExecutor: %v", err)
	}
	_, env, err := ex.Argv(ExecSpec{SessionID: "s-1", Model: "gateway/model", Cwd: cfg.StateDir, SessionDir: cfg.StateDir})
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	home := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			home = strings.TrimPrefix(kv, "HOME=")
		}
	}
	if home != filepath.Join(cfg.RuntimeDir, "executors", "prime-home") {
		t.Fatalf("prime's HOME must be on the tmpfs, got %q", home)
	}
}
