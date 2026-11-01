package driver

import (
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestQuotaCountsUnknownAndRetainedDisks(t *testing.T) {
	d := setup(t, newFake(), "")
	d.config.MaxTotalDiskGiB = 40
	s := &State{Version: 1, Key: strings.Repeat("a", 64), Owner: d.store.Owner, DiskGiB: 30, Destroyed: true, Phase: "destroyed_disk_retained"}
	if e := d.store.write(s); e != nil {
		t.Fatal(e)
	}
	if d.checkBudget(TaskConfig{CPUs: 1, MemoryMiB: 512, DiskGiB: 20}) == nil {
		t.Fatal("retained disk quota bypass")
	}
	d.config.MaxTotalDiskGiB = 0
	d.config.MaxVMs = 1
	s.Destroyed = false
	s.Phase = "creating"
	d.store.write(s)
	if d.checkBudget(TaskConfig{CPUs: 1, MemoryMiB: 512, DiskGiB: 1}) == nil {
		t.Fatal("uncertain VM bypass")
	}
}
func TestCreatorNameAndDuplicateMachineID(t *testing.T) {
	tc := TaskConfig{WorkerSessionID: "00000000-0000-4000-8000-000000000003", CreatorAgentID: "00000000-0000-4000-8000-000000000001", CreatorSessionID: "00000000-0000-4000-8000-000000000002", Purpose: "tests", Project: "project"}
	if e := validateCreator(tc); e != nil {
		t.Fatal(e)
	}
	name := instanceName(tc, strings.Repeat("a", 64))
	if name != "project-tests-aaaaaaaaaaaa" {
		t.Fatal(name)
	}
	d := setup(t, newFake(), "")
	a := &State{Version: 1, Key: strings.Repeat("a", 64), Owner: d.store.Owner, MachineID: "same", TemplateID: "agent-base"}
	d.store.write(a)
	b := &State{Key: strings.Repeat("b", 64), TemplateID: "agent-base"}
	if d.uniqueIdentity(b, "same") == nil {
		t.Fatal("duplicate identity admitted")
	}
	if e := d.uniqueIdentity(b, "fresh"); e != nil {
		t.Fatal(e)
	}
}
func TestNativeStagesEvenBeforeGuestBoot(t *testing.T) {
	f := newFake()
	f.fault = "create-before"
	d := setup(t, f, "")
	cfg := fixture(t)
	_, _, e := d.StartTask(cfg)
	if e == nil {
		t.Fatal("fixture did not fail")
	}
	raw, _ := os.ReadFile(cfg.StdoutPath)
	if !strings.Contains(string(raw), `"stage":"vm_create"`) {
		t.Fatal(string(raw))
	}
	if strings.Contains(string(raw), "https:") {
		t.Fatal("URL leaked")
	}
	files, _ := filepath.Glob(filepath.Join(d.store.Root, "*.json"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	_ = lima.Hash(raw)
}

func TestCatalogMissingDiskBudgetRejectsBeforeAnyCreate(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.CatalogFile = "/operator/pending-catalog.json"
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e == nil || !strings.Contains(e.Error(), "disk budget pending") {
		t.Fatal(e)
	}
	if f.creates+f.starts+f.launches != 0 {
		t.Fatal("unbudgeted request reached runtime")
	}
}

func TestKnownPreCreateFailureDoesNotLeakQuotaButUncertainCreateDoes(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.MaxVMs = 1
	d.config.DiskFreeFloorMiB = 1 << 50
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e == nil {
		t.Fatal("expected physical-space rejection")
	}
	s, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || s.Phase != "no_vm_failed" {
		t.Fatal(s, e)
	}
	if f.creates != 0 {
		t.Fatal("unexpected create")
	}
	d.config.DiskFreeFloorMiB = 0
	cfg.ID += "-new-attempt"
	if _, _, e = d.StartTask(cfg); e != nil {
		t.Fatal("known no-VM ledger leaked quota", e)
	}
}
func TestStopAcknowledgementWithoutStoppedStateDoesNotReleaseQuota(t *testing.T) {
	f := newFake()
	f.phase = "exited"
	d := setup(t, f, "")
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.fault = "stop-no-change"
	f.mu.Unlock()
	if e := d.StopTask(cfg.ID, time.Millisecond, ""); e == nil {
		t.Fatal("unconfirmed stop accepted")
	}
	s, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil {
		t.Fatal(e)
	}
	if s.Phase == "stopped" || s.Destroyed {
		t.Fatal("released unconfirmed VM")
	}
}

func TestNativeFIFOStaysOpenAcrossStartupStagesAndGuestOutput(t *testing.T) {
	f := newFake()
	f.phase = "exited"
	d := setup(t, f, "")
	cfg := fixture(t)
	pipe := filepath.Join(t.TempDir(), "stdout.fifo")
	if e := syscall.Mkfifo(pipe, 0600); e != nil {
		t.Fatal(e)
	}
	cfg.StdoutPath = pipe
	logs := make(chan []byte, 1)
	go func() {
		file, e := os.Open(pipe)
		if e != nil {
			logs <- nil
			return
		}
		defer file.Close()
		b, _ := io.ReadAll(file)
		logs <- b
	}()
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	select {
	case b := <-logs:
		if !strings.Contains(string(b), `"stage":"workload_launch"`) || !strings.Contains(string(b), "guest-output") {
			t.Fatal("logmon saw premature FIFO EOF", string(b))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FIFO stream did not terminate")
	}
}

func TestTemplateRuntimePreflightFailureNeverDispatchesCreate(t *testing.T) {
	f := newFake()
	f.fault = "template-preflight"
	d := setup(t, f, "")
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e == nil || !strings.Contains(e.Error(), "preflight failed before create") {
		t.Fatal(e)
	}
	if f.creates+f.starts+f.launches != 0 {
		t.Fatal("preflight dispatched runtime")
	}
	s, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || s.Phase != "no_vm_failed" {
		t.Fatal(s, e)
	}
	raw, _ := os.ReadFile(cfg.StdoutPath)
	if strings.Contains(string(raw), `"stage":"vm_create"`) {
		t.Fatal("false create stage")
	}
}
