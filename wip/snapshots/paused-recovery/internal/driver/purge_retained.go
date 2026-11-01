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
	"syscall"
	"time"
)

type purgeObservation struct {
	Absent         bool
	ConfigHash     string
	DirDev, DirIno uint64
}
type purgeDeps struct {
	allocation func(context.Context) (failedAllocation, error)
	observe    func(context.Context, *State) (purgeObservation, error)
	remove     func(context.Context, string) error
	now        func() time.Time
}
type purgeIntent struct {
	OriginalLedgerSHA256     string
	AllocationGC404          bool
	Version                  int
	Original                 State
	Instance                 string
	NodeID, JobID, Namespace string
	ConfigHash               string
	DirDev, DirIno           uint64
	AllocationIndex          uint64
	CreatedAt                time.Time
}
type purgeCompletion struct {
	Version                     int
	Key, Instance, IntentSHA256 string
	AbsentVerifiedAt            time.Time
}

func PurgeRetained(ctx context.Context, o FailedCreateOptions) (FailedCreateResult, error) {
	if o.AllowGC404 {
		return FailedCreateResult{}, fmt.Errorf("GC404 requires status-aware existing operator stdio verifier")
	}
	return purgeRetainedRuntime(ctx, o, nil)
}
func PurgeRetainedStdio(ctx context.Context, o FailedCreateOptions, input io.ReadCloser, output io.Writer) (FailedCreateResult, error) {
	if o.NomadBinary != "" {
		return FailedCreateResult{}, fmt.Errorf("stdio verifier excludes local Nomad CLI")
	}
	v := newStdioRecoveryVerifier(o, input, output)
	return purgeRetainedRuntime(ctx, o, v.read)
}
func purgeRetainedRuntime(ctx context.Context, o FailedCreateOptions, verifier func(context.Context) (failedAllocation, error)) (FailedCreateResult, error) {
	r, e := newMaintenanceRuntime(o, verifier)
	if e != nil {
		return FailedCreateResult{}, e
	}
	d := purgeDeps{allocation: r.deps.allocation, now: time.Now, remove: r.client.Delete}
	d.observe = func(ctx context.Context, s *State) (purgeObservation, error) {
		p := filepath.Join(o.LimaHome, s.Instance)
		st, e := os.Lstat(p)
		if os.IsNotExist(e) {
			if _, e = r.client.Get(ctx, s.Instance); !errors.Is(e, lima.ErrNotFound) {
				return purgeObservation{}, fmt.Errorf("instance absence ambiguous")
			}
			if e = runtimeProcessAbsent(ctx, r.identity.uid, s.Instance, p); e != nil {
				return purgeObservation{}, e
			}
			return purgeObservation{Absent: true}, nil
		}
		if e != nil || !st.IsDir() || int(st.Sys().(*syscall.Stat_t).Uid) != r.identity.uid {
			return purgeObservation{}, fmt.Errorf("owned nonsymlink instance directory required")
		}
		v, e := r.client.Get(ctx, s.Instance)
		if e != nil {
			return purgeObservation{}, fmt.Errorf("instance metadata unavailable; reservation retained")
		}
		if v.Status != "Stopped" || v.DriverPID != 0 || v.HostAgentPID != 0 || v.Protected || v.VMType != "qemu" || filepath.Clean(v.Dir) != p || v.Param["nomad_driver_id"] != s.Owner || v.Param["nomad_task_key"] != s.Key || v.CPUs != s.CPUs || v.Memory != int64(s.MemoryMiB)*1048576 || v.Disk != int64(s.DiskGiB)*1073741824 {
			return purgeObservation{}, fmt.Errorf("stopped instance ownership/resources/protection mismatch")
		}
		// Reject symlink entries rather than deleting an unreviewed disk layout.
		entries, e := os.ReadDir(p)
		if e != nil {
			return purgeObservation{}, e
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 {
				return purgeObservation{}, fmt.Errorf("instance symlink layout requires review; retained")
			}
		}
		hash, e := r.client.ConfigHash(s.Instance)
		if e != nil {
			return purgeObservation{}, e
		}
		if s.ConfigHash != "" && s.ConfigHash != hash {
			return purgeObservation{}, fmt.Errorf("instance configuration changed; retained")
		}
		if e = runtimeProcessAbsent(ctx, r.identity.uid, s.Instance, p); e != nil {
			return purgeObservation{}, e
		}
		raw := st.Sys().(*syscall.Stat_t)
		return purgeObservation{ConfigHash: hash, DirDev: uint64(raw.Dev), DirIno: raw.Ino}, nil
	}
	return purgeRetained(ctx, o, d)
}
func terminalPurgeAllocation(a failedAllocation, o FailedCreateOptions, s *State) error {
	if a.HTTPStatus == 404 {
		if !o.AllowGC404 || !a.OperatorManagementVerified || !recoveryKey.MatchString(o.ExpectedLedgerSHA256) || a.NodeHTTPStatus != 200 || a.NodeAllocationsHTTPStatus != 200 || a.VerifiedNodeID != o.NodeID || a.NodeActiveAllocations != 0 {
			return fmt.Errorf("GC404 requires exact approved ledger and fresh empty matching node proof")
		}
		return nil
	}
	if a.HTTPStatus != 0 && a.HTTPStatus != 200 {
		return fmt.Errorf("allocation authorization/HTTP status not terminal proof")
	}
	t, ok := a.TaskStates[s.TaskName]
	if a.ID != o.AllocationID || a.ID != s.AllocID || a.NodeID != o.NodeID || a.JobID != o.JobID || a.Namespace != o.Namespace || (a.ClientStatus != "failed" && a.ClientStatus != "complete") || a.NextAllocation != "" || a.ModifyIndex == 0 || !ok || t.State != "dead" {
		return fmt.Errorf("exact terminal allocation required; retained")
	}
	return nil
}
func durablePurgeAudit(path string, b []byte) error {
	if old, e := privateRecoveryRead(path, 1<<20); e == nil {
		if string(old) != string(b) {
			return fmt.Errorf("existing purge audit conflicts")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) purged(v *State) (bool, error) {
	if v.Phase != "destroyed_disk_purged" {
		return false, nil
	}
	if !v.Destroyed || !recoveryKey.MatchString(v.PurgeAuditSHA256) || !recoveryKey.MatchString(v.PurgeIntentSHA256) {
		return false, fmt.Errorf("purge completion evidence missing; retain admission")
	}
	raw, e := privateRecoveryRead(filepath.Join(s.Root, v.Key+".purge-complete.audit"), 1<<20)
	if e != nil || lima.Hash(raw) != v.PurgeAuditSHA256 {
		return false, fmt.Errorf("purge completion audit unreadable; retain admission")
	}
	var c purgeCompletion
	if json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.Key != v.Key || c.Instance != v.Instance || c.IntentSHA256 != v.PurgeIntentSHA256 || c.AbsentVerifiedAt.IsZero() {
		return false, fmt.Errorf("purge completion identity mismatch")
	}
	intent, e := privateRecoveryRead(filepath.Join(s.Root, v.Key+".purge-intent.audit"), 1<<20)
	if e != nil || lima.Hash(intent) != c.IntentSHA256 {
		return false, fmt.Errorf("purge intent audit unreadable; retain admission")
	}
	var prior purgeIntent
	if json.Unmarshal(intent, &prior) != nil || v.DiskGiB != 0 || v.PurgedDiskGiB < 1 || prior.Original.DiskGiB != v.PurgedDiskGiB {
		return false, fmt.Errorf("purged disk accounting/audit mismatch")
	}
	return true, nil
}
func purgeRetained(ctx context.Context, o FailedCreateOptions, d purgeDeps) (FailedCreateResult, error) {
	var result FailedCreateResult
	if !recoveryKey.MatchString(o.Key) || !recoveryUUID.MatchString(o.AllocationID) || !recoveryUUID.MatchString(o.NodeID) || (o.JobID == "" && !o.AllowGC404) || o.Namespace == "" || o.Instance == "" || filepath.Base(o.Instance) != o.Instance || !filepath.IsAbs(o.StateDir) {
		return result, fmt.Errorf("exact key/instance/allocation/node/job/namespace required")
	}
	if _, e := privateRecoveryRead(filepath.Join(o.StateDir, "owner"), 32); e != nil {
		return result, fmt.Errorf("existing private store required")
	}
	st, e := os.Lstat(o.StateDir)
	if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 || int(st.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return result, fmt.Errorf("existing private state directory required")
	}
	store, e := openStore(o.StateDir)
	if e != nil {
		return result, e
	}
	lock, e := store.lock(o.Key)
	if e != nil {
		return result, e
	}
	defer unlock(lock)
	admission, e := store.lock(lima.Hash([]byte("node-admission")))
	if e != nil {
		return result, e
	}
	defer unlock(admission)
	s, e := store.read(o.Key)
	if e != nil {
		return result, e
	}
	result = FailedCreateResult{Key: s.Key, Instance: s.Instance, Phase: s.Phase}
	if s.Instance != o.Instance || s.AllocID != o.AllocationID || !s.Destroyed {
		return result, fmt.Errorf("retained ledger identity mismatch")
	}
	if done, e := store.purged(s); e != nil {
		return result, e
	} else if done {
		raw, e := privateRecoveryRead(filepath.Join(store.Root, s.Key+".purge-intent.audit"), 1<<20)
		if e != nil {
			return result, e
		}
		var proof purgeIntent
		if json.Unmarshal(raw, &proof) != nil || proof.NodeID != o.NodeID || proof.JobID != o.JobID || proof.Namespace != o.Namespace || (o.ExpectedLedgerSHA256 != "" && proof.OriginalLedgerSHA256 != o.ExpectedLedgerSHA256) {
			return result, fmt.Errorf("completed purge target/approval mismatch")
		}
		result.AuditSHA256 = s.PurgeAuditSHA256
		result.AlreadyRecovered = true
		return result, nil
	}
	if s.DiskGiB < 1 {
		return result, fmt.Errorf("retained disk reservation required")
	}
	if s.Phase != "destroyed_disk_retained" && s.Phase != "purge_pending" {
		return result, fmt.Errorf("explicit destroyed retained disk required")
	}
	originalRaw, e := privateRecoveryRead(filepath.Join(store.Root, s.Key+".json"), 1<<20)
	if e != nil {
		return result, e
	}
	if o.AllowGC404 && !recoveryKey.MatchString(o.ExpectedLedgerSHA256) {
		return result, fmt.Errorf("GC404 requires approved original ledger SHA256")
	}
	began := d.now()
	a, e := d.allocation(ctx)
	if e != nil {
		return result, e
	}
	if e = terminalPurgeAllocation(a, o, s); e != nil {
		return result, e
	}
	first, e := d.observe(ctx, s)
	if e != nil {
		return result, e
	}
	intentPath := filepath.Join(store.Root, s.Key+".purge-intent.audit")
	var intent purgeIntent
	var intentRaw []byte
	if raw, er := privateRecoveryRead(intentPath, 1<<20); er == nil {
		if json.Unmarshal(raw, &intent) != nil || intent.Version != 1 || intent.Original.Key != s.Key || intent.Original.Owner != s.Owner || intent.Original.AllocID != s.AllocID || intent.Instance != s.Instance || intent.NodeID != o.NodeID || intent.JobID != o.JobID || intent.Namespace != o.Namespace {
			return result, fmt.Errorf("purge intent identity conflicts")
		}
		if o.ExpectedLedgerSHA256 != "" && intent.OriginalLedgerSHA256 != o.ExpectedLedgerSHA256 {
			return result, fmt.Errorf("approved purge ledger SHA mismatch")
		}
		intentRaw = raw
		if s.PurgeIntentSHA256 != "" && lima.Hash(raw) != s.PurgeIntentSHA256 {
			return result, fmt.Errorf("purge intent changed")
		}
	} else if !os.IsNotExist(er) {
		return result, er
	} else {
		if s.Phase != "destroyed_disk_retained" || first.Absent {
			return result, fmt.Errorf("no original owned instance/purge intent; retained")
		}
		if o.ExpectedLedgerSHA256 != "" && lima.Hash(originalRaw) != o.ExpectedLedgerSHA256 {
			return result, fmt.Errorf("current ledger differs from exact approval")
		}
		intent = purgeIntent{Version: 1, Original: *s, Instance: s.Instance, NodeID: o.NodeID, JobID: o.JobID, Namespace: o.Namespace, ConfigHash: first.ConfigHash, DirDev: first.DirDev, DirIno: first.DirIno, AllocationIndex: a.ModifyIndex, CreatedAt: d.now(), OriginalLedgerSHA256: lima.Hash(originalRaw), AllocationGC404: a.HTTPStatus == 404}
		intentRaw, e = json.Marshal(intent)
		if e != nil {
			return result, e
		}
	}
	if !first.Absent && (first.ConfigHash != intent.ConfigHash || first.DirDev != intent.DirDev || first.DirIno != intent.DirIno) {
		return result, fmt.Errorf("retained instance replaced or partially changed; no retry")
	}
	secondAlloc, e := d.allocation(ctx)
	if e != nil {
		return result, e
	}
	if e = terminalPurgeAllocation(secondAlloc, o, s); e != nil {
		return result, e
	}
	if a.HTTPStatus != secondAlloc.HTTPStatus || a.ModifyIndex != secondAlloc.ModifyIndex {
		return result, fmt.Errorf("allocation changed during purge proof")
	}
	second, e := d.observe(ctx, s)
	if e != nil {
		return result, e
	}
	if first != second {
		return result, fmt.Errorf("instance changed during purge proof")
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if d.now().Before(began) || d.now().Sub(began) > 25*time.Second {
		return result, fmt.Errorf("purge proof expired")
	}
	if e = durablePurgeAudit(intentPath, intentRaw); e != nil {
		return result, e
	}
	s.PurgeIntentSHA256 = lima.Hash(intentRaw)
	s.Phase = "purge_pending"
	if e = store.write(s); e != nil {
		return result, e
	}
	if !second.Absent {
		if e = d.remove(ctx, s.Instance); e != nil {
			return result, fmt.Errorf("delete outcome unknown; audit/reservation retained; reconcile same target")
		}
	}
	absent, e := d.observe(ctx, s)
	if e != nil || !absent.Absent {
		return result, fmt.Errorf("delete absence not confirmed; audit/reservation retained")
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	completionPath := filepath.Join(store.Root, s.Key+".purge-complete.audit")
	completion := purgeCompletion{1, s.Key, s.Instance, s.PurgeIntentSHA256, d.now()}
	raw, e := json.Marshal(completion)
	if e != nil {
		return result, e
	}
	// Reuse a previous fsynced completion if the ledger commit was interrupted.
	if old, er := privateRecoveryRead(completionPath, 1<<20); er == nil {
		var prior purgeCompletion
		if json.Unmarshal(old, &prior) != nil || prior.Key != s.Key || prior.Instance != s.Instance || prior.IntentSHA256 != s.PurgeIntentSHA256 || prior.AbsentVerifiedAt.IsZero() {
			return result, fmt.Errorf("completion audit conflicts")
		}
		raw = old
	} else if !os.IsNotExist(er) {
		return result, er
	}
	if e = durablePurgeAudit(completionPath, raw); e != nil {
		return result, e
	}
	s.Phase = "destroyed_disk_purged"
	// Existing running driver versions sum DiskGiB even for destroyed records.
	// Zero means no retained allocation; the original request stays in fsynced audit.
	s.PurgedDiskGiB = s.DiskGiB
	s.DiskGiB = 0
	s.PurgeAuditSHA256 = lima.Hash(raw)
	if e = store.write(s); e != nil {
		return result, e
	}
	result.Phase = s.Phase
	result.AuditSHA256 = s.PurgeAuditSHA256
	return result, nil
}
