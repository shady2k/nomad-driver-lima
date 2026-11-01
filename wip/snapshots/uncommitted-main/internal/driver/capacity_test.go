package driver

import (
	"nomad-driver-lima/internal/lima"
	"testing"
"os"
)

func TestCapacityRetainedAndBusyUnknown(t *testing.T) {
	root := t.TempDir()
 os.Chmod(root,0700)
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
	if known || len(a) != 1 {
		t.Fatal("busy admission advertised free capacity")
	}
}
