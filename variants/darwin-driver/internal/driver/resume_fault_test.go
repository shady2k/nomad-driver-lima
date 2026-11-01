package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
)

type resumeFaultRunner struct {
	base   *fakeCLI
	before func(context.Context, map[string]any, []string) error
	after  func(map[string]any, []string, []byte, error) ([]byte, error)
	stream func(context.Context) error
}

func (r *resumeFaultRunner) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	var p map[string]any
	_ = json.Unmarshal(in, &p)
	if r.before != nil {
		if e := r.before(ctx, p, args); e != nil {
			return nil, e
		}
	}
	b, e := r.base.Run(ctx, in, args...)
	if r.after != nil {
		return r.after(p, args, b, e)
	}
	return b, e
}
func (r *resumeFaultRunner) Stream(ctx context.Context, _ io.Reader, _ io.Writer, _ io.Writer, _ ...string) error {
	return r.stream(ctx)
}

func stoppedResumeFixture(t *testing.T) (*Driver, *fakeCLI, *drivers.TaskConfig, *drivers.TaskConfig, *State, string) {
	t.Helper()
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	old, token := tokenFixture(t)
	old.NodeID = "source-node"
	old.Namespace = "workers"
	os.WriteFile(token, []byte("synthetic-old"), 0600)
	if _, _, e := d.StartTask(old); e != nil {
		t.Fatal(e)
	}
	if e := d.StopTask(old.ID, time.Second, "SIGTERM"); e != nil {
		t.Fatal(e)
	}
	if e := d.DestroyTask(old.ID, false); e != nil {
		t.Fatal(e)
	}
	source, e := d.store.read(lima.Hash([]byte(old.ID)))
	if e != nil {
		t.Fatal(e)
	}
	next, nextToken := tokenFixture(t)
	next.ID = "new-allocation/work/attempt"
	next.AllocID = "00000000-0000-4000-8000-000000000004"
	next.NodeID = old.NodeID
	next.Namespace = old.Namespace
	os.WriteFile(nextToken, []byte("synthetic-new"), 0600)
	var tc TaskConfig
	next.DecodeDriverConfig(&tc)
	tc.ResumeFromKey = source.Key
	tc.ResumeGeneration = 1
	tc.ResumeNodeID = source.NodeID
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000005"
	next.EncodeConcreteDriverConfig(tc)
	f.mu.Lock()
	f.phase = "running"
	f.mu.Unlock()
	return d, f, old, next, source, nextToken
}

