package driver

import (
	"context"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"
)

func generation(s *State) uint64 {
	if s.Generation == 0 {
		return 1
	}
	return s.Generation
}
func instanceKey(s *State) string {
	if s.InstanceKey != "" {
		return s.InstanceKey
	}
	return s.Key
}

// Durable ownership is authoritative even for cached old handles.
func (d *Driver) currentOwner(s *State) error {
	current, err := d.store.read(s.Key)
	if err != nil {
		return err
	}
	if current.ResumeNext != nil || current.Instance != s.Instance || generation(current) != generation(s) {
		return fmt.Errorf("allocation no longer owns instance")
	}
	return nil
}
func validateResumeSource(source *State, cfg *drivers.TaskConfig, tc TaskConfig) error {
	if source.Namespace == "" || cfg.Namespace != source.Namespace {
		return fmt.Errorf("resume namespace owner mismatch")
	}
	r := source.StopReceipt
	if source.Phase != "stopped" && source.Phase != "destroyed_disk_retained" {
		return fmt.Errorf("resume requires stopped retained source")
	}
	if source.ResumeNext != nil || source.Exit == nil || r == nil || !r.RevocationConfirmed || r.ConfirmedAt.IsZero() || r.ConfirmedAt.After(time.Now()) || r.Allocation != source.AllocID || r.Instance != source.Instance || r.Session != source.WorkerSessionID || r.Generation != generation(source) {
		return fmt.Errorf("resume requires exact confirmed stop/revocation receipt")
	}
	if !recoveryKey.MatchString(r.TokenSHA256) || len(r.ExposedTokenSHA256) == 0 || r.Disk == nil || (r.Disk.Size < 1 || r.Disk.Ino == 0 || (r.Disk.Name != "disk" && r.Disk.Name != "diffdisk")) || source.MachineID == "" || source.BootID == "" {
		return fmt.Errorf("resume receipt token/disk identity malformed")
	}
	seenCurrent := false
	for _, h := range r.ExposedTokenSHA256 {
		if !recoveryKey.MatchString(h) {
			return fmt.Errorf("resume receipt token hash malformed")
		}
		if h == r.TokenSHA256 {
			seenCurrent = true
		}
	}
	if !seenCurrent {
		return fmt.Errorf("receipt current token absent from exposure history")
	}
	if source.NodeID == "" || source.NodeID != cfg.NodeID || tc.ResumeNodeID != source.NodeID {
		return fmt.Errorf("resume must be pinned to original node")
	}
	if cfg.AllocID == source.AllocID || cfg.ID == source.TaskID || tc.WorkerSessionID == source.WorkerSessionID || !vaultSessionID.MatchString(tc.WorkerSessionID) || !tc.VaultRequired {
		return fmt.Errorf("resume requires new allocation, session and task Vault token")
	}
	if tc.ResumeGeneration < 1 || uint64(tc.ResumeGeneration) != generation(source) {
		return fmt.Errorf("resume generation changed")
	}
	if tc.CPUs != source.CPUs || tc.MemoryMiB != source.MemoryMiB || tc.DiskGiB != source.DiskGiB || tc.TemplateID != source.TemplateID || tc.TemplateVersion != source.TemplateVersion || tc.TemplateDiskSHA256 != source.TemplateDiskSHA256 || tc.Architecture != source.ResolvedArchitecture {
		return fmt.Errorf("resume must preserve source resources/template/platform")
	}
	if err := source.RepositoryProfile.Validate(); err != nil {
		return err
	}
	if tc.RepositoryProfileID != "" || tc.RepositoryProfileSHA256 != "" {
		if source.RepositoryProfile == nil || tc.RepositoryProfileID != source.RepositoryProfile.Profile.ID || tc.RepositoryProfileSHA256 != source.RepositoryProfile.SHA256 {
			return fmt.Errorf("resume must preserve pinned repository profile")
		}
	}
	return nil
}

