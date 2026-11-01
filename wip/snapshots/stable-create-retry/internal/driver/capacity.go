package driver

import (
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	"nomad-driver-lima/internal/lima"
	"syscall"
	"time"
)

// Read-only advisory snapshot of the same ledger/physical volume used by admission.
// Busy or unreadable admission state is unknown, never a fabricated zero/free slot.
func capacityAttributes(cfg Config, store *Store) map[string]*pstructs.Attribute {
	out := map[string]*pstructs.Attribute{"driver.lima.capacity.known": pstructs.NewBoolAttribute(false)}
	if store == nil {
		return out
	}
	lock, err := store.lock(lima.Hash([]byte("node-admission")))
	if err != nil {
		return out
	}
	defer unlock(lock)
	rows, err := (&Driver{store: store}).ledger()
	if err != nil {
		return out
	}
	var count, cpu, mem, disk int64
	for _, s := range rows {
		purged, e := store.purged(s)
		if e != nil {
			return out
		}
		if purged || s.Phase == "no_vm_failed" {
			continue
		}
		disk += int64(s.DiskGiB)
		if !s.Destroyed && s.Phase != "stopped" {
			count++
			cpu += int64(s.CPUs)
			mem += int64(s.MemoryMiB + cfg.MemoryOverheadMiB)
		}
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(cfg.LimaHome, &fs) != nil {
		return out
	}
	values := map[string]int64{"observed_unix": time.Now().Unix(), "used_vms": count, "used_vcpus": cpu, "used_reservation_mib": mem, "used_disk_gib": disk, "physical_available_mib": int64(uint64(fs.Bavail) * uint64(fs.Bsize) / 1048576), "disk_floor_mib": int64(cfg.DiskFreeFloorMiB)}
	out["driver.lima.capacity.known"] = pstructs.NewBoolAttribute(true)
	for k, v := range values {
		out["driver.lima.capacity."+k] = pstructs.NewIntAttribute(v, "")
	}
	return out
}
