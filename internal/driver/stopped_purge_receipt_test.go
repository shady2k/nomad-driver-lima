package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stoppedPurgeFixture(t *testing.T) (FailedCreateOptions, *Store, *State, purgeDeps, *int) {
	o, store, s, d, calls := purgeFixture(t)
	s.Phase = "exited"
	s.Destroyed = false
	s.VaultFinalizing = true
	s.Exit = &lima.Result{Code: 143}
	s.WorkerSessionID = "00000000-0000-4000-8000-000000000003"
	if e := store.write(s); e != nil {
		t.Fatal(e)
	}
	raw, e := privateRecoveryRead(filepath.Join(store.Root, s.Key+".json"), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	o.ExpectedLedgerSHA256 = lima.Hash(raw)
	r := stoppedPurgeReceipt{Allocation: s.AllocID, Instance: s.Instance, Session: s.WorkerSessionID, VMStatus: "Stopped", DiskRetained: true, DiskSizeBytes: 1024, DiskSHA256: strings.Repeat("b", 64), LedgerUnchanged: true, LedgerSHA256: o.ExpectedLedgerSHA256, Revocation: "unconfirmed", ApprovalReference: "exact-user-stop-approval"}
	r.RuntimeStop.OK = true
	r.RuntimeStop.Stopped = true
	r.RuntimeStop.Revocation = "not_attempted"
	raw, _ = json.Marshal(r)
	o.StoppedSourceReceipt = filepath.Join(t.TempDir(), "source-stop.json")
	if e = os.WriteFile(o.StoppedSourceReceipt, raw, 0600); e != nil {
		t.Fatal(e)
	}
	o.StoppedSourceReceiptSHA256 = lima.Hash(raw)
	d.diskProof = func(*State) (string, int64, error) { return r.DiskSHA256, r.DiskSizeBytes, nil }
	return o, store, s, d, calls
}
func TestStoppedUnconfirmedPurgeRecordsTruthAndReleasesOnlyAbsentDisk(t *testing.T) {
	o, store, s, d, calls := stoppedPurgeFixture(t)
	r, e := purgeRetained(t.Context(), o, d)
	if e != nil || r.Phase != "destroyed_disk_purged" || *calls != 1 {
		t.Fatal(r, e, *calls)
	}
	after, _ := store.read(s.Key)
	if !after.Destroyed || !after.VaultFinalizing || after.DiskGiB != 0 {
		t.Fatal("lost truthful unresolved revocation", after)
	}
	raw, e := privateRecoveryRead(filepath.Join(store.Root, s.Key+".purge-intent.audit"), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	var intent purgeIntent
	json.Unmarshal(raw, &intent)
	if intent.Original.Destroyed || intent.Original.Phase != "exited" || !intent.Original.VaultFinalizing || len(intent.StoppedSourceRecovery) == 0 || intent.StoppedSourceReceiptSHA256 != o.StoppedSourceReceiptSHA256 {
		t.Fatal("fabricated original completion or lost receipt")
	}
	d.diskProof = func(*State) (string, int64, error) {
		t.Fatal("idempotent purge rehashed absent disk")
		return "", 0, nil
	}
	again, e := purgeRetained(t.Context(), o, d)
	if e != nil || !again.AlreadyRecovered || *calls != 1 {
		t.Fatal(again, e, *calls)
	}
}
func TestStoppedPurgeRejectsWrongProofAndActiveTarget(t *testing.T) {
	for _, kind := range []string{"receipt-hash", "disk-hash", "ledger-changed", "active-process", "active-alloc", "missing-proof"} {
		t.Run(kind, func(t *testing.T) {
			o, store, s, d, calls := stoppedPurgeFixture(t)
			switch kind {
			case "receipt-hash":
				o.StoppedSourceReceiptSHA256 = strings.Repeat("f", 64)
			case "disk-hash":
				d.diskProof = func(*State) (string, int64, error) { return strings.Repeat("c", 64), 1024, nil }
			case "ledger-changed":
				s.Purpose = "changed"
				store.write(s)
			case "active-process":
				d.observe = func(context.Context, *State) (purgeObservation, error) {
					return purgeObservation{}, fmt.Errorf("active process")
				}
			case "active-alloc":
				old := d.allocation
				d.allocation = func(c context.Context) (failedAllocation, error) {
					a, e := old(c)
					a.ClientStatus = "running"
					return a, e
				}
			case "missing-proof":
				o.StoppedSourceReceipt = ""
			}
			if _, e := purgeRetained(t.Context(), o, d); e == nil || *calls != 0 {
				t.Fatal("unsafe delete", e, *calls)
			}
		})
	}
}
func TestStoppedPurgeUnknownDeleteReconcilesSameAuditWithoutReplay(t *testing.T) {
	o, store, s, d, calls := stoppedPurgeFixture(t)
	remove := d.remove
	d.remove = func(c context.Context, n string) error { remove(c, n); return fmt.Errorf("lost delete response") }
	if _, e := purgeRetained(t.Context(), o, d); e == nil {
		t.Fatal("unknown deletion accepted")
	}
	pending, _ := store.read(s.Key)
	if pending.Destroyed || pending.DiskGiB == 0 || pending.Phase != "purge_pending" {
		t.Fatal("released before absence proof")
	}
	d.remove = remove
	r, e := purgeRetained(t.Context(), o, d)
	if e != nil || r.Phase != "destroyed_disk_purged" || *calls != 1 {
		t.Fatal(r, e, *calls)
	}
}

func TestStoppedReceiptGCAllowsOtherJobsWithoutTouchingThem(t *testing.T) {
	o, _, _, d, calls := stoppedPurgeFixture(t)
	o.AllowGC404 = true
	d.allocation = func(context.Context) (failedAllocation, error) {
		return failedAllocation{HTTPStatus: 404, OperatorManagementVerified: true, NodeHTTPStatus: 200, NodeAllocationsHTTPStatus: 200, VerifiedNodeID: o.NodeID, NodeActiveAllocations: 1}, nil
	}
	r, e := purgeRetained(t.Context(), o, d)
	if e != nil || r.Phase != "destroyed_disk_purged" || *calls != 1 {
		t.Fatal(r, e, *calls)
	}
}
