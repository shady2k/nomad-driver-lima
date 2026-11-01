package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type finishCLI struct {
	base           *fakeCLI
	uncertain      bool
	uncertainAfter bool
}

func (f *finishCLI) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	if args[0] != "delete" {
		return f.base.Run(ctx, in, args...)
	}
	f.base.mu.Lock()
	defer f.base.mu.Unlock()
	f.base.deletes++
	if f.uncertain {
		return nil, fmt.Errorf("synthetic delete outcome unknown")
	}
	name := args[len(args)-1]
	delete(f.base.infos, name)
	if e := os.RemoveAll(filepath.Join(f.base.home, name)); e != nil {
		return nil, e
	}
	if f.uncertainAfter {
		return nil, fmt.Errorf("response lost after delete")
	}
	return nil, nil
}
func finishFixture(t *testing.T) (*Driver, *fakeCLI, *finishCLI, *drivers.TaskConfig, FinishRequest) {
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, token := tokenFixture(t)
	cfg.NodeID = "00000000-0000-4000-8000-000000000045"
	cfg.Namespace = "workers"
	var tc TaskConfig
	cfg.DecodeDriverConfig(&tc)
	tc.CreatorAgentID = "00000000-0000-4000-8000-000000000041"
	tc.CreatorSessionID = "native-finish-source"
	cfg.EncodeConcreteDriverConfig(tc)
	gen := "00000000-0000-4000-8000-000000000042"
	cfg.Env = map[string]string{"NOMAD_META_creator_generation": gen}
	os.WriteFile(token, []byte("synthetic-task-token"), 0600)
	cli := &finishCLI{base: f}
	d.client.Runner = cli
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	req := FinishRequest{Action: "arm", Allocation: cfg.AllocID, Session: tc.WorkerSessionID, CreatorAgent: tc.CreatorAgentID, CreatorSession: tc.CreatorSessionID, CreatorGeneration: gen, Request: "00000000-0000-4000-8000-000000000043", Snapshot: "00000000-0000-4000-8000-000000000044", ManifestSHA: strings.Repeat("a", 64), ReceiptSHA: strings.Repeat("b", 64), Backend: "s3", ActivityRevision: 1}
	return d, f, cli, cfg, req
}
func finishCommand(r FinishRequest) []string {
	raw, _ := json.Marshal(r)
	return []string{finalizeControl, string(raw)}
}
func TestS3FinalizationArmsOnlyAuthenticatedControlThenStopsPurgesReleasesQuota(t *testing.T) {
	d, f, _, cfg, r := finishFixture(t)
	pre := r
	pre.Action = "preflight"
	if _, e := d.ExecTask(cfg.ID, finishCommand(pre), time.Second); e != nil {
		t.Fatal(e)
	}
	tsk, _ := d.get(cfg.ID)
	tsk.mu.Lock()
	armed := tsk.state.Finalization != nil
	tsk.mu.Unlock()
	if armed {
		t.Fatal("preflight armed destructive intent")
	}
	if _, e := d.ExecTask(cfg.ID, finishCommand(r), time.Second); e != nil {
		t.Fatal(e)
	}
	if _, e := d.ExecTask(cfg.ID, finishCommand(r), time.Second); e != nil {
		t.Fatal("same receipt must be idempotent", e)
	}
	if _, e := d.ExecTask(cfg.ID, []string{"/bin/true"}, time.Second); e == nil {
		t.Fatal("activity after finalization fence accepted")
	}
	changed := r
	changed.ManifestSHA = strings.Repeat("c", 64)
	if _, e := d.ExecTask(cfg.ID, finishCommand(changed), time.Second); e == nil {
		t.Fatal("stale candidate changed")
	}
	f.mu.Lock()
	deletes := f.deletes
	f.mu.Unlock()
	if deletes != 0 {
		t.Fatal("deleted before stop")
	}
	if e := d.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	tsk.mu.Lock()
	s := *tsk.state
	tsk.mu.Unlock()
	if s.Phase != "destroyed_disk_purged" || s.DiskGiB != 0 || s.PurgedDiskGiB != 1 || s.Version != 4 {
		t.Fatalf("bad completed state %+v", s)
	}
	if ok, e := d.store.purged(&s); e != nil || !ok {
		t.Fatal("no durable purge proof", e)
	}
	if e := d.DestroyTask(cfg.ID, false); e != nil {
		t.Fatal("Nomad GC after completed purge must be idempotent", e)
	}
	if f.deletes != 1 {
		t.Fatal("delete repeated")
	}
}
func TestS3FinalizationWrongReceiptGenerationOrUnknownDeleteRetains(t *testing.T) {
	for _, kind := range []string{"generation", "backend", "owner", "unknown-delete"} {
		t.Run(kind, func(t *testing.T) {
			d, f, cli, cfg, r := finishFixture(t)
			switch kind {
			case "generation":
				r.CreatorGeneration = "00000000-0000-4000-8000-000000000099"
			case "backend":
				r.Backend = "coordinator-local"
			case "owner":
				r.CreatorAgent = "00000000-0000-4000-8000-000000000099"
			case "unknown-delete":
				cli.uncertain = true
			}
			_, e := d.ExecTask(cfg.ID, finishCommand(r), time.Second)
			if kind != "unknown-delete" {
				if e == nil {
					t.Fatal("invalid receipt accepted")
				}
				if f.deletes != 0 {
					t.Fatal("deleted invalid source")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = d.StopTask(cfg.ID, time.Second, ""); e == nil {
				t.Fatal("unknown delete reported success")
			}
			task, _ := d.get(cfg.ID)
			task.mu.Lock()
			s := *task.state
			task.mu.Unlock()
			if s.DiskGiB != 1 || s.Phase != "purge_pending" {
				t.Fatal("unknown deletion released reservation")
			}
			if ok, e := d.store.purged(&s); e != nil || ok {
				t.Fatal("unknown deletion fabricated absence", e)
			}
		})
	}
}

func TestFinalizationNativeStreamingAndUnknownDeleteReconcileNoRepeat(t *testing.T) {
	d, f, cli, cfg, r := finishFixture(t)
	var out bytes.Buffer
	result, e := d.ExecTaskStreaming(context.Background(), cfg.ID, &drivers.ExecOptions{Command: finishCommand(r), Stdout: nopWriteCloser{&out}, Stderr: nopWriteCloser{io.Discard}})
	if e != nil || result.ExitCode != 0 || !strings.Contains(out.String(), "finish-v1") {
		t.Fatal("native alloc exec control failed", e)
	}
	cli.uncertainAfter = true
	if e = d.StopTask(cfg.ID, time.Second, ""); e == nil {
		t.Fatal("lost delete response reported confirmed")
	}
	if e = d.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal("same-task absence reconciliation failed", e)
	}
	f.mu.Lock()
	deletes := f.deletes
	f.mu.Unlock()
	if deletes != 1 {
		t.Fatal("uncertain delete blindly replayed")
	}
	if e = d.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal("completed stop retry failed", e)
	}
}