func TestResumePartialStartNeverReplaysWorkload(t *testing.T) {
	for _, fault := range []string{"boot-after", "identity", "sync-after", "launch-after", "attach"} {
		t.Run(fault, func(t *testing.T) {
			d, f, _, next, source, _ := stoppedResumeFixture(t)
			var once sync.Once
			runner := &resumeFaultRunner{base: f}
			runner.after = func(p map[string]any, args []string, b []byte, e error) ([]byte, error) {
				match := (fault == "boot-after" && args[0] == "start") || (fault == "identity" && p["action"] == "identity") || (fault == "sync-after" && p["action"] == "sync") || (fault == "launch-after" && p["action"] == "launch")
				injected := false
				if match {
					once.Do(func() { injected = true })
				}
				if injected {
					return nil, fmt.Errorf("injected lost %s response", fault)
				}
				return b, e
			}
			d.client.Runner = runner
			if fault == "attach" {
				next.StderrPath = t.TempDir()
			}
			if _, _, e := d.StartTask(next); e == nil {
				t.Fatal("fault not injected")
			}
			if e := d.StopTask(source.TaskID, time.Second, ""); e == nil {
				t.Fatal("old handle remained owner after partial Start")
			}
			rows, e := d.ledger()
			if e != nil || len(rows) != 1 || rows[0].DiskGiB != source.DiskGiB {
				t.Fatal("partial Start lost/doubled quota", rows, e)
			}
			if fault == "attach" {
				next.StderrPath = filepath.Join(t.TempDir(), "stderr")
				os.WriteFile(next.StderrPath, nil, 0600)
			}
			_, _, e = d.StartTask(next)
			if fault == "sync-after" || fault == "launch-after" {
				if e == nil || !strings.Contains(e.Error(), "unknown") {
					t.Fatal("uncertain launch was accepted", e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			expectedLaunches := 2
			if fault == "sync-after" {
				expectedLaunches = 1
			}
			if f.creates != 1 || f.starts != 2 || f.launches != expectedLaunches || f.deletes != 0 {
				t.Fatalf("replayed effects create=%d start=%d launch=%d delete=%d", f.creates, f.starts, f.launches, f.deletes)
			}
		})
	}
}

func TestResumeChangedDiskOrMachineNeverGetsCredentials(t *testing.T) {
	for _, fault := range []string{"disk", "machine", "old-boot"} {
		t.Run(fault, func(t *testing.T) {
			d, f, _, next, source, _ := stoppedResumeFixture(t)
			f.mu.Lock()
			syncs := f.vaultSyncs
			f.mu.Unlock()
			if fault == "disk" {
				p := filepath.Join(d.config.LimaHome, source.Instance, "disk")
				os.Rename(p, p+".original")
				os.WriteFile(p, []byte("synthetic-disk"), 0600)
			} else {
				d.client.Runner = &resumeFaultRunner{base: f, after: func(p map[string]any, _ []string, b []byte, e error) ([]byte, error) {
					if p["action"] == "identity" {
						machine, boot := "machine", "boot-2"
						if fault == "machine" {
							machine = "replaced-machine"
						} else {
							boot = source.BootID
						}
						b, _ = json.Marshal(map[string]string{"machine_id": machine, "boot_id": boot})
					}
					return b, e
				}}
			}
			if _, _, e := d.StartTask(next); e == nil {
				t.Fatal("changed disk/guest accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.vaultSyncs != syncs || f.launches != 1 || f.creates != 1 {
				t.Fatal("credentials or workload reached replacement disk")
			}
		})
	}
}

func TestConcurrentResumeHasOneWinnerAcrossDriverProcesses(t *testing.T) {
	d, f, _, first, source, _ := stoppedResumeFixture(t)
	secondDriver := setup(t, f, filepath.Dir(d.config.StateDir))
	secondDriver.config.VaultAddress = d.config.VaultAddress
	second, token := tokenFixture(t)
	second.ID = "other-allocation/work/attempt"
	second.AllocID = "00000000-0000-4000-8000-000000000006"
	second.NodeID = first.NodeID
	second.Namespace = first.Namespace
	var tc TaskConfig
	first.DecodeDriverConfig(&tc)
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000007"
	second.EncodeConcreteDriverConfig(tc)
	os.WriteFile(token, []byte("synthetic-other"), 0600)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; _, _, e := d.StartTask(first); results <- e }()
	go func() { <-start; _, _, e := secondDriver.StartTask(second); results <- e }()
	close(start)
	wins := 0
	for range 2 {
		if e := <-results; e == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("resume winners", wins)
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].DiskGiB != source.DiskGiB {
		t.Fatal(rows, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 2 || f.launches != 2 {
		t.Fatal("concurrent resume duplicated effects")
	}
}

func TestOldExecDispatchHoldsFenceAndStopCancelsStreams(t *testing.T) {
	d, f, old, next, _, _ := stoppedResumeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	streamEntered := make(chan struct{})
	var once sync.Once
	runner := &resumeFaultRunner{base: f, before: func(ctx context.Context, _ map[string]any, args []string) error {
		blocked := false
		if args[0] == "list" {
			once.Do(func() { blocked = true })
		}
		if blocked {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}, stream: func(ctx context.Context) error { close(streamEntered); <-ctx.Done(); return ctx.Err() }}
	d.client.Runner = runner
	// A completed source is still queryable until transferred; use a fresh operation
	// context to emulate an exec accepted immediately before cancellation began.
	oldTask, _ := d.get(old.ID)
	oldTask.operationCtx, oldTask.cancelOperations = context.WithCancel(d.ctx)
	result := make(chan error, 1)
	go func() { _, e := d.ExecTask(old.ID, []string{"true"}, time.Second); result <- e }()
	<-entered
	if _, _, e := d.StartTask(next); e == nil {
		t.Fatal("resume crossed old exec dispatch fence")
	}
	close(release)
	if e := <-result; e != nil {
		t.Fatal(e)
	}
	if _, _, e := d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	if _, e := d.ExecTask(old.ID, []string{"true"}, time.Second); e == nil {
		t.Fatal("old exec dispatched after transfer")
	}
	if e := d.SignalTask(old.ID, "SIGTERM"); e == nil {
		t.Fatal("old signal dispatched after transfer")
	}
	streamResult := make(chan error, 1)
	go func() {
		_, e := d.ExecTaskStreaming(context.Background(), next.ID, &drivers.ExecOptions{Command: []string{"watch"}, Stdout: discardWriteCloser{}, Stderr: discardWriteCloser{}})
		streamResult <- e
	}()
	<-streamEntered
	if e := d.StopTask(next.ID, time.Second, "SIGTERM"); e != nil {
		t.Fatal("stop did not drain stream", e)
	}
	if e := <-streamResult; e == nil {
		t.Fatal("stream was not cancelled")
	}
}

type discardWriteCloser struct{}

func (discardWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriteCloser) Close() error                { return nil }

func TestResumeRejectsMalformedQuotaProjectionAndSuccessor(t *testing.T) {
	for _, stage := range []string{"intent", "successor"} {
		for _, field := range []string{"disk", "cpu", "memory", "owner", "node", "namespace", "root-key", "task-key", "version", "phase", "destroyed", "provenance"} {
			if stage == "intent" && field == "provenance" {
				continue
			}
			t.Run(stage+"/"+field, func(t *testing.T) {
				root := t.TempDir()
				os.Chmod(root, 0700)
				store, e := openStore(root)
				if e != nil {
					t.Fatal(e)
				}
				source, next := resumeProjectionStates(store.Owner)
				source.ResumeNext = next
				actual := *next
				target := next
				if stage == "successor" {
					target = &actual
				}
				switch field {
				case "disk":
					target.DiskGiB = 0
				case "cpu":
					target.CPUs = 0
				case "memory":
					target.MemoryMiB = 0
				case "owner":
					target.Owner = strings.Repeat("f", 32)
				case "node":
					target.NodeID = "other"
				case "namespace":
					target.Namespace = "other"
				case "root-key":
					target.InstanceKey = strings.Repeat("f", 64)
				case "task-key":
					target.TaskID = "other"
				case "version":
					target.Version = 99
				case "phase":
					target.Phase = "no_vm_failed"
				case "destroyed":
					target.Destroyed = true
				case "provenance":
					target.CreatorSessionID = "forged"
				}
				if e = store.write(source); e != nil {
					t.Fatal(e)
				}
				if stage == "successor" {
					b, _ := json.Marshal(actual)
					os.WriteFile(filepath.Join(root, actual.Key+".json"), b, 0600)
				}
				driver := &Driver{store: store, config: Config{LimaHome: root}}
				if _, e = driver.ledger(); e == nil {
					t.Fatal("malformed transfer released reservation")
				}
				if driver.checkBudget(TaskConfig{DiskGiB: 1}) == nil {
					t.Fatal("unknown budget admitted new task")
				}
				known, _ := capacityAttributes(driver.config, store)["driver.lima.capacity.known"].GetBool()
				if known {
					t.Fatal("malformed transfer advertised capacity")
				}
			})
		}
	}
}

func TestResumeQuotaOnceAtEachCrashBoundary(t *testing.T) {
	for _, stage := range []string{"before-intent", "intent-only", "materialized", "running"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			os.Chmod(root, 0700)
			store, _ := openStore(root)
			source, next := resumeProjectionStates(store.Owner)
			if stage != "before-intent" {
				source.ResumeNext = next
			}
			if e := store.write(source); e != nil {
				t.Fatal(e)
			}
			if stage == "materialized" || stage == "running" {
				actual := *next
				if stage == "running" {
					actual.Phase = "running"
					actual.MachineID = "machine"
				}
				if e := store.write(&actual); e != nil {
					t.Fatal(e)
				}
			}
			d := &Driver{store: store, config: Config{LimaHome: root, MaxTotalDiskGiB: 20}}
			rows, e := d.ledger()
			if e != nil || len(rows) != 1 || rows[0].DiskGiB != 20 {
				t.Fatal(rows, e)
			}
			if e = d.checkBudget(TaskConfig{DiskGiB: 1}); e == nil {
				t.Fatal("existing disk reservation lost")
			}
			attrs := capacityAttributes(d.config, store)
			known, _ := attrs["driver.lima.capacity.known"].GetBool()
			disk, _ := attrs["driver.lima.capacity.used_disk_gib"].GetInt()
			if !known || disk != 20 {
				t.Fatal("capacity double counted disk", attrs)
			}
		})
	}
}

func TestResumeNeverReusesAncestorSession(t *testing.T) {
	d, f, old, next, _, _ := stoppedResumeFixture(t)
	if _, _, e := d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	if e := d.StopTask(next.ID, time.Second, "SIGTERM"); e != nil {
		t.Fatal(e)
	}
	if e := d.DestroyTask(next.ID, false); e != nil {
		t.Fatal(e)
	}
	third, token := tokenFixture(t)
	third.ID = "third/work/attempt"
	third.AllocID = "00000000-0000-4000-8000-000000000008"
	third.NodeID = old.NodeID
	third.Namespace = old.Namespace
	var tc, oldTC TaskConfig
	next.DecodeDriverConfig(&tc)
	old.DecodeDriverConfig(&oldTC)
	tc.ResumeFromKey = lima.Hash([]byte(next.ID))
	tc.ResumeGeneration = 2
	tc.WorkerSessionID = oldTC.WorkerSessionID
	third.EncodeConcreteDriverConfig(tc)
	os.WriteFile(token, []byte("synthetic-third"), 0600)
	if _, _, e := d.StartTask(third); e == nil || !strings.Contains(e.Error(), "already used") {
		t.Fatal("ancestor session accepted", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 2 || f.launches != 2 {
		t.Fatal("ancestor ACP state may have been loaded")
	}
}

func TestResumeRotationExposureSurvivesFinalizationCrash(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	old, token := tokenFixture(t)
	old.NodeID = "node"
	old.Namespace = "workers"
	os.WriteFile(token, []byte("synthetic-T1"), 0600)
	if _, _, e := d.StartTask(old); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(token, []byte("synthetic-T2"), 0600)
	key := lima.Hash([]byte(old.ID))
	deadline := time.Now().Add(time.Second)
	for {
		s, _ := d.store.read(key)
		hashes, e := d.store.vaultExposures(s)
		if e != nil {
			t.Fatal(e)
		}
		if len(hashes) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotation not delivered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	task, _ := d.get(old.ID)
	h := mustHandle(t, d, old.ID)
	task.vaultMu.Lock()
	task.mu.Lock()
	task.vaultStopped = true
	task.state.VaultFinalizing = true
	if e := d.store.write(task.state); e != nil {
		t.Fatal(e)
	}
	task.mu.Unlock()
	task.vaultMu.Unlock()
	if e := d.Shutdown(context.Background()); e != nil {
		t.Fatal(e)
	}
	recovered := setup(t, f, filepath.Dir(d.config.StateDir))
	recovered.config.VaultAddress = d.config.VaultAddress
	if e := recovered.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	if e := recovered.StopTask(old.ID, time.Second, "SIGTERM"); e != nil {
		t.Fatal(e)
	}
	s, e := recovered.store.read(key)
	if e != nil || s.StopReceipt == nil || len(s.StopReceipt.ExposedTokenSHA256) != 2 {
		t.Fatal("rotation history lost across crash", s, e)
	}
	probe := *s
	probe.ResumedFrom = s.Key
	for _, token := range []string{"synthetic-T1", "synthetic-T2"} {
		if e := recovered.freshResumeToken(&probe, lima.Hash([]byte(token))); e == nil {
			t.Fatal("ancestor token accepted after restart")
		}
	}
}

func TestResumeMalformedReceiptAndCorruptHistoryFailClosed(t *testing.T) {
	for _, field := range []string{"token-hash", "exposure-hash", "exposure-list", "disk", "allocation", "session", "instance", "generation", "future-time", "corrupt-state"} {
		t.Run(field, func(t *testing.T) {
			d, f, _, next, source, _ := stoppedResumeFixture(t)
			switch field {
			case "token-hash":
				source.StopReceipt.TokenSHA256 = "x"
			case "exposure-hash":
				source.StopReceipt.ExposedTokenSHA256 = []string{"x"}
			case "exposure-list":
				source.StopReceipt.ExposedTokenSHA256 = []string{lima.Hash([]byte("unknown"))}
			case "disk":
				source.StopReceipt.Disk.Ino++
			case "allocation":
				source.StopReceipt.Allocation = "other"
			case "session":
				source.StopReceipt.Session = "other"
			case "instance":
				source.StopReceipt.Instance = "other"
			case "generation":
				source.StopReceipt.Generation++
			case "future-time":
				source.StopReceipt.ConfirmedAt = time.Now().Add(time.Hour)
			}
			if field == "corrupt-state" {
				os.WriteFile(filepath.Join(d.store.Root, source.Key+".json"), []byte("{broken"), 0600)
				if e := d.store.write(source); e == nil {
					t.Fatal("corrupt historical state overwritten")
				}
			} else if e := d.store.write(source); e != nil {
				t.Fatal(e)
			}
			if _, _, e := d.StartTask(next); e == nil {
				t.Fatal("malformed stop receipt accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.starts != 1 || f.launches != 1 {
				t.Fatal("malformed receipt caused runtime effects")
			}
		})
	}
}

func TestResumeMissingAncestorExposureAuditIsUnknown(t *testing.T) {
	for _, stage := range []string{"source", "ancestor"} {
		t.Run(stage, func(t *testing.T) {
			d, f, old, next, source, _ := stoppedResumeFixture(t)
			candidate := next
			if stage == "ancestor" {
				if _, _, e := d.StartTask(next); e != nil {
					t.Fatal(e)
				}
				if e := d.StopTask(next.ID, time.Second, "SIGTERM"); e != nil {
					t.Fatal(e)
				}
				if e := d.DestroyTask(next.ID, false); e != nil {
					t.Fatal(e)
				}
				third, token := tokenFixture(t)
				third.ID = "third/work/attempt"
				third.AllocID = "00000000-0000-4000-8000-000000000008"
				third.NodeID = old.NodeID
				third.Namespace = old.Namespace
				var tc TaskConfig
				next.DecodeDriverConfig(&tc)
				tc.ResumeFromKey = lima.Hash([]byte(next.ID))
				tc.ResumeGeneration = 2
				tc.WorkerSessionID = "00000000-0000-4000-8000-000000000009"
				third.EncodeConcreteDriverConfig(tc)
				os.WriteFile(token, []byte("synthetic-old"), 0600)
				candidate = third
			}
			paths, e := filepath.Glob(filepath.Join(d.store.Root, source.Key+".vault-exposure.*.audit"))
			if e != nil || len(paths) != 1 {
				t.Fatal(paths, e)
			}
			if e = os.Remove(paths[0]); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			before := f.starts
			f.mu.Unlock()
			if _, _, e = d.StartTask(candidate); e == nil {
				t.Fatal("missing ancestor exposure audit treated as fresh token")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.starts != before {
				t.Fatal("unknown history booted VM")
			}
		})
	}
}

func TestResumeRotationAfterPreflightCannotExposeAncestorToken(t *testing.T) {
	d, f, _, next, _, token := stoppedResumeFixture(t)
	f.mu.Lock()
	syncs := f.vaultSyncs
	f.mu.Unlock()
	var once sync.Once
	d.client.Runner = &resumeFaultRunner{base: f, before: func(_ context.Context, p map[string]any, _ []string) error {
		if p["action"] == "identity" {
			once.Do(func() { os.WriteFile(token, []byte("synthetic-old"), 0600) })
		}
		return nil
	}}
	if _, _, e := d.StartTask(next); e == nil {
		t.Fatal("ancestor token rotated in after preflight was accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vaultSyncs != syncs || f.launches != 1 {
		t.Fatal("ancestor credential exposed to new generation")
	}
}
