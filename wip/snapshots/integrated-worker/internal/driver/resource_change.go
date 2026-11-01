package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
)

const maxResourceMemoryMiB = int64(^uint64(0)>>1) / (1048576 * 3)

type ResourceValues struct{ CPUs, MemoryMiB, DiskGiB int }
type ResourceChangeIntent struct {
	Current, Desired                                ResourceValues
	Reason, OriginalConfigSHA256, NonResourceSHA256 string
	OriginalDisk                                    RetainedDiskIdentity
	Before                                          *lima.ResourceObservation
}
type resourceConfigAudit struct {
	Key, Owner, Instance, IntentSHA256, ConfigSHA256, NonResourceSHA256 string
}

func resourceValues(s *State) ResourceValues { return ResourceValues{s.CPUs, s.MemoryMiB, s.DiskGiB} }
func validChangeReason(reason string) bool {
	return strings.TrimSpace(reason) != "" && len(reason) <= 1024 && !strings.ContainsAny(reason, "\x00\r\n")
}
func changeIntentHash(r *ResourceChangeIntent) string { b, _ := json.Marshal(r); return lima.Hash(b) }
func validGrowthBaseline(r *lima.ResourceObservation, diskGiB int) bool {
	return r != nil && r.DirectPartition && (r.FSType == "ext4" || r.FSType == "xfs") && r.FSUUID != "" && len(r.FSUUID) <= 128 && r.DiskBytes == int64(diskGiB)*1073741824 && r.PartitionBytes > 0 && r.FilesystemBytes > 0 && r.FilesystemBytes <= r.PartitionBytes && r.PartitionBytes <= r.DiskBytes
}
func (d *Driver) prepareResourceChange(source *State, tc TaskConfig) (*ResourceChangeIntent, error) {
	desired := ResourceValues{tc.CPUs, tc.MemoryMiB, tc.DiskGiB}
	current := resourceValues(source)
	if tc.ResourceChangeReason == "" {
		if desired != current {
			return nil, fmt.Errorf("explicit resource change reason required")
		}
		return nil, nil
	}
	if d.config.ExpectedVersion != "2.2.1" {
		return nil, fmt.Errorf("resource changes require reviewed Lima 2.2.1")
	}
	if !validChangeReason(tc.ResourceChangeReason) || desired == current {
		return nil, fmt.Errorf("bounded reason and an actual resource change required")
	}
	if desired.DiskGiB < current.DiskGiB {
		return nil, fmt.Errorf("disk shrink is forbidden")
	}
	if desired.CPUs < 1 || desired.MemoryMiB < 256 || int64(desired.MemoryMiB) > maxResourceMemoryMiB || desired.DiskGiB < 1 {
		return nil, fmt.Errorf("invalid desired resources")
	}
	if source.ResolvedVMType != "" && source.ResolvedVMType != "qemu" && source.ResolvedVMType != "vz" {
		return nil, fmt.Errorf("resource change requires builtin QEMU or VZ")
	}
	if desired.DiskGiB > current.DiskGiB && !validGrowthBaseline(source.StopReceipt.Resources, current.DiskGiB) {
		return nil, fmt.Errorf("disk growth requires stopped receipt with direct ext4/xfs root observation")
	}
	versionCtx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	version, e := d.client.Version(versionCtx)
	if e != nil || version != "limactl version 2.2.1" {
		return nil, fmt.Errorf("actual native Lima version differs from reviewed 2.2.1")
	}
	fingerprint, e := d.client.NonResourceConfigHash(source.Instance)
	if e != nil {
		return nil, e
	}
	return &ResourceChangeIntent{Current: current, Desired: desired, Reason: tc.ResourceChangeReason, OriginalConfigSHA256: source.ConfigHash, NonResourceSHA256: fingerprint, OriginalDisk: *source.StopReceipt.Disk, Before: source.StopReceipt.Resources}, nil
}
func validResourceIntent(source, next *State) bool {
	r := next.ResourceChange
	if r == nil {
		return resourceValues(source) == resourceValues(next)
	}
	if !validChangeReason(r.Reason) || r.Current != resourceValues(source) || r.Desired != resourceValues(next) || r.Current == r.Desired || r.Desired.DiskGiB < r.Current.DiskGiB || r.Desired.CPUs < 1 || r.Desired.MemoryMiB < 256 || int64(r.Desired.MemoryMiB) > maxResourceMemoryMiB || r.Desired.DiskGiB > 1048576 || !recoveryKey.MatchString(r.OriginalConfigSHA256) || r.OriginalConfigSHA256 != source.ConfigHash || !recoveryKey.MatchString(r.NonResourceSHA256) || source.StopReceipt == nil || source.StopReceipt.Disk == nil || r.OriginalDisk != *source.StopReceipt.Disk || !reflect.DeepEqual(r.Before, source.StopReceipt.Resources) {
		return false
	}
	return r.Desired.DiskGiB == r.Current.DiskGiB || validGrowthBaseline(r.Before, r.Current.DiskGiB)
}
func (s *Store) resourceConfigProof(state *State) error {
	if state.ResourceChange == nil || !recoveryKey.MatchString(state.ResourceChangeAuditSHA256) {
		return fmt.Errorf("resource config proof missing")
	}
	b, e := privateRecoveryRead(filepath.Join(s.Root, state.Key+".resource-config.audit"), 1<<20)
	if e != nil || lima.Hash(b) != state.ResourceChangeAuditSHA256 {
		return fmt.Errorf("resource config audit changed")
	}
	var a resourceConfigAudit
	if json.Unmarshal(b, &a) != nil || a.Key != state.Key || a.Owner != s.Owner || a.Instance != state.Instance || a.IntentSHA256 != changeIntentHash(state.ResourceChange) || a.ConfigSHA256 != state.ConfigHash || a.NonResourceSHA256 != state.ResourceChange.NonResourceSHA256 {
		return fmt.Errorf("resource config audit identity mismatch")
	}
	return nil
}
func configHasResources(v *lima.Info, r ResourceValues) bool {
	return v.CPUs == r.CPUs && v.Memory == int64(r.MemoryMiB)*1048576 && v.Disk == int64(r.DiskGiB)*1073741824
}

