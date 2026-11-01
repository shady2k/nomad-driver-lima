package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	log "github.com/hashicorp/go-hclog"
	ns "github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCLI struct {
	mu                                        sync.Mutex
	home                                      string
	infos                                     map[string]lima.Info
	creates, starts, launches, stops, deletes int
	vaultSyncs, vaultClears                   int
	phase                                     string
	code                                      int
	fault                                     string
	command                                   []string
}

func (f *fakeCLI) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "validate":
		if f.fault == "template-preflight" {
			return nil, fmt.Errorf("runtime cannot read template")
		}
		return nil, nil
	case "--version":
		return []byte("limactl version 2.2.1"), nil
	case "list":
		var b strings.Builder
		for _, v := range f.infos {
			x, _ := json.Marshal(v)
			b.Write(x)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	case "create":
		f.creates++
		var name, owner, key string
		cpu, mem, disk := 0, 0, 0
		for _, a := range args {
			if strings.HasPrefix(a, "--name=") {
				name = strings.TrimPrefix(a, "--name=")
			}
			if strings.HasPrefix(a, "--cpus=") {
				cpu, _ = strconv.Atoi(strings.TrimPrefix(a, "--cpus="))
			}
			if strings.HasPrefix(a, "--memory=") {
				v, _ := strconv.ParseFloat(strings.TrimPrefix(a, "--memory="), 64)
				mem = int(v * 1024)
			}
			if strings.HasPrefix(a, "--disk=") {
				disk, _ = strconv.Atoi(strings.TrimPrefix(a, "--disk="))
			}
			if strings.HasPrefix(a, "--set=.param") {
				m := regexp.MustCompile(`"([a-f0-9]+)"`).FindAllStringSubmatch(a, -1)
				owner = m[0][1]
				key = m[1][1]
			}
		}
		if f.fault == "create-before" {
			return nil, fmt.Errorf("fake uncertain create")
		}
		dir := filepath.Join(f.home, name)
		os.MkdirAll(dir, 0700)
		os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte("fixture config"), 0600)
		os.WriteFile(filepath.Join(dir, "disk"), []byte("synthetic-disk"), 0600)
		f.infos[name] = lima.Info{Name: name, Status: "Stopped", Dir: dir, CPUs: cpu, Memory: int64(mem) * 1048576, Disk: int64(disk) * 1073741824, Param: map[string]string{"nomad_driver_id": owner, "nomad_task_key": key}}
		if f.fault == "create-after" {
			return nil, fmt.Errorf("fake lost create acknowledgement")
		}
		return nil, nil
	case "start":
		name := args[len(args)-1]
		v := f.infos[name]
		v.Status = "Running"
		f.infos[name] = v
		f.starts++
		return nil, nil
	case "stop":
		if f.fault == "stop-no-change" {
			f.stops++
			return nil, nil
		}
		name := args[len(args)-1]
		v := f.infos[name]
		v.Status = "Stopped"
		f.infos[name] = v
		f.stops++
		return nil, nil
	case "delete":
		f.deletes++
		return nil, fmt.Errorf("delete must never be called")
	case "shell":
		if len(in) == 0 {
			return []byte("exec-ok"), nil
		}
		var p map[string]any
		json.Unmarshal(in, &p)
		action := p["action"].(string)
		if action == "sync" {
			f.vaultSyncs++
			return []byte(`{"ok":true}`), nil
		}
		if action == "revoke" {
			if f.fault == "revoke-uncertain" {
				return []byte(`{"ok":false}`), nil
			}
			return []byte(`{"ok":true}`), nil
		}
		if action == "clear" {
			f.vaultClears++
			return []byte(`{"ok":true}`), nil
		}
		if action == "identity" {
			return json.Marshal(map[string]string{"machine_id": "machine", "boot_id": fmt.Sprint("boot-", f.starts)})
		}
		if action == "launch" {
			f.launches++
			if c, ok := p["command"].([]any); ok {
				for _, x := range c {
					f.command = append(f.command, x.(string))
				}
			}
			if f.fault == "launch-after" {
				return nil, fmt.Errorf("fake launch response lost")
			}
		}
		if action == "signal" {
			f.phase = "exited"
			f.code = 143
		}
		if f.fault == "transport" {
			return nil, fmt.Errorf("SSH EOF")
		}
		var result any
		if f.phase == "exited" {
			result = map[string]any{"code": f.code, "signal": 0, "error": ""}
		}
		off := int64(p["stdout_offset"].(float64))
		chunk := []byte{}
		if off == 0 {
			chunk = []byte("guest-output\n")
		}
		v := map[string]any{"phase": f.phase, "result": result, "machine_id": "machine", "boot_id": fmt.Sprint("boot-", f.starts), "stdout": base64.StdEncoding.EncodeToString(chunk), "stderr": "", "stdout_eof": true, "stderr_eof": true, "stdout_offset": off + int64(len(chunk)), "stderr_offset": 0}
		b, _ := json.Marshal(v)
		return b, nil
	}
	return nil, fmt.Errorf("unknown fake CLI")
}
func setup(t *testing.T, f *fakeCLI, root string) *Driver {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	template := filepath.Join(root, "template.yaml")
	os.WriteFile(template, []byte("images: []"), 0600)
	home := filepath.Join("/tmp", "nl-"+lima.Hash([]byte(root))[:8])
	os.MkdirAll(home, 0700)
	t.Cleanup(func() { os.RemoveAll(home) })
	d := New(t.Context(), log.NewNullLogger())
	e := d.configure(Config{LimaBinary: "/usr/bin/true", LimaHome: home, StateDir: filepath.Join(root, "state"), TemplateFile: template, TemplateSHA256: lima.Hash([]byte("images: []")), ExpectedVersion: "2.2.1", MemoryOverheadMiB: 128, DiskFreeFloorMiB: 0})
	if e != nil {
		t.Fatal(e)
	}
	f.home = d.config.LimaHome
	d.client.Runner = f
	d.mhzPerCore = 1000
	d.interval = 5 * time.Millisecond
	t.Cleanup(func() { ctx, c := context.WithTimeout(context.Background(), time.Second); defer c(); d.Shutdown(ctx) })
	return d
}
func fixture(t *testing.T) *drivers.TaskConfig {
	t.Helper()
	root := t.TempDir()
	out := filepath.Join(root, "stdout")
	err := filepath.Join(root, "stderr")
	os.WriteFile(out, nil, 0600)
	os.WriteFile(err, nil, 0600)
	cfg := &drivers.TaskConfig{ID: "allocation/task/attempt", AllocID: "00000000-0000-4000-8000-000000000001", Name: "work", StdoutPath: out, StderrPath: err, Resources: &drivers.Resources{NomadResources: &ns.AllocatedTaskResources{Cpu: ns.AllocatedCpuResources{CpuShares: 1000}, Memory: ns.AllocatedMemoryResources{MemoryMB: 640}}}}
	if e := cfg.EncodeConcreteDriverConfig(TaskConfig{Command: []string{"/bin/sh", "-c", "exit 7"}, CPUs: 1, MemoryMiB: 512, DiskGiB: 1, Architecture: runtime.GOARCH, GuestOS: "linux"}); e != nil {
		t.Fatal(e)
	}
	return cfg
}
func newFake() *fakeCLI { return &fakeCLI{infos: map[string]lima.Info{}, phase: "running", code: 7} }
func TestRealGuestExitLogsAndRetainedDestroy(t *testing.T) {
	f := newFake()
	f.phase = "exited"
	d := setup(t, f, "")
	cfg := fixture(t)
	h, _, e := d.StartTask(cfg)
	if e != nil || h == nil {
		t.Fatal(e)
	}
	ctx, c := context.WithTimeout(t.Context(), time.Second)
	defer c()
	ch, e := d.WaitTask(ctx, cfg.ID)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case r := <-ch:
		if r == nil || r.ExitCode != 7 || r.Successful() {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal("wait timed out")
	}
	b, _ := os.ReadFile(cfg.StdoutPath)
	if !strings.HasSuffix(string(b), "guest-output\n") || !strings.Contains(string(b), `"stage":"vm_boot"`) {
		t.Fatal(string(b))
	}
	if e = d.DestroyTask(cfg.ID, false); e != nil {
		t.Fatal(e)
	}
	if f.deletes != 0 || f.stops != 1 {
		t.Fatal("disk purge or missing stop")
	}
}
func TestRecoverSameInstanceDoesNotReplayWorkload(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.Shutdown(t.Context()); e != nil {
		t.Fatal(e)
	}
	d2 := setup(t, f, root)
	if e = d2.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	creates, launches, starts := f.creates, f.launches, f.starts
	f.mu.Unlock()
	if creates != 1 || launches != 1 || starts != 1 {
		t.Fatalf("duplicates %d %d %d", creates, launches, starts)
	}
	if e = d2.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
}
func TestForeignOwnerAndConfigTamperNeverStopped(t *testing.T) {
	for _, tamper := range []string{"owner", "config"} {
		t.Run(tamper, func(t *testing.T) {
			f := newFake()
			d := setup(t, f, "")
			cfg := fixture(t)
			_, _, e := d.StartTask(cfg)
			if e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			for n, v := range f.infos {
				if tamper == "owner" {
					v.Param["nomad_driver_id"] = "foreign"
					f.infos[n] = v
				} else {
					os.WriteFile(filepath.Join(v.Dir, "lima.yaml"), []byte("changed"), 0600)
				}
			}
			f.mu.Unlock()
			if d.StopTask(cfg.ID, time.Millisecond, "") == nil {
				t.Fatal("accepted foreign instance")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.stops != 0 {
				t.Fatal("stopped foreign VM")
			}
		})
	}
}
func TestAmbiguousCreateOrLaunchNeverBlindlyRetried(t *testing.T) {
	for _, fault := range []string{"create-before", "launch-after"} {
		t.Run(fault, func(t *testing.T) {
			f := newFake()
			f.fault = fault
			d := setup(t, f, "")
			cfg := fixture(t)
			if _, _, e := d.StartTask(cfg); e == nil {
				t.Fatal("expected injected failure")
			}
			f.mu.Lock()
			f.fault = ""
			f.mu.Unlock()
			_, _, _ = d.StartTask(cfg)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 1 || f.launches > 1 {
				t.Fatalf("blind replay create=%d launch=%d", f.creates, f.launches)
			}
		})
	}
}
func TestLostCreateAcknowledgementCanAdoptOnlyOwnedInstance(t *testing.T) {
	f := newFake()
	f.fault = "create-after"
	d := setup(t, f, "")
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e == nil {
		t.Fatal("expected lost acknowledgement")
	}
	f.mu.Lock()
	f.fault = ""
	f.mu.Unlock()
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	if f.creates != 1 || f.launches != 1 {
		t.Fatal("duplicate create")
	}
}
func TestTransportFailureIsNotSuccessAndUnsupportedStatsHonest(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	cfg := fixture(t)
	_, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.fault = "transport"
	f.mu.Unlock()
	ctx, c := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer c()
	ch, _ := d.WaitTask(ctx, cfg.ID)
	if r := <-ch; r != nil {
		t.Fatal("EOF became terminal success")
	}
	if ch, e := d.TaskStats(t.Context(), cfg.ID, time.Second); e == nil || ch != nil {
		t.Fatal("fabricated statistics")
	}
}
func TestReservationAndArchitectureValidationBeforeCreate(t *testing.T) {
	for _, kind := range []string{"cpu", "ram", "arch", "mount"} {
		t.Run(kind, func(t *testing.T) {
			f := newFake()
			d := setup(t, f, "")
			cfg := fixture(t)
			switch kind {
			case "cpu":
				cfg.Resources.NomadResources.Cpu.CpuShares = 1
			case "ram":
				cfg.Resources.NomadResources.Memory.MemoryMB = 512
			case "arch":
				var tc TaskConfig
				cfg.DecodeDriverConfig(&tc)
				tc.Architecture = "unsupported"
				cfg.EncodeConcreteDriverConfig(tc)
			case "mount":
				cfg.User = "root"
			}
			if _, _, e := d.StartTask(cfg); e == nil {
				t.Fatal("invalid reservation/host option accepted")
			}
			if f.creates != 0 {
				t.Fatal("create before validation")
			}
		})
	}
}
