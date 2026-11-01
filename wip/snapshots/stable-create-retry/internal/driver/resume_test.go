package driver

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
)

func TestResumeSameDiskFreshAllocationAndOldHandleFenced(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	old, tokenPath := tokenFixture(t)
	old.NodeID = "source-node"
	old.Namespace = "workers"
	if e := os.WriteFile(tokenPath, []byte("synthetic-old"), 0600); e != nil {
		t.Fatal(e)
	}
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
	if source.StopReceipt == nil || !source.StopReceipt.RevocationConfirmed {
		t.Fatal("missing stop receipt")
	}
	next, nextToken := tokenFixture(t)
	next.ID = "new-allocation/work/attempt"
	next.AllocID = "00000000-0000-4000-8000-000000000004"
	next.NodeID = old.NodeID
	next.Namespace = old.Namespace
	if e = os.WriteFile(nextToken, []byte("synthetic-new"), 0600); e != nil {
		t.Fatal(e)
	}
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
	if e = os.WriteFile(nextToken, []byte("synthetic-old"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = d.StartTask(next); e == nil {
		t.Fatal("reused token accepted")
	}
	unchanged, err := d.store.read(source.Key)
	if err != nil || unchanged.ResumeNext != nil {
		t.Fatal("failed preflight transferred ownership", err)
	}
	if e = os.WriteFile(nextToken, []byte("synthetic-new"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	current, e := d.store.read(lima.Hash([]byte(next.ID)))
	if e != nil {
		t.Fatal(e)
	}
	if current.Instance != source.Instance || current.InstanceKey != source.Key || current.Generation != 2 || current.ResumedFrom != source.Key {
		t.Fatal("did not preserve disk/lineage")
	}
	if e = d.StopTask(old.ID, time.Second, ""); e == nil {
		t.Fatal("old stop accepted")
	}
	if e = d.DestroyTask(old.ID, true); e == nil {
		t.Fatal("old destroy accepted")
	}
	if e = d.RecoverTask(mustHandle(t, d, old.ID)); e != nil { // Existing cached handles remain terminal, never adopt the successor.
		t.Fatal(e)
	}
	if e = d.store.write(source); e == nil {
		t.Fatal("stale state overwrite accepted")
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].Key != current.Key {
		t.Fatal("disk counted twice", rows, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 2 || f.launches != 2 || f.deletes != 0 {
		t.Fatalf("wrong effects: create=%d start=%d launch=%d delete=%d", f.creates, f.starts, f.launches, f.deletes)
	}
}
func mustHandle(t *testing.T, d *Driver, id string) *drivers.TaskHandle {
	t.Helper()
	task, e := d.get(id)
	if e != nil {
		t.Fatal(e)
	}
	h, e := d.handle(task)
	if e != nil {
		t.Fatal(e)
	}
	return h
}

func TestResumeIntentProjectsQuotaAndFencesBeforeSuccessorWrite(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	store, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	source, next := resumeProjectionStates(store.Owner)
	if e = store.write(source); e != nil {
		t.Fatal(e)
	}
	stale := *source
	source.ResumeNext = next
	if e = store.write(source); e != nil {
		t.Fatal(e)
	}
	d := &Driver{store: store, ctx: context.Background()}
	if e = d.currentOwner(&stale); e == nil {
		t.Fatal("old authority survived intent")
	}
	if e = store.write(&stale); e == nil {
		t.Fatal("stale write survived intent")
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].Key != next.Key || rows[0].DiskGiB != 20 {
		t.Fatal(rows, e)
	}
	if e = store.reconcileResumeIntents(); e != nil {
		t.Fatal(e)
	}
	if restored, err := store.read(next.Key); err != nil || restored.SpecHash != next.SpecHash {
		t.Fatal("intent not reconciled", err)
	}
	rows, e = d.ledger()
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	next.SpecHash = "changed"
	if e = store.write(next); e != nil {
		t.Fatal(e)
	}
	if _, e = d.ledger(); e == nil {
		t.Fatal("contradictory successor admitted")
	}
}

func TestResumeRejectsUnconfirmedAndStaleBindings(t *testing.T) {
	for _, kind := range []string{"receipt", "revocation", "generation", "node", "namespace", "session", "allocation", "disk", "phase"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixture(t)
			cfg.NodeID = "node"
			cfg.Namespace = "workers"
			source := &State{Key: strings.Repeat("a", 64), AllocID: "old", TaskID: "old/task", Instance: "same", WorkerSessionID: "00000000-0000-4000-8000-000000000003", Generation: 1, MachineID: "machine", BootID: "boot1", Phase: "stopped", NodeID: "node", Namespace: "workers", CPUs: 1, MemoryMiB: 512, DiskGiB: 1, ResolvedArchitecture: runtime.GOARCH, Exit: &lima.Result{}}
			source.StopReceipt = &StopReceipt{Allocation: source.AllocID, Session: source.WorkerSessionID, Instance: source.Instance, Generation: 1, RevocationConfirmed: true, ConfirmedAt: time.Now(), TokenSHA256: strings.Repeat("a", 64), ExposedTokenSHA256: []string{strings.Repeat("a", 64)}, Disk: &RetainedDiskIdentity{Name: "disk", Ino: 1, Size: 1}}
			tc := TaskConfig{ResumeFromKey: source.Key, ResumeGeneration: 1, ResumeNodeID: "node", VaultRequired: true, WorkerSessionID: "00000000-0000-4000-8000-000000000005", CPUs: 1, MemoryMiB: 512, DiskGiB: 1, Architecture: runtime.GOARCH}
			if e := validateResumeSource(source, cfg, tc); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "receipt":
				source.StopReceipt = nil
			case "revocation":
				source.StopReceipt.RevocationConfirmed = false
			case "generation":
				tc.ResumeGeneration = 2
			case "node":
				cfg.NodeID = "other"
			case "namespace":
				cfg.Namespace = "other"
			case "session":
				tc.WorkerSessionID = source.WorkerSessionID
			case "allocation":
				cfg.AllocID = source.AllocID
			case "disk":
				tc.DiskGiB = 2
			case "phase":
				source.Phase = "running"
			}
			if e := validateResumeSource(source, cfg, tc); e == nil {
				t.Fatal("unsafe resume accepted")
			}
		})
	}
}

func TestPurgeTransferredSourceHasNoDeleteEffects(t *testing.T) {
	o, store, source, deps, calls := purgeFixture(t)
	next := *source
	next.Key = strings.Repeat("c", 64)
	next.ResumedFrom = source.Key
	next.Generation = generation(source) + 1
	next.ResumeNext = nil
	source.ResumeNext = &next
	if e := store.write(source); e != nil {
		t.Fatal(e)
	}
	if _, e := purgeRetained(t.Context(), o, deps); e == nil {
		t.Fatal("old source purge accepted")
	}
	if *calls != 0 {
		t.Fatal("old purge deleted resumed disk")
	}
}

func resumeProjectionStates(owner string) (*State, *State) {
	source := &State{Version: 1, Owner: owner, TaskID: "source/task", AllocID: "00000000-0000-4000-8000-000000000001", WorkerSessionID: "00000000-0000-4000-8000-000000000002", Instance: "same-instance", Namespace: "workers", NodeID: "node", DiskGiB: 20, CPUs: 1, MemoryMiB: 512, Phase: "stopped", Generation: 1, MachineID: "machine", BootID: "boot1"}
	source.Key = lima.Hash([]byte(source.TaskID))
	next := *source
	next.TaskID = "next/task"
	next.Key = lima.Hash([]byte(next.TaskID))
	next.AllocID = "00000000-0000-4000-8000-000000000003"
	next.WorkerSessionID = "00000000-0000-4000-8000-000000000004"
	next.ResumedFrom = source.Key
	next.InstanceKey = source.Key
	next.Generation = 2
	next.Phase = "resume_intent"
	next.SpecHash = lima.Hash([]byte("exact-spec"))
	next.ResumeMachineID = source.MachineID
	next.ResumePreviousBootID = source.BootID
	next.MachineID = ""
	next.BootID = ""
	return source, &next
}
