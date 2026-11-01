package driver

// This is an offline, exact-target operator maintenance operation. It reuses
// SDK cleanup and retained resume receipts; it never changes allocation bindings.
import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	log "github.com/hashicorp/go-hclog"
	"nomad-driver-lima/internal/lima"
)

type RecoveryPairTarget struct {
	TaskID       string `json:"task_id"`
	Key          string `json:"key"`
	Instance     string `json:"instance"`
	MachineID    string `json:"machine_id"`
	BootID       string `json:"boot_id"`
	LedgerSHA256 string `json:"ledger_sha256"`
}
type RecoveryPairPlan struct {
	Version   int                `json:"version"`
	Original  RecoveryPairTarget `json:"original"`
	Duplicate RecoveryPairTarget `json:"duplicate"`
}
type RetainedResumeBinding struct {
	Key              string `json:"key"`
	Allocation       string `json:"allocation"`
	Node             string `json:"node"`
	Namespace        string `json:"namespace"`
	Task             string `json:"task"`
	Session          string `json:"session"`
	CreatorAgentID   string `json:"creator_agent_id"`
	CreatorSessionID string `json:"creator_session_id"`
	Generation       uint64 `json:"generation"`
	ReceiptSHA256    string `json:"receipt_sha256"`
}
type RecoveryPairResult struct {
	ResumeSource            *RetainedResumeBinding `json:"resume_source,omitempty"`
	Phase                   string                 `json:"phase"`
	SharedToken             bool                   `json:"shared_token"`
	OriginalExposures       int                    `json:"original_exposures"`
	DuplicateExposures      int                    `json:"duplicate_exposures"`
	OriginalRetainedReceipt bool                   `json:"original_retained_receipt"`
	DuplicateRetained       bool                   `json:"duplicate_retained"`
	AuditSHA256             string                 `json:"audit_sha256,omitempty"`
}

// RecoverPair accepts only operator-owned non-secret config and an exact pinned
// plan. Confirmation must cover both runtime stops, token revoke and disk retain.
// Client must be quiesced: both durable task locks must be available before any
// guest effect. Other VM processes are neither signalled nor restarted.
func RecoverPair(ctx context.Context, cfg Config, plan RecoveryPairPlan, confirm bool) (RecoveryPairResult, error) {
	result := RecoveryPairResult{Phase: "preflight"}
	if !filepath.IsAbs(cfg.LimaBinary) || !filepath.IsAbs(cfg.LimaHome) || !filepath.IsAbs(cfg.StateDir) || !validVaultAddress(cfg.VaultAddress) || cfg.ExpectedVersion == "" {
		return result, fmt.Errorf("explicit existing runtime paths, HTTPS Vault address and Lima version required")
	}
	d := New(ctx, log.NewNullLogger())
	defer d.Shutdown(context.Background())
	identity, err := resolveRuntimeUser(cfg.RuntimeUser)
	if err != nil {
		return result, err
	}
	if err = identity.validateHome(cfg.LimaHome); err != nil {
		return result, err
	}
	if err = lima.ValidateHome(cfg.LimaHome); err != nil {
		return result, err
	}
	store, err := openStore(cfg.StateDir)
	if err != nil {
		return result, err
	}
	d.config = cfg
	d.store = store
	d.client = &lima.Client{Home: cfg.LimaHome, Runner: lima.CommandRunner{Binary: cfg.LimaBinary, Home: cfg.LimaHome, UserHome: identity.home, Credential: identity.credential}}
	version, err := d.client.Version(ctx)
	if err != nil || version != "limactl version "+cfg.ExpectedVersion {
		return result, fmt.Errorf("maintenance Lima version mismatch")
	}
	return d.recoverPair(plan, confirm)
}

