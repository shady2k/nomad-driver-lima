package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

const finalizeControl = "/__nomad_driver_lima/finalize-v1"

type FinishRequest struct {
	Action            string `json:"action"`
	Allocation        string `json:"allocation"`
	Session           string `json:"session"`
	CreatorAgent      string `json:"creator_agent_id"`
	CreatorSession    string `json:"creator_session_id"`
	CreatorGeneration string `json:"creator_generation"`
	Request           string `json:"request_id"`
	Snapshot          string `json:"snapshot_id"`
	ManifestSHA       string `json:"manifest_sha256"`
	ReceiptSHA        string `json:"s3_receipt_sha256"`
	Backend           string `json:"backend"`
	ActivityRevision  int64  `json:"activity_revision"`
}

// Nomad's authenticated allocation-exec caller asserts verified S3 durability.
// This is not a guest log assertion or an independent S3 verification by driver.
type FinalizationState struct {
	Request                                       FinishRequest
	TaskID, SpecHash, MachineID, BootID, Instance string
	Generation                                    uint64
	ConfirmedAt                                   time.Time
}

func (d *Driver) finalizationControl(ctx context.Context, id string, cmd []string) ([]byte, error) {
	if len(cmd) != 2 || len(cmd[1]) > 8192 {
		return nil, fmt.Errorf("FINALIZATION_CONTROL_SCHEMA")
	}
	var r FinishRequest
	decoder := json.NewDecoder(strings.NewReader(cmd[1]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil {
		return nil, fmt.Errorf("FINALIZATION_CONTROL_SCHEMA")
	}
	var tail any
	if decoder.Decode(&tail) != io.EOF {
		return nil, fmt.Errorf("FINALIZATION_CONTROL_SCHEMA")
	}
	if (r.Action != "preflight" && r.Action != "arm") || r.Backend != "s3" || !recoveryUUID.MatchString(r.Allocation) || !recoveryUUID.MatchString(r.Session) || !recoveryUUID.MatchString(r.CreatorAgent) || !recoveryUUID.MatchString(r.CreatorGeneration) || r.CreatorSession == "" || !recoveryUUID.MatchString(r.Request) || !recoveryUUID.MatchString(r.Snapshot) || !recoveryKey.MatchString(r.ManifestSHA) || !recoveryKey.MatchString(r.ReceiptSHA) || r.ActivityRevision < 0 {
		return nil, fmt.Errorf("VERIFIED_S3_FINISH_BINDING_REQUIRED")
	}
	t, e := d.get(id)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	if s.AllocID != r.Allocation || s.WorkerSessionID != r.Session || s.CreatorAgentID != r.CreatorAgent || s.CreatorSessionID != r.CreatorSession {
		return nil, fmt.Errorf("FINALIZATION_OWNER_OR_SESSION_MISMATCH")
	}
	expected := t.cfg.Env["NOMAD_META_creator_generation"]
	if expected == "" {
		expected = t.cfg.Env["NOMAD_META_CREATOR_GENERATION"]
	}
	if expected == "" || expected != r.CreatorGeneration {
		return nil, fmt.Errorf("FINALIZATION_NATIVE_GENERATION_MISMATCH")
	}
	lock, e := d.store.lockMode(lima.Hash([]byte("instance-operation:"+s.Instance)), syscall.LOCK_EX)
	if e != nil {
		return nil, fmt.Errorf("FINALIZATION_ACTIVE_OPERATION_RETAINED")
	}
	defer unlock(lock)
	// Re-read under the same exclusive fence used by command/stop operations.
	t.mu.Lock()
	s = *t.state
	t.mu.Unlock()
	normalized := r
	normalized.Action = "arm"
	if s.Finalization != nil {
		if !reflect.DeepEqual(s.Finalization.Request, normalized) || s.Finalization.SpecHash != s.SpecHash || s.Finalization.TaskID != s.TaskID {
			return nil, fmt.Errorf("FINALIZATION_CANDIDATE_CHANGED")
		}
		return json.Marshal(map[string]any{"protocol": "finish-v1", "armed": true, "manifest_sha256": r.ManifestSHA, "allocation": s.AllocID, "session": s.WorkerSessionID})
	}
	if s.Exit != nil || s.Destroyed || s.VaultFinalizing || s.ResumeNext != nil || s.MachineID == "" || s.BootID == "" {
		return nil, fmt.Errorf("FINALIZATION_SOURCE_NOT_ACTIVE_RETAINED")
	}
	var cfg TaskConfig
	if t.cfg.DecodeDriverConfig(&cfg) != nil || !cfg.VaultRequired {
		return nil, fmt.Errorf("FINALIZATION_CONFIRMED_VAULT_LIFECYCLE_REQUIRED")
	}
	info, e := d.owned(ctx, &s)
	if e != nil || info.Status != "Running" {
		return nil, fmt.Errorf("FINALIZATION_VM_IDENTITY_UNCONFIRMED")
	}
	_, identity, e := d.client.Guest(ctx, s.Instance, d.payload(&s, "identity"))
	if e != nil || identity.MachineID != s.MachineID || identity.BootID != s.BootID {
		return nil, fmt.Errorf("FINALIZATION_BOOT_IDENTITY_CHANGED")
	}
	if r.Action == "arm" {
		t.vaultMu.Lock()
		t.vaultStopped = true
		t.mu.Lock()
		t.state.VaultFinalizing = true
		t.state.Finalization = &FinalizationState{Request: normalized, TaskID: s.TaskID, SpecHash: s.SpecHash, MachineID: s.MachineID, BootID: s.BootID, Instance: s.Instance, Generation: generation(&s), ConfirmedAt: time.Now().UTC()}
		t.state.Version = 4
		e = d.store.write(t.state)
		s = *t.state
		t.mu.Unlock()
		t.vaultMu.Unlock()
		if e != nil {
			return nil, e
		}
		d.emit(&s, "final_s3_receipt_bound")
	}
	return json.Marshal(map[string]any{"protocol": "finish-v1", "armed": r.Action == "arm", "manifest_sha256": r.ManifestSHA, "allocation": s.AllocID, "session": s.WorkerSessionID})
}
func (d *Driver) purgeFinalized(ctx context.Context, t *task) error {
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	if s.Finalization == nil {
		return nil
	}
	admission, e := d.store.lock(lima.Hash([]byte("node-admission")))
	if e != nil {
		return fmt.Errorf("FINAL_DISK_PURGE_ADMISSION_BUSY_RETAINED")
	}
	defer unlock(admission)
	if done, e := d.store.purged(&s); e != nil {
		return e
	} else if done {
		return nil
	}
	f := s.Finalization
	stop := s.StopReceipt
	if f.TaskID != s.TaskID || f.SpecHash != s.SpecHash || f.Instance != s.Instance || f.Generation != generation(&s) || f.MachineID != s.MachineID || f.BootID != s.BootID || f.Request.Allocation != s.AllocID || f.Request.Session != s.WorkerSessionID || s.Exit == nil || s.ResumeNext != nil || stop == nil || !stop.RevocationConfirmed || stop.Allocation != s.AllocID || stop.Session != s.WorkerSessionID || stop.Instance != s.Instance || stop.Generation != generation(&s) || stop.Disk == nil {
		return fmt.Errorf("FINAL_DISK_PURGE_STOP_OR_RECEIPT_UNCONFIRMED_RETAINED")
	}
	intentPath := filepath.Join(d.store.Root, s.Key+".purge-intent.audit")
	var raw []byte
	if s.PurgeIntentSHA256 == "" {
		info, e := d.owned(ctx, &s)
		if e != nil || info.Status != "Stopped" || info.DriverPID > 0 || info.HostAgentPID > 0 {
			return fmt.Errorf("FINAL_DISK_PURGE_VM_NOT_CONFIRMED_STOPPED")
		}
		disk, e := d.retainedDiskIdentity(s.Instance)
		if e != nil || !reflect.DeepEqual(disk, stop.Disk) {
			return fmt.Errorf("FINAL_DISK_PURGE_DISK_CHANGED_RETAINED")
		}
		dir, e := os.Lstat(filepath.Join(d.config.LimaHome, s.Instance))
		if e != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("FINAL_DISK_PURGE_DIRECTORY_UNSAFE")
		}
		st := dir.Sys().(*syscall.Stat_t)
		original, _ := json.Marshal(s)
		intent := purgeIntent{Version: 1, Original: s, Instance: s.Instance, NodeID: s.NodeID, JobID: t.cfg.JobID, Namespace: s.Namespace, ConfigHash: s.ConfigHash, DirDev: uint64(st.Dev), DirIno: st.Ino, CreatedAt: time.Now().UTC(), OriginalLedgerSHA256: lima.Hash(original)}
		raw, e = json.Marshal(intent)
		if e != nil {
			return e
		}
		if e = durablePurgeAudit(intentPath, raw); e != nil {
			return e
		}
		t.mu.Lock()
		t.state.PurgeIntentSHA256 = lima.Hash(raw)
		t.state.Phase = "purge_pending"
		e = d.store.write(t.state)
		s = *t.state
		t.mu.Unlock()
		if e != nil {
			return e
		}
		if e = d.client.Delete(ctx, s.Instance); e != nil {
			d.emit(&s, "disk_purge_unconfirmed")
			return fmt.Errorf("FINAL_DISK_DELETE_OUTCOME_UNKNOWN_RESERVATION_RETAINED")
		}
	} else {
		// No repeated delete after an uncertain dispatch. Only absence may complete it.
		raw, e = privateRecoveryRead(intentPath, 1<<20)
		if e != nil || lima.Hash(raw) != s.PurgeIntentSHA256 {
			return fmt.Errorf("FINAL_DISK_PURGE_INTENT_UNCONFIRMED")
		}
		var intent purgeIntent
		if json.Unmarshal(raw, &intent) != nil || intent.Version != 1 || intent.Original.Key != s.Key || intent.Original.Owner != s.Owner || intent.Original.AllocID != s.AllocID || intent.Instance != s.Instance || intent.Original.DiskGiB != s.DiskGiB || !reflect.DeepEqual(intent.Original.Finalization, s.Finalization) {
			return fmt.Errorf("FINAL_DISK_PURGE_INTENT_BINDING_CHANGED")
		}

	}
	if _, e = d.client.Get(ctx, s.Instance); !errors.Is(e, lima.ErrNotFound) {
		return fmt.Errorf("FINAL_DISK_ABSENCE_UNCONFIRMED_RESERVATION_RETAINED")
	}
	if _, e = os.Lstat(filepath.Join(d.config.LimaHome, s.Instance)); !os.IsNotExist(e) {
		return fmt.Errorf("FINAL_DISK_DIRECTORY_REMAINS_RESERVATION_RETAINED")
	}
	complete := purgeCompletion{1, s.Key, s.Instance, s.PurgeIntentSHA256, time.Now().UTC()}
	proof, _ := json.Marshal(complete)
	completionPath := filepath.Join(d.store.Root, s.Key+".purge-complete.audit")
	if old, er := privateRecoveryRead(completionPath, 1<<20); er == nil {
		var prior purgeCompletion
		if json.Unmarshal(old, &prior) != nil || prior.Key != s.Key || prior.Instance != s.Instance || prior.IntentSHA256 != s.PurgeIntentSHA256 || prior.AbsentVerifiedAt.IsZero() {
			return fmt.Errorf("FINAL_DISK_COMPLETION_AUDIT_CONFLICT")
		}
		proof = old
	} else if !os.IsNotExist(er) {
		return er
	}
	if e = durablePurgeAudit(filepath.Join(d.store.Root, s.Key+".purge-complete.audit"), proof); e != nil {
		return e
	}
	t.mu.Lock()
	t.state.PurgedDiskGiB = t.state.DiskGiB
	t.state.DiskGiB = 0
	t.state.PurgeAuditSHA256 = lima.Hash(proof)
	t.state.Phase = "destroyed_disk_purged"
	t.state.Destroyed = true
	e = d.store.write(t.state)
	s = *t.state
	t.mu.Unlock()
	if e == nil {
		d.emit(&s, "vm_disk_purged")
	}
	return e
}

// Terminal recovered tasks never reopen the guest or a completed log FIFO.
type finishedWriter struct{}

func (finishedWriter) Write(p []byte) (int, error) { return len(p), nil }
func (finishedWriter) Close() error                { return nil }

func validFinalizationState(s *State) bool {
	f := s.Finalization
	if f == nil {
		return false
	}
	r := f.Request
	return (s.Version == 4 || s.Version == 5) && s.VaultFinalizing && f.TaskID == s.TaskID && f.SpecHash == s.SpecHash && f.Instance == s.Instance && f.Generation == generation(s) && f.MachineID == s.MachineID && f.BootID == s.BootID && !f.ConfirmedAt.IsZero() && r.Action == "arm" && r.Backend == "s3" && r.Allocation == s.AllocID && r.Session == s.WorkerSessionID && r.CreatorAgent == s.CreatorAgentID && r.CreatorSession == s.CreatorSessionID && recoveryUUID.MatchString(r.CreatorGeneration) && recoveryUUID.MatchString(r.Request) && recoveryUUID.MatchString(r.Snapshot) && recoveryKey.MatchString(r.ManifestSHA) && recoveryKey.MatchString(r.ReceiptSHA) && r.ActivityRevision >= 0
}
