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

func abandonedFixture(t *testing.T, phase string) (FailedCreateOptions, *Store, *State, purgeDeps, *int) {
	o, store, s, d, calls := stoppedPurgeFixture(t)
	s.Phase = phase
	s.Exit = nil
	s.VaultFinalizing = false
	s.CreatorAgentID = "creator"
	s.CreatorSessionID = "native-session"
	if e := store.write(s); e != nil {
		t.Fatal(e)
	}
	raw, _ := privateRecoveryRead(filepath.Join(store.Root, s.Key+".json"), 1<<20)
	o.ExpectedLedgerSHA256 = lima.Hash(raw)
	raw, _ = privateRecoveryRead(o.StoppedSourceReceipt, 1<<20)
	var r stoppedPurgeReceipt
	json.Unmarshal(raw, &r)
	r.Purpose = "abandoned_stopped_purge"
	r.LedgerSHA256 = o.ExpectedLedgerSHA256
	raw, _ = json.Marshal(r)
	os.WriteFile(o.StoppedSourceReceipt, raw, 0600)
	o.StoppedSourceReceiptSHA256 = lima.Hash(raw)
	o.AbandonedStopped = true
	o.AllowGC404 = true
	previous := d.allocation
	d.allocation = func(ctx context.Context) (failedAllocation, error) {
		a, e := previous(ctx)
		a.CurrentJob = &purgeJobProof{HTTPStatus: 200, ModifyIndex: 1, ID: o.JobID, Namespace: o.Namespace, Status: "dead", Stop: true, CreatorAgentID: s.CreatorAgentID, CreatorSessionID: s.CreatorSessionID}
		return a, e
	}
	d.dependencies = func(context.Context, *State) error { return nil }
	return o, store, s, d, calls
}
func TestAbandonedStoppedPurgeKeepsOriginalUnknownAndNoRevokeClaim(t *testing.T) {
	for _, phase := range []string{"running", "exited"} {
		t.Run(phase, func(t *testing.T) {
			o, store, s, d, calls := abandonedFixture(t, phase)
			r, e := purgeRetained(t.Context(), o, d)
			if e != nil || *calls != 1 || r.Phase != "destroyed_disk_purged" {
				t.Fatal(r, e, *calls)
			}
			after, _ := store.read(s.Key)
			if after.Exit != nil || after.StopReceipt != nil || after.DiskGiB != 0 {
				t.Fatal("fabricated completion", after)
			}
			raw, _ := privateRecoveryRead(filepath.Join(store.Root, s.Key+".purge-intent.audit"), 1<<20)
			var intent purgeIntent
			json.Unmarshal(raw, &intent)
			if intent.Original.Phase != phase || intent.Original.Exit != nil || !strings.Contains(string(intent.StoppedSourceRecovery), `"revocation":"unconfirmed"`) {
				t.Fatal("lost original unknown outcome")
			}
			again, e := purgeRetained(t.Context(), o, d)
			if e != nil || !again.AlreadyRecovered || *calls != 1 {
				t.Fatal(again, e, *calls)
			}
		})
	}
}
func TestAbandonedStoppedPurgeFailClosed(t *testing.T) {
	for _, kind := range []string{"job-running", "job-owner", "job-namespace", "job-missing", "job-http", "active-allocation", "dependency", "unknown-delete", "wrong-purpose"} {
		t.Run(kind, func(t *testing.T) {
			o, store, s, d, calls := abandonedFixture(t, "running")
			previous := d.allocation
			switch kind {
			case "dependency":
				d.dependencies = func(context.Context, *State) error { return fmt.Errorf("dependent disk") }
			case "unknown-delete":
				d.remove = func(context.Context, string) error { return fmt.Errorf("unknown") }
			case "wrong-purpose":
				raw, _ := privateRecoveryRead(o.StoppedSourceReceipt, 1<<20)
				var r stoppedPurgeReceipt
				json.Unmarshal(raw, &r)
				r.Purpose = ""
				raw, _ = json.Marshal(r)
				os.WriteFile(o.StoppedSourceReceipt, raw, 0600)
				o.StoppedSourceReceiptSHA256 = lima.Hash(raw)
			default:
				d.allocation = func(ctx context.Context) (failedAllocation, error) {
					a, e := previous(ctx)
					switch kind {
					case "job-running":
						a.CurrentJob.Stop = false
					case "job-owner":
						a.CurrentJob.CreatorAgentID = "other"
					case "job-namespace":
						a.CurrentJob.Namespace = "other"
					case "job-missing":
						a.CurrentJob = nil
					case "job-http":
						a.CurrentJob.HTTPStatus = 403
					case "active-allocation":
						a.ClientStatus = "running"
					}
					return a, e
				}
			}
			if _, e := purgeRetained(t.Context(), o, d); e == nil {
				t.Fatal("accepted unsafe purge")
			}
			if *calls != 0 {
				t.Fatal("unsafe remove")
			}
			after, _ := store.read(s.Key)
			if after.DiskGiB == 0 {
				t.Fatal("released reservation without absence")
			}
		})
	}
}
func TestAbandonedPurgeBackingChain(t *testing.T) {
	for _, kind := range []string{"independent", "dependent", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			for _, n := range []string{"target", "other"} {
				os.Mkdir(filepath.Join(root, n), 0700)
				os.WriteFile(filepath.Join(root, n, "disk"), []byte(n), 0600)
			}
			target := filepath.Join(root, "target", "disk")
			other := filepath.Join(root, "other", "disk")
			backing := other
			if kind == "dependent" {
				backing = target
			}
			if kind == "symlink" {
				os.Symlink(target, filepath.Join(root, "other", "basedisk"))
			}
			b, _ := json.Marshal([]map[string]string{{"filename": other, "full-backing-filename": backing}})
			binary := filepath.Join(t.TempDir(), "qemu-img")
			os.WriteFile(binary, []byte("#!/bin/sh\ncat <<'JSON'\n"+string(b)+"\nJSON\n"), 0700)
			e := stoppedPurgeDependencies(t.Context(), FailedCreateOptions{LimaHome: root, QemuImg: binary}, &State{Instance: "target"})
			if (e == nil) != (kind == "independent") {
				t.Fatal(kind, e)
			}
		})
	}
}

