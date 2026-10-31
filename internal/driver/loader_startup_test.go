package driver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/numalib"
	"github.com/hashicorp/nomad/helper/pluginutils/loader"
	nomadconfig "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
)

// Real external plugin process and the exact Nomad loader, not a direct mock call.
// CLI is a fixture that permits version/list only. No VM can be created here.
func TestExternalPluginLoaderTwoStageConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run isolated loader test as existing unprivileged runtime user")
	}
	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins")
	if e := os.Mkdir(pluginDir, 0700); e != nil {
		t.Fatal(e)
	}
	binary := filepath.Join(pluginDir, "nomad-driver-lima")
	if source := os.Getenv("NOMAD_LIMA_TEST_PLUGIN_BINARY"); source != "" {
		b, e := os.ReadFile(source)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(binary, b, 0700); e != nil {
			t.Fatal(e)
		}
	} else {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-trimpath", "-o", binary, "../../cmd/nomad-driver-lima")
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("isolated plugin build: %v %s", e, b)
		}
	}
	home, e := os.MkdirTemp("/tmp", "ndl-loader-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(home)
	template := filepath.Join(root, "template.yaml")
	content := []byte("plain: true\nmounts: []\n")
	if e = os.WriteFile(template, content, 0600); e != nil {
		t.Fatal(e)
	}
	fake := filepath.Join(root, "fake-limactl")
	calls := filepath.Join(root, "cli-calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + calls + "'\ncase \"$1\" in --version) echo 'limactl version 2.2.1';; list) exit 0;; *) exit 88;; esac\n"
	if e = os.WriteFile(fake, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	cfg := Config{LimaBinary: fake, LimaHome: home, StateDir: filepath.Join(root, "state"), TemplateFile: template, TemplateSHA256: lima.Hash(content), ExpectedVersion: "2.2.1", MemoryOverheadMiB: 128, DiskFreeFloorMiB: 0}
	values := map[string]interface{}{"limactl": cfg.LimaBinary, "lima_home": cfg.LimaHome, "state_dir": cfg.StateDir, "template_file": cfg.TemplateFile, "template_sha256": cfg.TemplateSHA256, "expected_version": cfg.ExpectedVersion, "memory_overhead_mib": 128, "disk_free_floor_mib": 0}
	l, e := loader.NewPluginLoader(&loader.PluginLoaderConfig{Logger: log.NewNullLogger(), PluginDir: pluginDir, Configs: []*nomadconfig.PluginConfig{{Name: "nomad-driver-lima", Config: values}}, SupportedVersions: map[string][]string{base.PluginTypeDriver: {"0.1.0"}}})
	if e != nil {
		t.Fatalf("real loader early validation failed: %v", e)
	}
	// Explicit non-nil AgentConfig with absent topology matches the failing startup.
	instance, e := l.Dispense("lima", base.PluginTypeDriver, &base.AgentConfig{}, log.NewNullLogger())
	if e != nil {
		t.Fatal(e)
	}
	defer instance.Kill()
	client := instance.Plugin().(drivers.DriverPlugin)
	check := func(ready bool, perCore int64) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		ch, e := client.Fingerprint(ctx)
		if e != nil {
			t.Fatal(e)
		}
		select {
		case fp := <-ch:
			if fp == nil {
				t.Fatal("missing fingerprint")
			}
			v, ok := fp.Attributes["driver.lima.compute.ready"].GetBool()
			n, _ := fp.Attributes["driver.lima.cpu.nomad_compute_per_core"].GetInt()
			if !ok || v != ready || n != perCore {
				t.Fatalf("compute fingerprint: ready=%v perCore=%d", v, n)
			}
			if ready && fp.Health != drivers.HealthStateHealthy {
				t.Fatal(fp.HealthDescription)
			}
			if !ready && fp.Health != drivers.HealthStateUnhealthy {
				t.Fatal("pending topology advertised healthy")
			}
		case <-ctx.Done():
			t.Fatal("fingerprint timeout")
		}
	}
	check(false, 0)
	task := fixture(t)
	h, _, e := client.StartTask(task)
	if e == nil || h != nil || !strings.Contains(e.Error(), "compute/reservations unavailable") {
		t.Fatalf("pending topology admitted task or wrong error: %v", e)
	}
	var encoded []byte
	if e = base.MsgPackEncode(&encoded, &cfg); e != nil {
		t.Fatal(e)
	}
	top := &numalib.Topology{Cores: []numalib.Core{{BaseSpeed: 2800}}}
	top.SetNodesFrom([]uint8{0})
	valid := &base.AgentConfig{Driver: &base.ClientDriverConfig{Topology: top}}
	if e = client.SetConfig(&base.Config{PluginConfig: encoded, AgentConfig: valid, ApiVersion: "0.1.0"}); e != nil {
		t.Fatalf("later real topology rejected: %v", e)
	}
	check(true, 2800)
	// Real capacity is now used: fixture's 1000 CPU reservation cannot cover 2800.
	h, _, e = client.StartTask(task)
	if e == nil || h != nil || !strings.Contains(e.Error(), "reservation does not cover") {
		t.Fatalf("real capacity not enforced: %v", e)
	}
	// Losing topology later must close admission, not retain an invented/stale value.
	if e = client.SetConfig(&base.Config{PluginConfig: encoded, AgentConfig: &base.AgentConfig{Driver: &base.ClientDriverConfig{}}, ApiVersion: "0.1.0"}); e != nil {
		t.Fatal(e)
	}
	check(false, 0)
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "create") {
		t.Fatal("test attempted VM create")
	}
}

func TestPendingComputeDoesNotHideInvalidRuntimeConfig(t *testing.T) {
	d := New(context.Background(), log.NewNullLogger())
	defer d.Shutdown(context.Background())
	if e := d.SetConfig(nil); e == nil {
		t.Fatal("nil plugin config accepted")
	}
	var encoded []byte
	if e := base.MsgPackEncode(&encoded, &Config{}); e != nil {
		t.Fatal(e)
	}
	if e := d.SetConfig(&base.Config{PluginConfig: encoded, AgentConfig: &base.AgentConfig{}}); e == nil {
		t.Fatal("empty topology suppressed invalid runtime config")
	}
	if n, _ := d.admissionCompute(); n != 0 {
		t.Fatal("fabricated compute after invalid config")
	}
}
