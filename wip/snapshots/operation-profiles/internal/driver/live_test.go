package driver

// Opt-in only. This exercises new driver code, not a repeated Docker/TUN PoC.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	log "github.com/hashicorp/go-hclog"
	ns "github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func liveDriver(t *testing.T) *Driver {
	t.Helper()
	p := os.Getenv("NOMAD_LIMA_LIVE_CONFIG")
	if p == "" {
		t.Skip("explicit isolated Lima operator configuration required")
	}
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var c Config
	if e = json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	d := New(context.Background(), log.NewNullLogger())
	if e = d.configure(c); e != nil {
		t.Fatal(e)
	}
	d.mhzPerCore = 1 // Direct SDK harness uses oversized synthetic reservation, not scheduler admission.
	d.interval = 250 * time.Millisecond
	return d
}
func TestLiveChild(t *testing.T) {
	if os.Getenv("NOMAD_LIMA_LIVE_CHILD") != "1" {
		t.Skip("parent only")
	}
	d := liveDriver(t)
	root := os.Getenv("NOMAD_LIMA_LIVE_ROOT")
	id := os.Getenv("NOMAD_LIMA_LIVE_ID")
	cfg := &drivers.TaskConfig{ID: id, AllocID: "00000000-0000-4000-8000-000000000001", Name: "synthetic", StdoutPath: filepath.Join(root, "stdout"), StderrPath: filepath.Join(root, "stderr"), Resources: &drivers.Resources{NomadResources: &ns.AllocatedTaskResources{Cpu: ns.AllocatedCpuResources{CpuShares: 100000}, Memory: ns.AllocatedMemoryResources{MemoryMB: 2048}}}}
	os.WriteFile(cfg.StdoutPath, nil, 0600)
	os.WriteFile(cfg.StderrPath, nil, 0600)
	cmd := `import pathlib,time,sys; p=pathlib.Path('/tmp/lima-driver-counter'); p.write_text(str(int(p.read_text())+1) if p.exists() else '1'); print('driver-workload-start',flush=True); time.sleep(int(sys.argv[1])); pathlib.Path('/tmp/lima-driver-result.txt').write_text('synthetic-driver-result\n'); print('driver-workload-finish',flush=True); sys.exit(7)`
	duration := "20"
	if os.Getenv("NOMAD_LIMA_MAC_ACCEPTANCE") == "1" {
		duration = "45"
	}
	if e := cfg.EncodeConcreteDriverConfig(TaskConfig{Command: []string{"/usr/bin/python3", "-c", cmd, duration}, CPUs: 1, MemoryMiB: 1024, DiskGiB: 8, Architecture: runtime.GOARCH, GuestOS: "linux"}); e != nil {
		t.Fatal(e)
	}
	rawConfig, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(root, "task-config.json"), rawConfig, 0600)
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(h)
	if e = os.WriteFile(filepath.Join(root, "handle.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	for {
		time.Sleep(time.Second)
	}
}
func TestLiveCrashRecovery(t *testing.T) {
	if os.Getenv("NOMAD_LIMA_LIVE_CONFIG") == "" {
		t.Skip("explicit isolated upstream Lima required")
	}
	root := os.Getenv("NOMAD_LIMA_LIVE_ROOT")
	if root == "" {
		t.Fatal("private test root required")
	}
	os.MkdirAll(root, 0700)
	id := "live/" + fmt.Sprint(time.Now().UnixNano())
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	child := exec.Command(exe, "-test.run=^TestLiveChild$", "-test.v")
	child.Env = append(os.Environ(), "NOMAD_LIMA_LIVE_CHILD=1", "NOMAD_LIMA_LIVE_ID="+id)
	logfile, e := os.OpenFile(filepath.Join(root, "child.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer logfile.Close()
	child.Stdout = logfile
	child.Stderr = logfile
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	childWaited := false
	defer func() {
		if !childWaited {
			_ = child.Process.Kill()
			<-childDone
		}
	}()

	deadline := time.Now().Add(5 * time.Minute)
	var h drivers.TaskHandle
	for {
		select {
		case err := <-childDone:
			childWaited = true
			t.Fatalf("driver child failed before handle: %v; inspect protected child.log/control diagnostics", err)
		default:
		}
		b, e := os.ReadFile(filepath.Join(root, "handle.json"))
		if e == nil && json.Unmarshal(b, &h) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child start timeout; inspect isolated ledger/log, no replay")
		}
		time.Sleep(time.Second)
	}
	// Kill only this driver test process. Upstream Lima VM/guest process must survive.
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	<-childDone
	childWaited = true
	d := liveDriver(t)
	defer d.Shutdown(context.Background())
	var binding map[string]string
	if e := h.GetDriverState(&binding); e != nil {
		t.Fatal(e)
	}
	prior, e := d.store.read(binding["key"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if _, e := d.owned(ctx, prior); e == nil {
			_ = d.client.Stop(ctx, prior.Instance)
		}
	})
	if e = d.RecoverTask(&h); e != nil {
		t.Fatal(e)
	}
	task, e := d.get(id)
	if e != nil {
		t.Fatal(e)
	}
	task.mu.Lock()
	state := *task.state
	task.mu.Unlock()
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if _, e := d.owned(ctx, &state); e == nil {
			_ = d.client.Stop(ctx, state.Instance)
		}
	})
	r, e := d.ExecTask(id, []string{"cat", "/tmp/lima-driver-counter"}, 15*time.Second)
	if e != nil || strings.TrimSpace(string(r.Stdout)) != "1" {
		t.Fatalf("workload replay/missing: %v", e)
	}
	if os.Getenv("NOMAD_LIMA_MAC_ACCEPTANCE") == "1" {
		exercisePTYStats(t, d, id, root)
	}
	ctx, c := context.WithTimeout(context.Background(), 60*time.Second)
	defer c()
	ch, e := d.WaitTask(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case result := <-ch:
		if result == nil || result.ExitCode != 7 || result.Err != nil {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal("guest result wait timeout")
	}
	r, e = d.ExecTask(id, []string{"cat", "/tmp/lima-driver-result.txt"}, 15*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(r.Stdout)
	if e = d.DestroyTask(id, false); e != nil {
		t.Fatal(e)
	}
	v, e := d.client.Get(ctx, state.Instance)
	if e != nil || v.Status != "Stopped" {
		t.Fatal(v, e)
	}
	stdout, e := os.ReadFile(filepath.Join(root, "stdout"))
	if e != nil || !strings.Contains(string(stdout), "driver-workload-finish") {
		t.Fatal("missing recovered log", e)
	}
	receipt := map[string]any{"instance": state.Instance, "guest_machine_id": state.MachineID, "guest_boot_id": state.BootID, "workload_launch_count": 1, "real_guest_exit_code": 7, "driver_sigkill_tested": true, "recovered_same_vm": true, "result_sha256": hex.EncodeToString(sum[:]), "final_status": v.Status, "disk_purged": false, "model_calls": 0, "scope": "real Lima direct SDK harness; not a production Nomad deployment"}
	b, _ := json.MarshalIndent(receipt, "", "  ")
	os.WriteFile(filepath.Join(root, "acceptance.json"), b, 0600)
	t.Log(string(b))
	_ = lima.ErrNotFound
}

func TestLivePTYStatsRuntimeUser(t *testing.T) {
	d := liveDriver(t)
	defer d.Shutdown(context.Background())
	root := os.Getenv("NOMAD_LIMA_LIVE_ROOT")
	if root == "" {
		t.Fatal("private root required")
	}
	if os.Geteuid() != 0 || d.config.RuntimeUser == "" {
		t.Skip("explicit isolated root runtime-user harness only")
	}
	cfg := &drivers.TaskConfig{ID: "pty-stats/" + fmt.Sprint(time.Now().UnixNano()), AllocID: "00000000-0000-4000-8000-000000000002", Name: "synthetic", StdoutPath: filepath.Join(root, "stdout"), StderrPath: filepath.Join(root, "stderr"), Resources: &drivers.Resources{NomadResources: &ns.AllocatedTaskResources{Cpu: ns.AllocatedCpuResources{CpuShares: 100000}, Memory: ns.AllocatedMemoryResources{MemoryMB: 2048}}}}
	os.WriteFile(cfg.StdoutPath, nil, 0600)
	os.WriteFile(cfg.StderrPath, nil, 0600)
	if e := cfg.EncodeConcreteDriverConfig(TaskConfig{Command: []string{"/bin/sleep", "120"}, CPUs: 1, MemoryMiB: 1024, DiskGiB: 8, Architecture: runtime.GOARCH, GuestOS: "linux"}); e != nil {
		t.Fatal(e)
	}
	_, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	task, _ := d.get(cfg.ID)
	task.mu.Lock()
	state := *task.state
	task.mu.Unlock()
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if _, e := d.owned(ctx, &state); e == nil {
			_ = d.client.Stop(ctx, state.Instance)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stats, e := d.TaskStats(ctx, cfg.ID, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	var rss uint64
	select {
	case v := <-stats:
		if v == nil || v.ResourceUsage.MemoryStats.RSS == 0 {
			t.Fatal("missing host RSS")
		}
		rss = v.ResourceUsage.MemoryStats.RSS
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var out bytes.Buffer
	resize := make(chan drivers.TerminalSize, 1)
	resize <- drivers.TerminalSize{Height: 37, Width: 101}
	result, e := d.ExecTaskStreaming(ctx, cfg.ID, &drivers.ExecOptions{Command: []string{"/bin/sh", "-c", "test -t 0 && cat >/dev/null && sleep 1 && stty size && printf pty-complete; exit 7"}, Tty: true, Stdin: io.NopCloser(bytes.NewReader(nil)), Stdout: nopWriteCloser{&out}, Stderr: nopWriteCloser{io.Discard}, ResizeCh: resize})
	close(resize)
	if e != nil || result == nil || result.ExitCode != 7 || !strings.Contains(out.String(), "37 101") || !strings.Contains(out.String(), "pty-complete") {
		t.Fatalf("PTY/resize/exit: %v %v %q", result, e, out.String())
	}
	if e = d.StopTask(cfg.ID, 10*time.Second, "SIGTERM"); e != nil {
		t.Fatal(e)
	}
	if e = d.DestroyTask(cfg.ID, false); e != nil {
		t.Fatal(e)
	}
	v, e := d.client.Get(ctx, state.Instance)
	if e != nil || v.Status != "Stopped" {
		t.Fatal(v, e)
	}
	receipt := map[string]any{"instance": state.Instance, "root_driver": true, "runtime_user": d.config.RuntimeUser, "host_vm_rss_bytes": rss, "pty_resize": "37x101", "exec_exit_code": 7, "final_status": v.Status, "disk_purged": false, "model_calls": 0}
	b, _ := json.MarshalIndent(receipt, "", "  ")
	os.WriteFile(filepath.Join(root, "acceptance.json"), b, 0600)
	t.Log(string(b))
}

type nopWriteCloser struct{ io.Writer }

func (n nopWriteCloser) Close() error { return nil }

type readinessWriter struct {
	bytes.Buffer
	once  sync.Once
	ready chan struct{}
}

func (w *readinessWriter) Write(p []byte) (int, error) {
	n, e := w.Buffer.Write(p)
	if strings.Contains(w.Buffer.String(), "interrupt-ready") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, e
}
func exercisePTYStats(t *testing.T, d *Driver, id, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	task, e := d.get(id)
	if e != nil {
		t.Fatal(e)
	}
	task.mu.Lock()
	s := *task.state
	task.mu.Unlock()
	info, e := d.owned(ctx, &s)
	if e != nil {
		t.Fatal(e)
	}
	stats, e := d.TaskStats(ctx, id, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	var rss uint64
	select {
	case v := <-stats:
		if v == nil || v.ResourceUsage.MemoryStats.RSS == 0 {
			t.Fatal("missing real VZ RSS")
		}
		rss = v.ResourceUsage.MemoryStats.RSS
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var output bytes.Buffer
	resize := make(chan drivers.TerminalSize, 1)
	resize <- drivers.TerminalSize{Height: 37, Width: 101}
	result, e := d.ExecTaskStreaming(ctx, id, &drivers.ExecOptions{Command: []string{"/bin/sh", "-c", "test -t 0 && cat >/dev/null && sleep 1 && stty size && printf pty-complete; exit 7"}, Tty: true, Stdin: io.NopCloser(bytes.NewReader(nil)), Stdout: nopWriteCloser{&output}, Stderr: nopWriteCloser{io.Discard}, ResizeCh: resize})
	close(resize)
	if e != nil || result == nil || result.ExitCode != 7 || !strings.Contains(output.String(), "37 101") || !strings.Contains(output.String(), "pty-complete") {
		t.Fatalf("Mac PTY/EOF/resize: %v %v %q", result, e, output.String())
	}
	input, writer := io.Pipe()
	out := &readinessWriter{ready: make(chan struct{})}
	go func() {
		defer writer.Close()
		select {
		case <-out.ready:
			_, _ = writer.Write([]byte{3})
		case <-ctx.Done():
		}
	}()
	interrupted, e := d.ExecTaskStreaming(ctx, id, &drivers.ExecOptions{Command: []string{"/bin/sh", "-c", "printf 'interrupt-ready\n'; exec sleep 30"}, Tty: true, Stdin: input, Stdout: nopWriteCloser{out}, Stderr: nopWriteCloser{io.Discard}})
	if e != nil || interrupted == nil || interrupted.ExitCode != 130 {
		t.Fatalf("PTY Ctrl-C: %v %v %q", interrupted, e, out.String())
	}
	receipt := map[string]any{"vm_type": info.VMType, "driver_pid": info.DriverPID, "host_agent_pid": info.HostAgentPID, "rss_bytes": rss, "scope": "host VM process, may include VZ hostagent; not guest workload", "pty_resize": "37x101", "stdin_eof": true, "pty_exit": 7, "ctrl_c_exit": 130}
	b, _ := json.MarshalIndent(receipt, "", "  ")
	os.WriteFile(filepath.Join(root, "pty-stats.json"), b, 0600)
	t.Log(string(b))
}