func (d *Driver) recoveryPairTarget(target RecoveryPairTarget) (*State, error) {
	if !recoveryKey.MatchString(target.Key) || target.Key != lima.Hash([]byte(target.TaskID)) || !recoveryKey.MatchString(target.LedgerSHA256) || target.Instance == "" || target.MachineID == "" || target.BootID == "" {
		return nil, fmt.Errorf("exact recovery target identity required")
	}
	s, err := d.store.read(target.Key)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if lima.Hash(raw) != target.LedgerSHA256 || s.TaskID != target.TaskID || s.Instance != target.Instance || s.MachineID != target.MachineID || s.BootID != target.BootID || s.Destroyed || s.ResumeNext != nil {
		return nil, fmt.Errorf("recovery target changed; no stop")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	v, err := d.owned(ctx, s)
	if err != nil {
		return nil, err
	}
	if v.Status != "Running" {
		return nil, fmt.Errorf("pair recovery expects both exact running runtimes; inspect partial outcome separately")
	}
	_, ident, err := d.client.Guest(ctx, s.Instance, d.payload(s, "identity"))
	if err != nil || ident.MachineID != s.MachineID || ident.BootID != s.BootID {
		return nil, fmt.Errorf("recovery guest identity changed")
	}
	return s, nil
}

func coversExposures(source, duplicate []string) bool {
	if len(source) == 0 || len(duplicate) == 0 {
		return false
	}
	known := map[string]bool{}
	for _, hash := range source {
		known[hash] = true
	}
	for _, hash := range duplicate {
		if !known[hash] {
			return false
		}
	}
	return true
}

func (d *Driver) recoverPair(plan RecoveryPairPlan, confirm bool) (RecoveryPairResult, error) {
	result := RecoveryPairResult{Phase: "preflight"}
	if plan.Version != 1 || plan.Original.TaskID == plan.Duplicate.TaskID || plan.Original.Instance == plan.Duplicate.Instance {
		return result, fmt.Errorf("distinct exact recovery pair required")
	}
	// Admission lock fences new starts during the whole operation. Task locks
	// additionally prove the client/plugin is no longer managing either handle.
	admission, err := d.store.lock(lima.Hash([]byte("node-admission")))
	if err != nil {
		return result, err
	}
	defer unlock(admission)
	original, err := d.recoveryPairTarget(plan.Original)
	if err != nil {
		return result, err
	}
	duplicate, err := d.recoveryPairTarget(plan.Duplicate)
	if err != nil {
		return result, err
	}
	if original.NodeID == "" || original.Namespace == "" || original.CreatorAgentID == "" || original.CreatorSessionID == "" || original.AllocID == "" || original.AllocID != duplicate.AllocID || original.TaskName != duplicate.TaskName || original.WorkerSessionID == "" || original.WorkerSessionID != duplicate.WorkerSessionID || original.CreatorAgentID != duplicate.CreatorAgentID || original.CreatorSessionID != duplicate.CreatorSessionID || original.Namespace != duplicate.Namespace || original.NodeID != duplicate.NodeID {
		return result, fmt.Errorf("recovery pair allocation/session/creator mismatch")
	}
	sourceHashes, err := d.store.vaultExposures(original)
	if err != nil {
		return result, err
	}
	duplicateHashes, err := d.store.vaultExposures(duplicate)
	if err != nil {
		return result, err
	}
	if !coversExposures(sourceHashes, duplicateHashes) || original.VaultTokenSHA256 == "" || original.VaultTokenSHA256 != duplicate.VaultTokenSHA256 {
		return result, fmt.Errorf("shared task-token exposure history not established; no stop")
	}
	// Do not revoke a token recorded for another surviving/uncertain runtime.
	// Token fingerprints are compared privately and never emitted.
	rows, err := d.ledger()
	if err != nil {
		return result, err
	}
	inventory, err := d.client.List(d.ctx)
	if err != nil {
		return result, fmt.Errorf("runtime inventory unavailable; no revoke")
	}
	stopped := map[string]bool{}
	for _, v := range inventory {
		stopped[v.Name] = v.Status == "Stopped" && v.DriverPID == 0 && v.HostAgentPID == 0
	}
	revoking := map[string]bool{}
	for _, hash := range sourceHashes {
		revoking[hash] = true
	}
	for _, other := range rows {
		if other.Key == original.Key || other.Key == duplicate.Key || stopped[other.Instance] || other.Phase == "no_vm_failed" {
			continue
		}
		purged, e := d.store.purged(other)
		if e != nil {
			return result, e
		}
		if purged {
			continue
		}
		history, e := d.store.vaultExposures(other)
		if e != nil {
			return result, e
		}
		if other.VaultTokenSHA256 != "" {
			history = append(history, other.VaultTokenSHA256)
		}
		for _, hash := range history {
			if revoking[hash] {
				return result, fmt.Errorf("task token also exposed to another surviving or uncertain runtime; no stop/revoke")
			}
		}
	}
	result.SharedToken = true
	result.OriginalExposures = len(sourceHashes)
	result.DuplicateExposures = len(duplicateHashes)
	if !confirm {
		return result, nil
	}
	first, releaseFirst, err := d.cleanupTask(original.TaskID)
	if err != nil {
		return result, err
	}
	defer releaseFirst()
	second, releaseSecond, err := d.cleanupTask(duplicate.TaskID)
	if err != nil {
		return result, err
	}
	defer releaseSecond()
	// Recheck the exact pinned state after both locks. No process effect precedes
	// this check or the fsynced intent audit.
	if _, err = d.recoveryPairTarget(plan.Original); err != nil {
		return result, err
	}
	if _, err = d.recoveryPairTarget(plan.Duplicate); err != nil {
		return result, err
	}
	raw, _ := json.Marshal(plan)
	auditPath := d.store.Root + "/" + lima.Hash(raw) + ".pair-recovery.intent.audit"
	if err = durablePurgeAudit(auditPath, raw); err != nil {
		return result, err
	}
	result.AuditSHA256 = lima.Hash(raw)
	result.Phase = "quiescing_both_runtimes"
	for _, t := range []*task{first, second} {
		operation, e := d.drainTaskOperations(t, t.state.Instance)
		if e != nil {
			return result, e
		}
		defer unlock(operation)
		// This branch is stop-only: no token read/revoke/clear. Thus neither shared
		// credential is revoked until both Prime/Bao runtime stops are confirmed.
		if e = d.client.StopVaultRuntime(d.ctx, t.state.Instance, t.state.WorkerSessionID, t.state.Owner); e != nil {
			return result, e
		}
	}
	result.Phase = "revoking_shared_token_and_stopping_original"
	if err = d.stopTask(original.TaskID, time.Second, "SIGTERM"); err != nil {
		return result, err
	}
	first.mu.Lock()
	finished := *first.state
	first.mu.Unlock()
	if finished.StopReceipt == nil || !finished.StopReceipt.RevocationConfirmed || !reflect.DeepEqual(finished.StopReceipt.ExposedTokenSHA256, sourceHashes) {
		return result, fmt.Errorf("original retained receipt not confirmed; no resume")
	}
	result.OriginalRetainedReceipt = true
	// All duplicate token hashes were already acknowledged by revoke-self for the
	// original. Do not submit another revoke (403 would be ambiguous). No forged
	// guest receipt or resumable duplicate receipt is created.
	result.Phase = "stopping_empty_duplicate"
	if _, err = d.owned(d.ctx, duplicate); err != nil {
		return result, err
	}
	if err = d.client.Stop(d.ctx, duplicate.Instance); err != nil {
		return result, err
	}
	v, err := d.owned(d.ctx, duplicate)
	if err != nil || v.Status != "Stopped" || v.DriverPID != 0 || v.HostAgentPID != 0 {
		return result, fmt.Errorf("duplicate physical stop unconfirmed; reservation retained")
	}
	second.mu.Lock()
	second.state.VaultFinalizing = true
	second.state.Phase = "stopped"
	second.state.Exit = &lima.Result{Code: 143, Signal: 15, Error: "empty_duplicate_stopped_shared_token_revoked_by_original_receipt"}
	second.state.CompletedAt = time.Now().UTC()
	second.state.StopReceipt = nil
	err = d.store.write(second.state)
	second.mu.Unlock()
	if err != nil {
		return result, err
	}
	second.once.Do(func() { close(second.done) })
	result.DuplicateRetained = true
	receiptJSON, _ := json.Marshal(finished.StopReceipt)
	result.ResumeSource = &RetainedResumeBinding{Key: finished.Key, Allocation: finished.AllocID, Node: finished.NodeID, Namespace: finished.Namespace, Task: finished.TaskName, Session: finished.WorkerSessionID, CreatorAgentID: finished.CreatorAgentID, CreatorSessionID: finished.CreatorSessionID, Generation: generation(&finished), ReceiptSHA256: lima.Hash(receiptJSON)}
	result.Phase = "both_stopped_original_resume_receipt_ready"
	completion, _ := json.Marshal(result)
	if err = durablePurgeAudit(d.store.Root+"/"+lima.Hash(raw)+".pair-recovery.completed.audit", completion); err != nil {
		return result, err
	}
	return result, nil
}