// Source commit is the irreversible transfer intent. Exact-allocation retries
// materialize its successor; a different allocation cannot steal the transfer.
func (d *Driver) prepareResume(cfg *drivers.TaskConfig, tc TaskConfig, spec, key string) (*State, error) {
	if !recoveryKey.MatchString(tc.ResumeFromKey) || tc.ResumeFromKey == key {
		return nil, fmt.Errorf("exact resume source key required")
	}
	source, err := d.store.read(tc.ResumeFromKey)
	if err != nil {
		return nil, err
	}
	operation, err := d.store.lock(lima.Hash([]byte("instance-operation:" + source.Instance)))
	if err != nil {
		return nil, err
	}
	defer unlock(operation)
	sourceLock, err := d.store.lock(source.Key)
	if err != nil {
		return nil, err
	}
	defer unlock(sourceLock)
	admission, err := d.store.lock(lima.Hash([]byte("node-admission")))
	if err != nil {
		return nil, err
	}
	defer unlock(admission)
	source, err = d.store.read(source.Key)
	if err != nil {
		return nil, err
	}
	if source.ResumeNext != nil {
		next := source.ResumeNext
		if next.Key != key || next.SpecHash != spec || next.AllocID != cfg.AllocID || next.NodeID != cfg.NodeID {
			return nil, fmt.Errorf("resume intent belongs to another allocation")
		}
		if err = d.store.write(next); err != nil {
			return nil, err
		}
		return next, nil
	}
	if err = validateResumeSource(source, cfg, tc); err != nil {
		return nil, err
	}
	token, err := readVaultToken(cfg)
	if err != nil {
		return nil, err
	}
	recorded, err := d.store.vaultExposures(source)
	if err != nil || !reflect.DeepEqual(recorded, source.StopReceipt.ExposedTokenSHA256) {
		return nil, fmt.Errorf("stop receipt exposure history changed")
	}
	history, err := d.instanceHistory(source.Instance)
	if err != nil {
		return nil, err
	}
	for _, previous := range history {
		if previous.WorkerSessionID == tc.WorkerSessionID {
			return nil, fmt.Errorf("worker session already used by this instance")
		}
	}
	probe := *source
	probe.ResumedFrom = source.Key
	if err = d.freshResumeToken(&probe, lima.Hash([]byte(token))); err != nil {
		return nil, err
	}
	disk, err := d.retainedDiskIdentity(source.Instance)
	if err != nil || !reflect.DeepEqual(disk, source.StopReceipt.Disk) {
		return nil, fmt.Errorf("retained source disk identity changed")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	v, err := d.owned(ctx, source)
	if err != nil {
		return nil, err
	}
	if v.Status != "Stopped" || v.DriverPID > 0 || v.HostAgentPID > 0 {
		return nil, fmt.Errorf("resume source stop not confirmed")
	}
	request := tc
	request.DiskGiB = 0 // Source already reserves this same disk.
	if err = d.checkBudget(request); err != nil {
		return nil, err
	}
	next := *source
	next.Key, next.TaskID, next.AllocID, next.TaskName, next.SpecHash = key, cfg.ID, cfg.AllocID, cfg.Name, spec
	next.InstanceKey, next.ResumedFrom, next.Generation = instanceKey(source), source.Key, generation(source)+1
	next.WorkerSessionID = tc.WorkerSessionID
	next.CreatorAgentID, next.CreatorSessionID = tc.CreatorAgentID, tc.CreatorSessionID
	next.Purpose, next.Project, next.TaskLink = tc.Purpose, tc.Project, tc.TaskLink
	next.ResumeNext, next.StopReceipt, next.Exit = nil, nil, nil
	next.Destroyed, next.VaultFinalizing = false, false
	next.ResumeMachineID, next.ResumePreviousBootID = source.MachineID, source.BootID
	next.MachineID, next.BootID, next.VaultTokenSHA256 = "", "", ""
	next.StartedAt, next.CompletedAt, next.StageStartedAt = time.Time{}, time.Time{}, time.Time{}
	next.StageName, next.Phase = "", "resume_intent"
	next.OutOffset, next.ErrOffset = 0, 0
	next.Lifecycle = ActivityObservation{}
	source.ResumeNext = &next
	if err = d.store.write(source); err != nil {
		return nil, err
	}
	if err = d.store.write(&next); err != nil {
		return nil, err
	}
	d.emit(source, "resumed_to")
	d.emit(&next, "resumed_from")
	return &next, nil
}

// Admission projects a successor even after a crash between the two commits.
// The source and successor never reserve this disk twice.
func resumeLedger(rows []*State, store *Store) ([]*State, error) {
	byKey := map[string]*State{}
	for _, s := range rows {
		byKey[s.Key] = s
	}
	for _, s := range rows {
		if n := s.ResumeNext; n != nil {
			if err := validResumeIntent(s, n); err != nil {
				return nil, err
			}
			if actual := byKey[n.Key]; actual != nil {
				if err := validResumeSuccessor(n, actual, store); err != nil {
					return nil, err
				}
			} else {
				byKey[n.Key] = n
			}
		}
	}
	out := []*State{}
	for _, s := range byKey {
		if s.ResumeNext == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// Reconcile only the host-side journal. This never boots a VM, delivers a
// credential, launches a runtime, or replays a workload after driver restart.
func (s *Store) reconcileResumeIntents() error {
	admission, err := s.lock(lima.Hash([]byte("node-admission")))
	if err != nil {
		return err
	}
	defer unlock(admission)
	rows, err := (&Driver{store: s}).ledger()
	if err != nil {
		return err
	}
	for _, next := range rows {
		if next.ResumedFrom == "" {
			continue
		}
		if _, err = s.read(next.Key); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		lock, err := s.lock(next.Key)
		if err != nil {
			continue
		}
		// The task lock prevents a concurrent StartTask from being overwritten.
		if _, err = s.read(next.Key); os.IsNotExist(err) {
			err = s.write(next)
		}
		unlock(lock)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) lockTaskOperation(id string) (*os.File, error) {
	t, e := d.get(id)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	instance := t.state.Instance
	t.mu.Unlock()
	if t.operationCtx != nil && t.operationCtx.Err() != nil {
		return nil, fmt.Errorf("task operations sealed for stop")
	}
	lock, e := d.store.lockMode(lima.Hash([]byte("instance-operation:"+instance)), syscall.LOCK_SH)
	if e == nil && t.operationCtx != nil && t.operationCtx.Err() != nil {
		unlock(lock)
		return nil, fmt.Errorf("task operations sealed for stop")
	}
	return lock, e
}
func (d *Driver) instanceHistory(instance string) ([]*State, error) {
	paths, e := filepath.Glob(filepath.Join(d.store.Root, "*.json"))
	if e != nil {
		return nil, e
	}
	var out []*State
	for _, p := range paths {
		key := filepath.Base(p)
		s, e := d.store.read(key[:len(key)-5])
		if e != nil {
			return nil, e
		}
		if s.Instance == instance {
			out = append(out, s)
		}
	}
	return out, nil
}
func (d *Driver) freshResumeToken(s *State, hash string) error {
	if s.ResumedFrom == "" {
		return nil
	}
	rows, e := d.instanceHistory(s.Instance)
	if e != nil {
		return e
	}
	for _, old := range rows {
		if old.Key == s.Key && s.ResumedFrom != s.Key {
			continue
		}
		hashes, e := d.store.vaultExposures(old)
		if e != nil {
			return e
		}
		receipt := old.StopReceipt
		if receipt == nil || !receipt.RevocationConfirmed || !recoveryKey.MatchString(receipt.TokenSHA256) || len(receipt.ExposedTokenSHA256) == 0 || !reflect.DeepEqual(hashes, receipt.ExposedTokenSHA256) {
			return fmt.Errorf("ancestor token exposure history missing or changed")
		}
		for _, previous := range hashes {
			if previous == hash {
				return fmt.Errorf("fresh allocation Vault token required; ancestor token rejected")
			}
		}
	}
	return nil
}
func (d *Driver) retainedDiskIdentity(instance string) (*RetainedDiskIdentity, error) {
	var disk *RetainedDiskIdentity
	for _, name := range []string{"disk", "diffdisk"} {
		st, e := os.Lstat(filepath.Join(d.config.LimaHome, instance, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if !st.Mode().IsRegular() || st.Size() < 1 || disk != nil {
			return nil, fmt.Errorf("unambiguous regular retained disk required")
		}
		stat := st.Sys().(*syscall.Stat_t)
		disk = &RetainedDiskIdentity{Name: name, Dev: uint64(stat.Dev), Ino: uint64(stat.Ino), Size: st.Size()}
	}
	if disk == nil {
		return nil, fmt.Errorf("retained writable disk missing")
	}
	return disk, nil
}

func sameResumeBinding(a, b *State) bool {
	return reflect.DeepEqual(a.PrivateCopy, b.PrivateCopy) && reflect.DeepEqual(a.RepositoryProfile, b.RepositoryProfile) && a.Version == b.Version && a.Owner == b.Owner && a.Key == b.Key && a.TaskID == b.TaskID && a.TaskName == b.TaskName && a.AllocID == b.AllocID && a.SpecHash == b.SpecHash && a.ResumedFrom == b.ResumedFrom && a.Instance == b.Instance && a.InstanceKey == b.InstanceKey && a.NodeID == b.NodeID && a.Namespace == b.Namespace && a.Generation == b.Generation && a.CPUs == b.CPUs && a.MemoryMiB == b.MemoryMiB && a.WorkerSessionID == b.WorkerSessionID && a.TemplateID == b.TemplateID && a.TemplateVersion == b.TemplateVersion && a.TemplateDiskSHA256 == b.TemplateDiskSHA256 && a.TemplateCatalogSHA256 == b.TemplateCatalogSHA256 && a.TemplateYAMLSHA256 == b.TemplateYAMLSHA256 && a.ResolvedArchitecture == b.ResolvedArchitecture && a.ResolvedVMType == b.ResolvedVMType && a.ConfigHash == b.ConfigHash && a.CreatorAgentID == b.CreatorAgentID && a.CreatorSessionID == b.CreatorSessionID && a.Purpose == b.Purpose && a.Project == b.Project && a.TaskLink == b.TaskLink && a.ResumeMachineID == b.ResumeMachineID && a.ResumePreviousBootID == b.ResumePreviousBootID
}
func validResumeIntent(source, next *State) error {
	expected := *next
	expected.Version = source.Version
	expected.RepositoryProfile = source.RepositoryProfile
	expected.PrivateCopy = source.PrivateCopy
	expected.Owner, expected.Instance, expected.InstanceKey = source.Owner, source.Instance, instanceKey(source)
	expected.NodeID, expected.Namespace = source.NodeID, source.Namespace
	expected.Generation, expected.ResumedFrom = generation(source)+1, source.Key
	expected.CPUs, expected.MemoryMiB = source.CPUs, source.MemoryMiB
	expected.TemplateID, expected.TemplateVersion, expected.TemplateDiskSHA256 = source.TemplateID, source.TemplateVersion, source.TemplateDiskSHA256
	expected.TemplateCatalogSHA256, expected.TemplateYAMLSHA256 = source.TemplateCatalogSHA256, source.TemplateYAMLSHA256
	expected.ResolvedArchitecture, expected.ResolvedVMType, expected.ConfigHash = source.ResolvedArchitecture, source.ResolvedVMType, source.ConfigHash
	expected.ResumeMachineID, expected.ResumePreviousBootID = source.MachineID, source.BootID
	if !sameResumeBinding(&expected, next) || next.Key == source.Key || next.Key != lima.Hash([]byte(next.TaskID)) || !recoveryKey.MatchString(next.SpecHash) || next.AllocID == source.AllocID || !vaultSessionID.MatchString(next.AllocID) || next.WorkerSessionID == source.WorkerSessionID || !vaultSessionID.MatchString(next.WorkerSessionID) || next.DiskGiB != source.DiskGiB || next.DiskGiB < 1 || next.Phase != "resume_intent" || next.Destroyed || next.VaultFinalizing || next.Exit != nil || next.StopReceipt != nil || next.ResumeNext != nil || next.MachineID != "" || next.BootID != "" || !next.StartedAt.IsZero() || !next.CompletedAt.IsZero() || next.OutOffset != 0 || next.ErrOffset != 0 {
		return fmt.Errorf("invalid resume intent; admission retained")
	}
	return nil
}
func validResumeSuccessor(intent, actual *State, store *Store) error {
	allowed := map[string]bool{"resume_intent": true, "starting": true, "launching": true, "running": true, "exited": true, "stopped": true, "destroyed_disk_retained": true, "purge_pending": true, "destroyed_disk_purged": true}
	if !allowed[actual.Phase] || (actual.Destroyed && actual.Phase != "destroyed_disk_retained" && actual.Phase != "purge_pending" && actual.Phase != "destroyed_disk_purged") || ((actual.Phase == "destroyed_disk_retained" || actual.Phase == "destroyed_disk_purged") && !actual.Destroyed) {
		return fmt.Errorf("resume successor lifecycle/reservation mismatch")
	}
	if actual.MachineID != "" && actual.MachineID != intent.ResumeMachineID {
		return fmt.Errorf("resume successor machine identity changed")
	}
	if !sameResumeBinding(intent, actual) {
		return fmt.Errorf("resume successor immutable binding mismatch")
	}
	if actual.DiskGiB != intent.DiskGiB {
		purged, e := store.purged(actual)
		if e != nil {
			return e
		}
		if !purged || actual.PurgedDiskGiB != intent.DiskGiB {
			return fmt.Errorf("resume successor disk reservation mismatch")
		}
	}
	return nil
}

func (d *Driver) drainTaskOperations(t *task, instance string) (*os.File, error) {
	if t.cancelOperations != nil {
		t.cancelOperations()
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		lock, e := d.store.lock(lima.Hash([]byte("instance-operation:" + instance)))
		if e == nil {
			return lock, nil
		}
		select {
		case <-deadline.C:
			return nil, fmt.Errorf("task operations cancellation pending; retry stop after streams drain")
		case <-d.ctx.Done():
			return nil, d.ctx.Err()
		case <-tick.C:
		}
	}
}
