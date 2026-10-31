package driver

import (
	"context"
	"encoding/json"
	"errors"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	"nomad-driver-lima/internal/lima"
	"os"
	"syscall"
	"time"
)

// Read-only advisory snapshot of the same ledger/physical volume used by admission.
// Busy or unreadable admission state is unknown, never a fabricated zero/free slot.
func capacityAttributes(cfg Config, store *Store) map[string]*pstructs.Attribute {
	return capacityAttributesContext(context.Background(), cfg, store)
}
func capacityAttributesContext(parent context.Context, cfg Config, store *Store) map[string]*pstructs.Attribute {
	binding, _ := json.Marshal([]any{cfg.StateDir, cfg.LimaHome, os.Geteuid(), os.Getegid()})
	out := map[string]*pstructs.Attribute{"driver.lima.capacity.known": pstructs.NewBoolAttribute(false), "driver.lima.capacity.attempted_unix": pstructs.NewIntAttribute(time.Now().Unix(), ""), "driver.lima.capacity.scope_sha256": pstructs.NewStringAttribute(lima.Hash(binding)), "driver.lima.capacity.effective_uid": pstructs.NewIntAttribute(int64(os.Geteuid()), ""), "driver.lima.capacity.effective_gid": pstructs.NewIntAttribute(int64(os.Getegid()), "")}
	reason := func(code string) map[string]*pstructs.Attribute {
		out["driver.lima.capacity.reason"] = pstructs.NewStringAttribute(code)
		return out
	}
	if store == nil {
		return reason("store_unconfigured")
	}
	ctx, cancel := context.WithTimeout(parent, 200*time.Millisecond)
	defer cancel()
	var lock *os.File
	for {
		if ctx.Err() != nil {
			if parent.Err() != nil {
				return reason("snapshot_wait_cancelled")
			}
			return reason("admission_lock_busy")
		}
		var err error
		lock, err = store.lock(lima.Hash([]byte("node-admission")))
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return reason("admission_lock_unavailable")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			if parent.Err() != nil {
				return reason("snapshot_wait_cancelled")
			}
			return reason("admission_lock_busy")
		case <-timer.C:
		}
	}
	defer unlock(lock)
	rows, err := (&Driver{store: store}).ledger()
	if err != nil {
		return reason("ledger_unreadable")
	}
	var count, cpu, mem, disk int64
	for _, s := range rows {
		purged, e := store.purged(s)
		if e != nil {
			return reason("purge_proof_unreadable")
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
		return reason("physical_statfs_unavailable")
	}
	values := map[string]int64{"observed_unix": time.Now().Unix(), "used_vms": count, "used_vcpus": cpu, "used_reservation_mib": mem, "used_disk_gib": disk, "physical_available_mib": int64(uint64(fs.Bavail) * uint64(fs.Bsize) / 1048576), "disk_floor_mib": int64(cfg.DiskFreeFloorMiB)}
	out["driver.lima.capacity.known"] = pstructs.NewBoolAttribute(true)
	for k, v := range values {
		out["driver.lima.capacity."+k] = pstructs.NewIntAttribute(v, "")
	}
	return reason("ready")
}