// Configuration update is its own durable transaction; image resize happens in
// Lima's next ordinary start. No return code alone proves either stage finished.
func (d *Driver) applyResourceConfig(ctx context.Context, s *State) error {
	r := s.ResourceChange
	if r == nil {
		return nil
	}
	lock, e := d.store.lock(lima.Hash([]byte("instance-operation:" + s.Instance)))
	if e != nil {
		return e
	}
	defer unlock(lock)
	version, e := d.client.Version(ctx)
	if e != nil || version != "limactl version 2.2.1" {
		return fmt.Errorf("native Lima version changed before edit")
	}
	if e = d.currentOwner(s); e != nil {
		return e
	}
	v, e := d.client.Get(ctx, s.Instance)
	if e != nil {
		return e
	}
	if v.VMType != "qemu" && v.VMType != "vz" {
		return fmt.Errorf("resource change supports builtin QEMU/VZ only")
	}
	if v.Param["nomad_driver_id"] != s.Owner || v.Param["nomad_task_key"] != instanceKey(s) || filepath.Clean(v.Dir) != filepath.Join(d.config.LimaHome, s.Instance) {
		return fmt.Errorf("resource edit ownership mismatch")
	}
	fingerprint, e := d.client.NonResourceConfigHash(s.Instance)
	if e != nil || fingerprint != r.NonResourceSHA256 {
		return fmt.Errorf("non-resource Lima settings changed; reservation retained")
	}
	hash, e := d.client.ConfigHash(s.Instance)
	if e != nil {
		return e
	}
	if s.ResourceChangeAuditSHA256 != "" {
		if e = d.store.resourceConfigProof(s); e != nil {
			return e
		}
		if hash != s.ConfigHash || !configHasResources(v, r.Desired) {
			return fmt.Errorf("configured resources changed")
		}
		return nil
	}
	if v.Status != "Stopped" || v.DriverPID > 0 || v.HostAgentPID > 0 {
		return fmt.Errorf("resource edit requires confirmed stopped VM; interruption is not automatic")
	}
	disk, e := d.retainedDiskIdentity(s.Instance)
	if e != nil || *disk != r.OriginalDisk {
		return fmt.Errorf("retained disk changed before resource edit")
	}
	if configHasResources(v, r.Current) && hash == r.OriginalConfigSHA256 {
		// A pending/resumed allocation already owns the full desired reservation.
		editErr := d.client.EditResources(ctx, s.Instance, r.Desired.CPUs, r.Desired.MemoryMiB, r.Desired.DiskGiB)
		v, e = d.client.Get(ctx, s.Instance)
		if e != nil {
			return fmt.Errorf("resource edit outcome unknown; reservation retained")
		}
		if !configHasResources(v, r.Desired) {
			return fmt.Errorf("resource edit pending or unknown; no quota rollback: %v", editErr)
		}
	} else if !configHasResources(v, r.Desired) {
		return fmt.Errorf("resource edit configuration differs from both intent endpoints")
	}
	if v.Status != "Stopped" || v.DriverPID > 0 || v.HostAgentPID > 0 || v.Param["nomad_driver_id"] != s.Owner || v.Param["nomad_task_key"] != instanceKey(s) {
		return fmt.Errorf("resource edit postcondition ownership/stop unknown")
	}
	fingerprint, e = d.client.NonResourceConfigHash(s.Instance)
	if e != nil || fingerprint != r.NonResourceSHA256 {
		return fmt.Errorf("resource edit changed unrelated settings")
	}
	hash, e = d.client.ConfigHash(s.Instance)
	if e != nil {
		return e
	}
	audit := resourceConfigAudit{s.Key, s.Owner, s.Instance, changeIntentHash(r), hash, fingerprint}
	raw, _ := json.Marshal(audit)
	path := filepath.Join(d.store.Root, s.Key+".resource-config.audit")
	if e = durablePurgeAudit(path, raw); e != nil {
		return e
	}
	s.ConfigHash, s.ResourceChangeAuditSHA256, s.ResourceChangePhase = hash, lima.Hash(raw), "configured_awaiting_boot"
	return d.store.write(s)
}
func checkChangedObservation(intent *ResourceChangeIntent, o *lima.ResourceObservation) error {
	if o == nil || o.CPUs != intent.Desired.CPUs || o.MemoryBytes < int64(intent.Desired.MemoryMiB)*1048576*3/4 || o.MemoryBytes > int64(intent.Desired.MemoryMiB)*1048576 {
		return fmt.Errorf("desired guest CPU/RAM not observed")
	}
	delta := int64(intent.Desired.DiskGiB-intent.Current.DiskGiB) * 1073741824
	if delta == 0 {
		return nil
	}
	before := intent.Before
	if !validGrowthBaseline(o, intent.Desired.DiskGiB) || o.FSType != before.FSType || o.FSUUID != before.FSUUID || o.PartitionBytes < before.PartitionBytes+delta-16*1048576 || o.FilesystemBytes < before.FilesystemBytes+delta*9/10 {
		return fmt.Errorf("root filesystem growth unverified; no workload or credential delivery")
	}
	return nil
}
func (d *Driver) verifyChangedResources(ctx context.Context, s *State) error {
	if e := d.store.resourceConfigProof(s); e != nil {
		return e
	}
	// Read-only observations may wait for cloud-init's native growth. This code
	// never formats, repartitions, runs resize2fs/xfs_growfs, or changes a mount.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		snap, id, e := d.client.Guest(ctx, s.Instance, d.payload(s, "resource_status"))
		if e == nil && (id.MachineID != s.ResumeMachineID || id.BootID == s.ResumePreviousBootID) {
			return fmt.Errorf("resource observation guest identity changed")
		}
		if e == nil && checkChangedObservation(s.ResourceChange, snap.Resources) == nil {
			s.ResourceObservation = snap.Resources
			s.ResourceChangePhase = "hardware_rootfs_verified"
			return d.store.write(s)
		}
		// CPU/RAM and unsupported layouts are not cloud-init growth delays.
		if s.ResourceChange.Desired.DiskGiB == s.ResourceChange.Current.DiskGiB {
			return fmt.Errorf("desired CPU/RAM hardware unverified")
		}
		s.ResourceChangePhase = "rootfs_unverified"
		if err := d.store.write(s); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("rootfs growth pending/unsupported; desired reservation retained, no launch")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func verifyRequestedState(cfg *drivers.TaskConfig, tc TaskConfig, s *State) error {
	if s.AllocID != cfg.AllocID || s.NodeID != cfg.NodeID || s.Namespace != cfg.Namespace || s.CPUs != tc.CPUs || s.MemoryMiB != tc.MemoryMiB || s.DiskGiB != tc.DiskGiB || s.WorkerSessionID != tc.WorkerSessionID {
		return fmt.Errorf("durable allocation/resources differ from requested reservation")
	}
	if s.ResourceChange != nil && (s.ResourceChange.Reason != tc.ResourceChangeReason || s.ResourceChange.Desired != resourceValues(s)) {
		return fmt.Errorf("resource transaction differs from requested change")
	}
	return nil
}

// A resource-change allocation must not fail and silently leave its unverified
// pre-credential boot consuming hardware after Nomad releases the reservation.
// This cleanup applies only to this new generation, never an active source VM.
func (d *Driver) quiesceUnverifiedResourceBoot(s *State) {
	if s.ResourceChange == nil || s.ResourceCredentialDeliveryStarted || !s.StartedAt.IsZero() || d.store.resourceConfigProof(s) != nil {
		return
	}
	current, e := d.store.read(s.Key)
	if e != nil || current.ResourceCredentialDeliveryStarted {
		return
	}
	exposures, e := d.store.vaultExposures(s)
	if e != nil || len(exposures) > 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lock, e := d.store.lock(lima.Hash([]byte("instance-operation:" + s.Instance)))
	if e != nil {
		return
	}
	defer unlock(lock)
	if _, e = d.owned(ctx, s); e != nil {
		return
	}
	v, e := d.client.Get(ctx, s.Instance)
	if e != nil {
		return
	}
	if v.Status == "Running" {
		s.ResourceChangePhase = "unverified_boot_cleanup_pending"
		if e = d.store.write(s); e != nil {
			return
		}
		d.emit(s, "resource_change_cleanup_pending")
		if e = d.client.Stop(ctx, s.Instance); e != nil {
			return
		}
	}
	v, e = d.client.Get(ctx, s.Instance)
	if e == nil && v.Status == "Stopped" && v.DriverPID == 0 && v.HostAgentPID == 0 {
		s.ResourceChangePhase = "unverified_boot_stopped"
		s.MachineID, s.BootID = "", ""
		s.ResourceObservation = nil
		if d.store.write(s) == nil {
			d.emit(s, "resource_change_unverified_boot_stopped")
		}
	}
}
