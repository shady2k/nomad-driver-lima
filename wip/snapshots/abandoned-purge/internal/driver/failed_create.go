package driver

// Narrow operator recovery: no create/start/stop/delete and no prompt dispatch.
// A retained uncertain reservation can be released only for the specific
// pre-create cache-open failure, with fresh independent runtime/Nomad evidence.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type FailedCreateOptions struct {
	StateDir, LimaHome, LimaBinary, RuntimeUser, NomadBinary string
	Key, AllocationID, NodeID, JobID, Namespace, FailureLog  string
	Instance                                                 string
	ExpectedLedgerSHA256                                     string
	StoppedSourceReceipt, StoppedSourceReceiptSHA256         string
	AllowGC404                                               bool
	AbandonedStopped                                         bool
	QemuImg                                                  string
}
type failedAllocation struct {
	CurrentJob *purgeJobProof

	OperatorManagementVerified                                                   bool
	HTTPStatus, NodeHTTPStatus, NodeAllocationsHTTPStatus, NodeActiveAllocations int
	VerifiedNodeID                                                               string
	ID, NodeID, JobID, Namespace, ClientStatus, NextAllocation                   string
	ModifyIndex                                                                  uint64
	TaskStates                                                                   map[string]struct {
		State                  string
		Failed                 bool
		Restarts               uint64
		StartedAt, LastRestart time.Time
		Events                 []struct{ Type string }
	}
}
type FailedCreateResult struct {
	Key              string `json:"key"`
	Instance         string `json:"instance"`
	Phase            string `json:"phase"`
	AuditSHA256      string `json:"audit_sha256"`
	AlreadyRecovered bool   `json:"already_recovered"`
}
type failedCreateAudit struct {
	Version                  int
	Original                 State
	OriginalSHA256           string
	FailureLogSHA256         string
	NodeID, JobID, Namespace string
	AllocationModifyIndex    uint64
	ObservedAt               time.Time
}
type failedCreateDeps struct {
	allocation func(context.Context) (failedAllocation, error)
	absent     func(context.Context, *State) error
	now        func() time.Time
}

var recoveryKey = regexp.MustCompile(`^[a-f0-9]{64}$`)
var recoveryUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// RecoverFailedCreate uses existing operator authentication only for a read-only
// Nomad GET. It never accepts caller-supplied absence/terminal assertions.
func RecoverFailedCreate(ctx context.Context, o FailedCreateOptions) (FailedCreateResult, error) {
	return recoverFailedCreateRuntime(ctx, o, nil)
}

type maintenanceRuntime struct {
	client   *lima.Client
	identity *runtimeIdentity
	deps     failedCreateDeps
}

func recoverFailedCreateRuntime(ctx context.Context, o FailedCreateOptions, verifier func(context.Context) (failedAllocation, error)) (FailedCreateResult, error) {
	r, e := newMaintenanceRuntime(o, verifier)
	if e != nil {
		return FailedCreateResult{}, e
	}
	return recoverFailedCreate(ctx, o, r.deps)
}
func newMaintenanceRuntime(o FailedCreateOptions, verifier func(context.Context) (failedAllocation, error)) (*maintenanceRuntime, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("failed-create recovery currently supports Linux /proc proof only")
	}
	if !filepath.IsAbs(o.LimaBinary) || (verifier == nil && !filepath.IsAbs(o.NomadBinary)) {
		return nil, fmt.Errorf("absolute official CLI paths required")
	}
	identity, e := resolveRuntimeUser(o.RuntimeUser)
	if e != nil {
		return nil, e
	}
	home, he := os.Lstat(o.LimaHome)
	if he != nil || !home.IsDir() || home.Mode().Perm() != 0700 || filepath.Clean(o.LimaHome) != o.LimaHome {
		return nil, fmt.Errorf("existing private non-symlink Lima home required")
	}
	if e = identity.validateHome(o.LimaHome); e != nil {
		return nil, e
	}
	client := &lima.Client{Home: o.LimaHome, Runner: lima.CommandRunner{Binary: o.LimaBinary, Home: o.LimaHome, UserHome: identity.home, Credential: identity.credential}}
	deps := failedCreateDeps{now: time.Now}
	deps.allocation = func(ctx context.Context) (failedAllocation, error) {
		// Explicit GET: noninteractive Nomad CLI otherwise selects POST.
		cmd := exec.CommandContext(ctx, o.NomadBinary, "operator", "api", "-namespace="+o.Namespace, "-X", "GET", "/v1/allocation/"+o.AllocationID)
		var out cappedRecoveryBuffer
		out.limit = 2 << 20
		cmd.Stdout = &out
		cmd.Stderr = io.Discard
		if e := cmd.Run(); e != nil {
			return failedAllocation{}, fmt.Errorf("Nomad allocation read failed; no recovery")
		}
		var a failedAllocation
		dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
		if e := dec.Decode(&a); e != nil {
			return a, fmt.Errorf("Nomad allocation response invalid; no recovery")
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return a, fmt.Errorf("Nomad response has trailing data")
		}
		return a, nil
	}
	if verifier != nil {
		deps.allocation = verifier
	}
	deps.absent = func(ctx context.Context, s *State) error {
		if _, e := client.Get(ctx, s.Instance); e != lima.ErrNotFound {
			return fmt.Errorf("instance absence not proven; no recovery")
		}
		p := filepath.Join(o.LimaHome, s.Instance)
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			return fmt.Errorf("instance directory/partial disk absence not proven; no recovery")
		}
		return runtimeProcessAbsent(ctx, identity.uid, s.Instance, p)
	}
	return &maintenanceRuntime{client: client, identity: identity, deps: deps}, nil
}

type cappedRecoveryBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedRecoveryBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("recovery response too large")
	}
	return b.Buffer.Write(p)
}
func privateRecoveryRead(path string, limit int64) ([]byte, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || int(raw.Uid) != os.Geteuid() || st.Mode().Perm() != 0600 || st.Size() > limit {
		return nil, fmt.Errorf("private owned bounded recovery evidence required")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}
func validateFailedAllocation(a failedAllocation, o FailedCreateOptions, s *State) error {
	t, ok := a.TaskStates[s.TaskName]
	if a.ID != s.AllocID || a.ID != o.AllocationID || a.NodeID != o.NodeID || a.JobID != o.JobID || a.Namespace != o.Namespace || a.ClientStatus != "failed" || a.NextAllocation != "" || a.ModifyIndex == 0 || !ok || t.State != "dead" || !t.Failed || t.Restarts != 0 || !t.StartedAt.IsZero() || !t.LastRestart.IsZero() {
		return fmt.Errorf("allocation is not the exact terminal never-started attempt")
	}
	for _, v := range t.Events {
		if v.Type == "Started" || v.Type == "Restarting" || v.Type == "Restart Signaled" {
			return fmt.Errorf("allocation has runtime/restart evidence")
		}
	}
	return nil
}
func recoverFailedCreate(ctx context.Context, o FailedCreateOptions, d failedCreateDeps) (FailedCreateResult, error) {
	var result FailedCreateResult
	if !recoveryKey.MatchString(o.Key) || !recoveryUUID.MatchString(o.AllocationID) || !recoveryUUID.MatchString(o.NodeID) || o.JobID == "" || o.Namespace == "" || !filepath.IsAbs(o.StateDir) || !filepath.IsAbs(o.LimaHome) {
		return result, fmt.Errorf("explicit exact recovery target required")
	}
	// Existing store only: never create a different owner/store during recovery.
	if _, e := privateRecoveryRead(filepath.Join(o.StateDir, "owner"), 32); e != nil {
		return result, fmt.Errorf("existing private store owner required")
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
	auditPath := filepath.Join(store.Root, o.Key+".failed-create.audit")
	if s.Phase == "no_vm_failed" && s.FailedCreateAuditSHA256 != "" {
		b, e := privateRecoveryRead(auditPath, 1<<20)
		if e != nil || lima.Hash(b) != s.FailedCreateAuditSHA256 {
			return result, fmt.Errorf("recovery audit mismatch")
		}
		var a failedCreateAudit
		if json.Unmarshal(b, &a) != nil || a.Original.Key != s.Key || a.Original.Owner != s.Owner || a.Original.Instance != s.Instance || a.Original.AllocID != o.AllocationID || s.AllocID != o.AllocationID || a.NodeID != o.NodeID || a.JobID != o.JobID || a.Namespace != o.Namespace {
			return result, fmt.Errorf("recovery target/audit mismatch")
		}
		result.AuditSHA256 = s.FailedCreateAuditSHA256
		result.AlreadyRecovered = true
		return result, nil
	}
	if s.Phase != "creating" || s.AllocID != o.AllocationID || s.MachineID != "" || s.BootID != "" || s.ConfigHash != "" || !s.StartedAt.IsZero() || !s.CompletedAt.IsZero() || s.Exit != nil || s.Destroyed || s.VaultFinalizing || s.FailedCreateAuditSHA256 != "" {
		return result, fmt.Errorf("ledger is not an untouched failed-creating intent")
	}
	// Validated instance naming, CLI list and absence proof happen before any write.
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,55}$`).MatchString(s.Instance) {
		return result, fmt.Errorf("invalid ledger instance")
	}
	diagnosticDir := filepath.Join(o.StateDir, "diagnostics")
	if filepath.Dir(o.FailureLog) != diagnosticDir || !regexp.MustCompile(`^create-[0-9]+\.stderr$`).MatchString(filepath.Base(o.FailureLog)) {
		return result, fmt.Errorf("exact existing create diagnostic required")
	}
	ds, e := os.Lstat(diagnosticDir)
	if e != nil || !ds.IsDir() || ds.Mode().Perm() != 0700 || int(ds.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return result, fmt.Errorf("private diagnostic directory required")
	}
	log, e := privateRecoveryRead(o.FailureLog, 1<<20)
	if e != nil {
		return result, e
	}
	denied := regexp.MustCompile(`open ` + regexp.QuoteMeta(filepath.Join(o.LimaHome, "_catalog-cache")) + `/[a-f0-9]{64}\.yaml: permission denied`)
	if !denied.Match(log) {
		return result, fmt.Errorf("specific pre-create cache-open failure not proven")
	}
	start := d.now()
	a, e := d.allocation(ctx)
	if e != nil {
		return result, e
	}
	if e = validateFailedAllocation(a, o, s); e != nil {
		return result, e
	}
	if e = d.absent(ctx, s); e != nil {
		return result, e
	}
	// Re-read both authoritative status and runtime immediately before write.
	a2, e := d.allocation(ctx)
	if e != nil {
		return result, e
	}
	if e = validateFailedAllocation(a2, o, s); e != nil {
		return result, e
	}
	if a2.ModifyIndex != a.ModifyIndex {
		return result, fmt.Errorf("allocation changed during recovery")
	}
	if e = d.absent(ctx, s); e != nil {
		return result, e
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	now := d.now()
	if now.Before(start) || now.Sub(start) > 30*time.Second {
		return result, fmt.Errorf("recovery proof expired")
	}
	original, e := json.Marshal(s)
	if e != nil {
		return result, e
	}
	audit := failedCreateAudit{1, *s, lima.Hash(original), lima.Hash(log), o.NodeID, o.JobID, o.Namespace, a.ModifyIndex, now}
	b, e := json.Marshal(audit)
	if e != nil {
		return result, e
	}
	if old, e := privateRecoveryRead(auditPath, 1<<20); e == nil {
		var prior failedCreateAudit
		if json.Unmarshal(old, &prior) != nil || prior.OriginalSHA256 != audit.OriginalSHA256 || prior.FailureLogSHA256 != audit.FailureLogSHA256 || prior.NodeID != o.NodeID || prior.JobID != o.JobID || prior.Namespace != o.Namespace {
			return result, fmt.Errorf("existing audit conflicts; retained")
		}
		b = old // Crash after audit and before ledger: preserve the first intent audit.
	} else if !os.IsNotExist(e) {
		return result, e
	} else {
		f, e := os.OpenFile(auditPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return result, e
		}
		_, e = f.Write(b)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return result, e
		}
		if ce != nil {
			return result, ce
		}
		dir, e := os.Open(store.Root)
		if e != nil {
			return result, e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return result, e
		}
	}
	s.Phase = "no_vm_failed"
	s.FailedCreateAuditSHA256 = lima.Hash(b)
	if e = store.write(s); e != nil {
		return result, e
	}
	result.Phase = s.Phase
	result.AuditSHA256 = s.FailedCreateAuditSHA256
	return result, nil
}

// Read cmdline only, never process environment; retain no argv in audit/output.
// Every live process of the runtime UID must be inspectable; ambiguity fails closed.
func runtimeProcessAbsent(ctx context.Context, uid int, instance, path string) error {
	entries, e := os.ReadDir("/proc")
	if e != nil {
		return fmt.Errorf("process proof unavailable")
	}
	for _, v := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, e = strconv.Atoi(v.Name()); e != nil {
			continue
		}
		base := filepath.Join("/proc", v.Name())
		st, e := os.Stat(base)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return fmt.Errorf("process ownership proof unavailable")
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("process ownership unavailable")
		}
		if int(raw.Uid) != uid {
			continue
		}
		b, e := os.ReadFile(filepath.Join(base, "cmdline"))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return fmt.Errorf("runtime process inspection unavailable")
		}
		for _, arg := range strings.Split(string(b), "\x00") {
			if strings.Contains(arg, instance) || strings.Contains(arg, path) {
				return fmt.Errorf("matching runtime process exists; reservation retained")
			}
		}
	}
	return nil
}
