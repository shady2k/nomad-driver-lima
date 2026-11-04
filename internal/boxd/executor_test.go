package boxd

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/boxd/acp"
)

// TestOpenCodeACPHandshake checks the opencode adapter against a really
// installed opencode (no prompt, so no model request): boxd's argv plus ACP
// handshake must work with a third-party agent, not just our fake.
func TestOpenCodeACPHandshake(t *testing.T) {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed on this host")
	}
	cfg := testConfig(t)
	cfg.OpenCode = bin
	// An empty HOME keeps the host's opencode profile out of the test.
	home := filepath.Join(cfg.StateDir, "homes", "opencode")
	if err := mkdirAll(home); err != nil {
		t.Fatal(err)
	}
	ex, err := buildExecutor("opencode", cfg)
	if err != nil {
		t.Fatal(err)
	}
	probe := ex.Probe(t.Context())
	if !probe.Available {
		t.Fatalf("opencode not available: %+v", probe)
	}
	sessionDir := filepath.Join(cfg.StateDir, "probe")
	argv, env, err := ex.Argv(ExecSpec{SessionID: "s-probe", Model: "opencode/big-pickle", Cwd: cfg.StateDir, SessionDir: sessionDir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != bin+" acp" {
		t.Fatalf("opencode argv = %v", argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = cfg.StateDir
	client, err := acp.NewClient(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start opencode: %v", err)
	}
	defer client.Shutdown(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx, "boxd-test"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ns, err := client.NewSession(ctx, cfg.StateDir)
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if ns.SessionID == "" {
		t.Fatal("no session id from opencode")
	}
	// The model option of opencode carries "provider/model" as a plain string,
	// which is the shape boxd sends.
	found := false
	for _, o := range ns.ConfigOptions {
		if o.ID == "model" {
			found = true
			if !strings.Contains(o.CurrentValue, "/") {
				t.Fatalf("opencode model value %q is not provider/model", o.CurrentValue)
			}
			if id, value, ok := modelOption([]configOption{{ID: "model", CurrentValue: o.CurrentValue}}, "opencode/big-pickle"); !ok || id != "model" || value != "opencode/big-pickle" {
				t.Fatalf("model option mapping = %q %q %v", id, value, ok)
			}
		}
	}
	if !found {
		t.Fatalf("opencode exposed no model config option: %+v", ns.ConfigOptions)
	}
	if err := client.CloseSession(ctx, ns.SessionID); err != nil {
		t.Fatalf("session/close: %v", err)
	}
}

// TestUnitFile checks the two units boxd ships: the bundle layout, which runs
// the daemon out of /opt/box, and the installed layout, which is the image's own
// /usr/local/bin/boxd.
func TestUnitFile(t *testing.T) {
	bundle := []string{
		"ExecStart=/opt/box/bin/boxd",
		"--socket /run/boxd/boxd.sock",
		"--state-dir /var/lib/boxd",
		"--runtime-dir /run/boxd",
		"--secrets-dir /run/box/secrets",
		"--local-dir /etc/box/local",
		"RequiresMountsFor=/opt/box /run/box/secrets /etc/box/local",
		"Environment=PATH=/opt/box/bin:",
		"Restart=always",
		"StateDirectory=boxd",
		"RuntimeDirectory=boxd",
		"OOMPolicy=continue",
		"OOMScoreAdjust=-800",
		"WantedBy=multi-user.target",
	}
	for _, want := range bundle {
		if !strings.Contains(UnitFile, want) {
			t.Errorf("the bundle unit is missing %q", want)
		}
	}
	if strings.Contains(UnitFile, "/usr/local/bin/boxd") {
		t.Errorf("the bundle unit must not start /usr/local/bin/boxd:\n%s", UnitFile)
	}

	installed := []string{
		"ExecStart=/usr/local/bin/boxd",
		"--state-dir /var/lib/boxd",
		"RequiresMountsFor=/run/box/secrets /etc/box/local",
		"Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"Restart=always",
		"StateDirectory=boxd",
		"RuntimeDirectory=boxd",
		"OOMPolicy=continue",
		"OOMScoreAdjust=-800",
		"WantedBy=multi-user.target",
	}
	for _, want := range installed {
		if !strings.Contains(InstalledUnitFile, want) {
			t.Errorf("the installed unit is missing %q", want)
		}
	}
	if strings.Contains(InstalledUnitFile, "/opt/box") {
		t.Errorf("the installed unit belongs to an image without a bundle, so it must not name /opt/box:\n%s", InstalledUnitFile)
	}
}
