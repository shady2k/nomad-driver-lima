package driver

import (
	"context"
	"nomad-driver-lima/internal/lima"
	"os"
	"testing"
	"time"
)

func TestCapacityRetainedAndBusyUnknown(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	store, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{LimaHome: root, MemoryOverheadMiB: 128, DiskFreeFloorMiB: 100}
	for i, s := range []*State{{Destroyed: true, DiskGiB: 20, Phase: "destroyed_disk_retained"}, {CPUs: 2, MemoryMiB: 1024, DiskGiB: 20, Phase: "running"}, {CPUs: 7, MemoryMiB: 9999, DiskGiB: 40, Phase: "no_vm_failed"}} {
		s.Key = lima.Hash([]byte{byte(i)})
		s.Owner = store.Owner
		s.Version = 1
		if e := store.write(s); e != nil {
			t.Fatal(e)
		}
	}
	a := capacityAttributes(cfg, store)
	for k, want := range map[string]int64{"used_vms": 1, "used_vcpus": 2, "used_reservation_mib": 1152, "used_disk_gib": 40, "disk_floor_mib": 100} {
		v, ok := a["driver.lima.capacity."+k].GetInt()
		if !ok || v != want {
			t.Fatalf("%s=%v want %v", k, v, want)
		}
	}
	lock, e := store.lock(lima.Hash([]byte("node-admission")))
	if e != nil {
		t.Fatal(e)
	}
	defer unlock(lock)
	a = capacityAttributes(cfg, store)
	known, _ := a["driver.lima.capacity.known"].GetBool()
	reason, _ := a["driver.lima.capacity.reason"].GetString()
	if known || reason != "admission_lock_busy" {
		t.Fatal("busy admission advertised free capacity")
	}
}

// Both production loops run every 30s. Reconciliation takes this exact lock
// even with no VM starts; a brief overlapping scan must not poison every tick.
func TestCapacityRepeatedPeriodicContentionThenPersistentBusyAndCancel(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	store, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{StateDir: root, LimaHome: root}
	for i := 0; i < 4; i++ {
		lock, e := store.lock(lima.Hash([]byte("node-admission")))
		if e != nil {
			t.Fatal(e)
		}
		done := make(chan struct{})
		go func() { time.Sleep(25 * time.Millisecond); unlock(lock); close(done) }()
		attrs := capacityAttributesContext(t.Context(), cfg, store)
		<-done
		known, _ := attrs["driver.lima.capacity.known"].GetBool()
		reason, _ := attrs["driver.lima.capacity.reason"].GetString()
		if !known || reason != "ready" {
			t.Fatalf("periodic overlap remained unknown: %s", reason)
		}
	}
	lock, e := store.lock(lima.Hash([]byte("node-admission")))
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	start := time.Now()
	a := capacityAttributesContext(ctx, cfg, store)
	cancel()
	known, _ := a["driver.lima.capacity.known"].GetBool()
	if known || time.Since(start) > 100*time.Millisecond {
		t.Fatal("cancelled wait advertised capacity or blocked")
	}
	a = capacityAttributes(cfg, store)
	known, _ = a["driver.lima.capacity.known"].GetBool()
	if known {
		t.Fatal("persistent contention bypassed")
	}
	unlock(lock)
	a = capacityAttributes(cfg, store)
	known, _ = a["driver.lima.capacity.known"].GetBool()
	if !known {
		t.Fatal("prior timeout leaked lock/poisoned snapshot")
	}
}
func TestCapacityFailureReasonsAreBoundedAndDoNotLeakLock(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	store, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{StateDir: root, LimaHome: root}
	bad := root + "/" + lima.Hash([]byte("invalid")) + ".json"
	os.WriteFile(bad, []byte("private raw content must not be surfaced"), 0600)
	a := capacityAttributes(cfg, store)
	reason, _ := a["driver.lima.capacity.reason"].GetString()
	if reason != "ledger_unreadable" {
		t.Fatal(reason)
	}
	os.Remove(bad)
	cfg.LimaHome = root + "/absent"
	a = capacityAttributes(cfg, store)
	reason, _ = a["driver.lima.capacity.reason"].GetString()
	if reason != "physical_statfs_unavailable" {
		t.Fatal(reason)
	}
	cfg.LimaHome = root
	a = capacityAttributes(cfg, store)
	known, _ := a["driver.lima.capacity.known"].GetBool()
	if !known {
		t.Fatal("failed snapshot leaked lock")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a = capacityAttributesContext(ctx, cfg, store)
	known, _ = a["driver.lima.capacity.known"].GetBool()
	if known {
		t.Fatal("cancelled snapshot advertised capacity")
	}
}

func TestCapacityConcurrentActualReconciliationReadsFreshChangedLedger(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	store, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{StateDir: root, LimaHome: root, MemoryOverheadMiB: 128}
	s := &State{Version: 1, Owner: store.Owner, Key: lima.Hash([]byte("retained")), Phase: "destroyed_disk_retained", Destroyed: true, DiskGiB: 64}
	if e = store.write(s); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 4; i++ {
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- store.reconcileResumeIntentsWithLedger(func() ([]*State, error) {
				close(entered)
				time.Sleep(25 * time.Millisecond)
				s.DiskGiB++
				if e := store.write(s); e != nil {
					return nil, e
				}
				return (&Driver{store: store}).ledger()
			})
		}()
		<-entered
		// The old fingerprint's one nonblocking attempt deterministically fails here.
		if lock, e := store.lock(lima.Hash([]byte("node-admission"))); e == nil {
			unlock(lock)
			t.Fatal("fixture did not reproduce overlapping reconciliation")
		}
		attrs := capacityAttributesContext(t.Context(), cfg, store)
		if e := <-done; e != nil {
			t.Fatal(e)
		}
		known, _ := attrs["driver.lima.capacity.known"].GetBool()
		disk, _ := attrs["driver.lima.capacity.used_disk_gib"].GetInt()
		if !known || disk != int64(65+i) {
			t.Fatalf("stale or unknown coherent read known=%v disk=%d", known, disk)
		}
	}
}

func TestCapacityUsesSameRuntimeMemoryAndFiniteLimit(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	store, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{LimaHome: root, MemoryOverheadMiB: 128}
	attrs := capacityAttributesContext(t.Context(), cfg, store, func([]*State) (runtimeCommitment, int64, error) {
		return runtimeCommitment{VMs: 3, CPUs: 3, MemoryMiB: 29056}, 30053, nil
	})
	known, _ := attrs["driver.lima.capacity.known"].GetBool()
	used, _ := attrs["driver.lima.capacity.used_reservation_mib"].GetInt()
	limit, _ := attrs["driver.lima.capacity.host_memory_limit_mib"].GetInt()
	if !known || used != 29056 || limit != 30053 {
		t.Fatal("runtime memory disagreement")
	}
	attrs = capacityAttributesContext(t.Context(), cfg, store, func([]*State) (runtimeCommitment, int64, error) {
		return runtimeCommitment{}, 0, context.DeadlineExceeded
	})
	known, _ = attrs["driver.lima.capacity.known"].GetBool()
	if known {
		t.Fatal("unknown runtime inventory advertised free RAM")
	}
}