func TestAbandonedPurgeUnknownDeletedResponseReconcilesWithoutSecondDelete(t *testing.T) {
	o, store, s, d, calls := abandonedFixture(t, "running")
	remove := d.remove
	d.remove = func(c context.Context, n string) error {
		if e := remove(c, n); e != nil {
			return e
		}
		return fmt.Errorf("response lost")
	}
	if _, e := purgeRetained(t.Context(), o, d); e == nil {
		t.Fatal("unknown accepted")
	}
	pending, _ := store.read(s.Key)
	if pending.DiskGiB == 0 || pending.Phase != "purge_pending" {
		t.Fatal("released without postdelete proof")
	}
	d.remove = remove
	result, e := purgeRetained(t.Context(), o, d)
	if e != nil || result.Phase != "destroyed_disk_purged" || *calls != 1 {
		t.Fatal(result, e, *calls)
	}
}

func TestAbandonedPurgeExistingAmbiguousIntentNeverRedispatches(t *testing.T) {
	for _, crashBeforeStateCommit := range []bool{false, true} {
		t.Run(fmt.Sprint(crashBeforeStateCommit), func(t *testing.T) {
			o, store, s, d, calls := abandonedFixture(t, "running")
			original := *s
			d.remove = func(context.Context, string) error { *calls++; return fmt.Errorf("unknown delivery") }
			if _, e := purgeRetained(t.Context(), o, d); e == nil {
				t.Fatal("unknown accepted")
			}
			if crashBeforeStateCommit {
				if e := store.write(&original); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := purgeRetained(t.Context(), o, d); e == nil || *calls != 1 {
				t.Fatal("delete redispatched", e, *calls)
			}
			after, _ := store.read(s.Key)
			if after.DiskGiB == 0 {
				t.Fatal("released present disk")
			}
		})
	}
}
func TestAbandonedPurgeGCJobNeedsFullAbsenceProof(t *testing.T) {
	for _, kind := range []string{"valid", "no-list", "forbidden", "wrong-ns", "allocation-present"} {
		t.Run(kind, func(t *testing.T) {
			o, _, _, d, calls := abandonedFixture(t, "exited")
			d.allocation = func(context.Context) (failedAllocation, error) {
				j := &purgeJobProof{HTTPStatus: 404, ID: o.JobID, Namespace: o.Namespace, AbsenceVerified: true, NamespaceJobsHTTPStatus: 200, NamespaceAllocationsHTTPStatus: 200}
				a := failedAllocation{HTTPStatus: 404, OperatorManagementVerified: true, NodeHTTPStatus: 200, NodeAllocationsHTTPStatus: 200, VerifiedNodeID: o.NodeID, CurrentJob: j}
				switch kind {
				case "no-list":
					j.AbsenceVerified = false
				case "forbidden":
					j.NamespaceJobsHTTPStatus = 403
				case "wrong-ns":
					j.Namespace = "other"
				case "allocation-present":
					a.HTTPStatus = 200
				}
				return a, nil
			}
			_, e := purgeRetained(t.Context(), o, d)
			if kind == "valid" {
				if e != nil || *calls != 1 {
					t.Fatal(e, *calls)
				}
			} else if e == nil || *calls != 0 {
				t.Fatal("unsafe GC purge", e, *calls)
			}
		})
	}
}
