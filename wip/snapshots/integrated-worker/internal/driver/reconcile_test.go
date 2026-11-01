package driver

import (
	"encoding/json"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportNeverReclaimsActiveOwnerAbsentOrUnknown(t *testing.T) {
	now := time.Unix(10000, 0)
	s := &State{StartedAt: now.Add(-time.Hour), Lifecycle: ActivityObservation{WorkState: "active", LastActivityAt: now.Add(-time.Hour)}}
	r := classifyActivity(s, now, false)
	if r.OwnerState != "owner_unconfirmed" || !r.OrphanSuspect || r.IdleState != "active" || r.AutoClose || r.AutoDelete {
		t.Fatal(r)
	}
	r = classifyActivity(s, now, true)
	if r.IdleState != "unknown" {
		t.Fatal(r)
	}
	s.Lifecycle.WorkState = "idle"
	if classifyActivity(s, now, false).IdleState != "idle_grace_elapsed" {
		t.Fatal("idle grace missing")
	}
	if classifyActivity(s, now.Add(-2*time.Hour), false).IdleState != "unknown" {
		t.Fatal("backwards clock treated as idle")
	}
}
func TestObservedRequestReplayCannotRenewOwner(t *testing.T) {
	d := setup(t, newFake(), "")
	s := &State{WorkerSessionID: "worker-session"}
	task := &task{state: s}
	line := []byte(`{"component":"agent-plugin","stage":"request_running","session_id":"worker-session","request_id":"one","time":1}` + "\n")
	first := time.Unix(100, 0)
	d.observeActivity(task, line, first)
	d.observeActivity(task, line, first.Add(time.Hour))
	if !s.Lifecycle.OwnerObservedAt.Equal(first) {
		t.Fatal("replay renewed owner observation")
	}
	if s.Lifecycle.LastOwnerEvent != lima.Hash(line[:len(line)-1]) {
		t.Fatal("must persist digest, not raw log")
	}
	raw, _ := json.Marshal(s.Lifecycle)
	if string(raw) == string(line) {
		t.Fatal("raw payload stored")
	}
	other := []byte(`{"component":"agent-plugin","stage":"request_terminal","session_id":"different"}` + "\n")
	d.observeActivity(task, other, first.Add(time.Hour))
	if s.Lifecycle.WorkState != "active" {
		t.Fatal("foreign session changed activity")
	}
}

func TestUnattachedReconcileWritesEvidenceNeverStopsOrAdopts(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	key := lima.Hash([]byte("unattached"))
	s := &State{Version: 1, Key: key, Owner: d.store.Owner, AllocID: "alloc", TaskID: "task", Instance: "nomad-" + key[:24], CreatorAgentID: "initiator", CreatorSessionID: "session"}
	if e := d.store.write(s); e != nil {
		t.Fatal(e)
	}
	f.infos[s.Instance] = lima.Info{Name: s.Instance, Status: "Running", Param: map[string]string{"nomad_driver_id": s.Owner, "nomad_task_key": s.Key}}
	d.reconcileSnapshot(map[string]string{})
	raw, e := os.ReadFile(filepath.Join(d.store.Root, "reports", key+".json"))
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(raw), "unattached_owned_vm_owner_unconfirmed") {
		t.Fatal(string(raw))
	}
	if f.stops+f.deletes+f.creates+f.launches != 0 {
		t.Fatal("report mutated runtime")
	}
	f.infos[s.Instance] = lima.Info{Name: s.Instance, Status: "Running", Param: map[string]string{"nomad_driver_id": "foreign"}}
	d.reconcileSnapshot(map[string]string{})
	raw, _ = os.ReadFile(filepath.Join(d.store.Root, "reports", key+".json"))
	if !strings.Contains(string(raw), "identity_mismatch_vm_retained") {
		t.Fatal(string(raw))
	}
}