func TestFinalizationPurgeTerminalRecoveryNoVMOrCredentialRecreation(t *testing.T) {
	d, f, cli, cfg, r := finishFixture(t)
	task, _ := d.get(cfg.ID)
	h, e := d.handle(task)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.ExecTask(cfg.ID, finishCommand(r), time.Second); e != nil {
		t.Fatal(e)
	}
	if e = d.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = d.Shutdown(ctx); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	creates, syncs := f.creates, f.vaultSyncs
	f.mu.Unlock()
	next := New(t.Context(), log.NewNullLogger())
	if e = next.configure(d.config); e != nil {
		t.Fatal(e)
	}
	next.client.Runner = cli
	if e = next.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	if e = next.DestroyTask(cfg.ID, false); e != nil {
		t.Fatal(e)
	}
	done, e := next.WaitTask(t.Context(), cfg.ID)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal recovery not done")
	}
	f.mu.Lock()
	newCreates, newSyncs := f.creates, f.vaultSyncs
	f.mu.Unlock()
	if newCreates != creates || newSyncs != syncs {
		t.Fatal("recreated VM or credentials after purge")
	}
	if e = next.Shutdown(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestFinalizationDoesNotCancelOrArmOverActiveCommand(t *testing.T) {
	d, _, _, cfg, r := finishFixture(t)
	task, _ := d.get(cfg.ID)
	task.mu.Lock()
	instance := task.state.Instance
	task.mu.Unlock()
	lock, e := d.store.lockMode(lima.Hash([]byte("instance-operation:"+instance)), syscall.LOCK_SH)
	if e != nil {
		t.Fatal(e)
	}
	_, e = d.ExecTask(cfg.ID, finishCommand(r), time.Second)
	unlock(lock)
	if e == nil || !strings.Contains(e.Error(), "ACTIVE_OPERATION") {
		t.Fatal("finalization crossed active command", e)
	}
	task.mu.Lock()
	armed := task.state.Finalization != nil
	task.mu.Unlock()
	if armed {
		t.Fatal("active operation was fenced destructively")
	}
	if _, e = d.ExecTask(cfg.ID, []string{"/bin/true"}, time.Second); e != nil {
		t.Fatal("operation cancellation leaked from rejected finish", e)
	}
}

func TestResumedAllocationFinalizesWithoutPoisoningHistoricalLedger(t *testing.T) {
	d, f, _, old, r := finishFixture(t)
	if e := d.StopTask(old.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	if e := d.DestroyTask(old.ID, false); e != nil {
		t.Fatal(e)
	}
	source, e := d.store.read(lima.Hash([]byte(old.ID)))
	if e != nil {
		t.Fatal(e)
	}
	next, token := tokenFixture(t)
	next.ID = "resumed-final/task/attempt"
	next.AllocID = "00000000-0000-4000-8000-000000000046"
	next.NodeID = old.NodeID
	next.Namespace = old.Namespace
	next.Env = old.Env
	var tc TaskConfig
	old.DecodeDriverConfig(&tc)
	tc.ResumeFromKey = source.Key
	tc.ResumeGeneration = int(generation(source))
	tc.ResumeNodeID = source.NodeID
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000047"
	next.EncodeConcreteDriverConfig(tc)
	os.WriteFile(token, []byte("synthetic-fresh-resumed-token"), 0600)
	f.mu.Lock()
	f.phase = "running"
	f.mu.Unlock()
	if _, _, e = d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	r.Allocation = next.AllocID
	r.Session = tc.WorkerSessionID
	if _, e = d.ExecTask(next.ID, finishCommand(r), time.Second); e != nil {
		t.Fatal(e)
	}
	if _, e = d.ledger(); e != nil {
		t.Fatal("schema4 poisoned resumed running ledger", e)
	}
	if e = d.StopTask(next.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].DiskGiB != 0 {
		t.Fatal("historical alias retained/poisoned deleted disk quota", e)
	}
}
func TestFinalizationAdmissionBusyThenGCCompletesSameOwnedPurge(t *testing.T) {
	d, _, _, cfg, r := finishFixture(t)
	if _, e := d.ExecTask(cfg.ID, finishCommand(r), time.Second); e != nil {
		t.Fatal(e)
	}
	lock, e := d.store.lock(lima.Hash([]byte("node-admission")))
	if e != nil {
		t.Fatal(e)
	}
	e = d.StopTask(cfg.ID, time.Second, "")
	unlock(lock)
	if e == nil || !strings.Contains(e.Error(), "ADMISSION_BUSY") {
		t.Fatal("busy purge incorrectly completed", e)
	}
	if e = d.DestroyTask(cfg.ID, false); e != nil {
		t.Fatal("GC did not retry same verified finish", e)
	}
	task, _ := d.get(cfg.ID)
	task.mu.Lock()
	phase := task.state.Phase
	task.mu.Unlock()
	if phase != "destroyed_disk_purged" {
		t.Fatal(phase)
	}
}
