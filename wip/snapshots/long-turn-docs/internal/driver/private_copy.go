package driver

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
	"net/url"
	"nomad-driver-lima/internal/catalog"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// PrivateCopyInput is an operator-approved recovery input, NOT a catalog template.
// No sealed/two-boot acceptance is asserted and no legacy receipt is synthesized.
type PrivateCopyInput struct {
	ID                string `json:"id"`
	NodeID            string `json:"node_id"`
	Namespace         string `json:"namespace"`
	CreatorAgentID    string `json:"creator_agent_id"`
	CreatorSessionID  string `json:"creator_session_id"`
	CreatorGeneration string `json:"creator_generation"`
	SourceKey         string `json:"source_key"`
	SourceAllocation  string `json:"source_allocation"`
	SourceSession     string `json:"source_session"`
	SourceInstance    string `json:"source_instance"`
	CopyPath          string `json:"copy_path"`
	CopySHA256        string `json:"copy_sha256"`
	CopySizeBytes     int64  `json:"copy_size_bytes"`
	VirtualSizeBytes  int64  `json:"virtual_size_bytes"`
	Architecture      string `json:"architecture"`
	Workspace         string `json:"workspace"`
	GuestStateDir     string `json:"guest_state_dir"`
	Approved          bool   `json:"operator_approved"`
}
type PrivateCopyManifest struct {
	Version            int                         `json:"schema_version"`
	MaxReservedDiskGiB int                         `json:"max_reserved_disk_gib"`
	Inputs             map[string]PrivateCopyInput `json:"inputs"`
}
type PrivateCopyAdmission struct {
	MaxReservedDiskGiB              int
	Input                           PrivateCopyInput
	ManifestSHA256                  string
	Phase                           string
	InitialMachineID, InitialBootID string
	CleanupReceiptSHA256            string
}

func privateDigest(path string) (string, int64, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return "", 0, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("private copy regular file required")
	}
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil {
		return "", 0, e
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func loadPrivateCopies(path, pin string) (*PrivateCopyManifest, error) {
	if path == "" && pin == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) || !recoveryKey.MatchString(pin) {
		return nil, fmt.Errorf("pinned operator private copy manifest required")
	}
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("private owned copy manifest required")
	}
	if st.Size() > 1<<20 {
		return nil, fmt.Errorf("copy manifest exceeds bound")
	}
	raw, e := os.ReadFile(path)
	if e != nil || lima.Hash(raw) != pin {
		return nil, fmt.Errorf("copy manifest pin changed")
	}
	var m PrivateCopyManifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&m); e != nil {
		return nil, fmt.Errorf("copy manifest schema invalid")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("copy manifest trailing data")
	}
	if m.Version != 1 || m.MaxReservedDiskGiB < 1 || len(m.Inputs) < 1 || len(m.Inputs) > 8 {
		return nil, fmt.Errorf("bounded private copy manifest required")
	}
	seenSource := map[string]bool{}
	seenCopy := map[string]bool{}
	for id, v := range m.Inputs {
		if seenSource[v.SourceKey] || seenCopy[v.CopyPath] {
			return nil, fmt.Errorf("private copy source must be unique")
		}
		seenSource[v.SourceKey] = true
		seenCopy[v.CopyPath] = true
		if !slugPattern.MatchString(id) || v.ID != id || !v.Approved || !uuidPattern.MatchString(v.NodeID) || v.Namespace == "" || !uuidPattern.MatchString(v.CreatorAgentID) || !uuidPattern.MatchString(v.CreatorGeneration) || !uuidPattern.MatchString(v.SourceAllocation) || !uuidPattern.MatchString(v.SourceSession) || !recoveryKey.MatchString(v.SourceKey) || !recoveryKey.MatchString(v.CopySHA256) || !filepath.IsAbs(v.CopyPath) || v.CopySizeBytes < 1 || v.VirtualSizeBytes < 1 || v.VirtualSizeBytes%(1<<30) != 0 || (v.Architecture != "amd64" && v.Architecture != "arm64") || v.Workspace != "/workspace/repo" || v.GuestStateDir != "/var/tmp/nomad-workers" {
			return nil, fmt.Errorf("private copy input identity invalid")
		}
		if len(v.SourceInstance) > 63 || !regexpInstance(v.SourceInstance) || v.CreatorSessionID == "" || strings.ContainsAny(v.CreatorSessionID, "\x00\r\n") {
			return nil, fmt.Errorf("private copy owner/source invalid")
		}
	}
	return &m, nil
}
func regexpInstance(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return len(s) > 0
}
func (d *Driver) privateCopyInput(cfg *drivers.TaskConfig, tc TaskConfig) (*PrivateCopyAdmission, int, error) {
	var snapshot *PrivateCopyAdmission
	var m *PrivateCopyManifest
	if d.store != nil {
		if old, err := d.store.read(lima.Hash([]byte(cfg.ID))); err == nil && old.PrivateCopy != nil {
			copy := *old.PrivateCopy
			snapshot = &copy
			if old.TaskID != cfg.ID || old.AllocID != cfg.AllocID {
				return nil, 0, fmt.Errorf("private copy durable task binding changed")
			}
		}
	}
	var e error
	if snapshot == nil {
		m, e = loadPrivateCopies(d.config.PrivateCopiesFile, d.config.PrivateCopiesSHA256)
		if e != nil || m == nil {
			return nil, 0, fmt.Errorf("private copy admission is not configured")
		}
	} else {
		m = &PrivateCopyManifest{MaxReservedDiskGiB: snapshot.MaxReservedDiskGiB, Inputs: map[string]PrivateCopyInput{snapshot.Input.ID: snapshot.Input}}
	}

	v, ok := m.Inputs[tc.PrivateCopyID]
	if !ok || v.NodeID != cfg.NodeID || v.Namespace != cfg.Namespace || v.CreatorAgentID != tc.CreatorAgentID || v.CreatorSessionID != tc.CreatorSessionID || v.CreatorGeneration != tc.PrivateCopyOwnerGeneration || v.Architecture != tc.Architecture || int64(tc.DiskGiB)*(1<<30) != v.VirtualSizeBytes || !tc.VaultRequired || tc.WorkerSessionID == v.SourceSession || cfg.AllocID == v.SourceAllocation || tc.ResumeFromKey != "" || tc.TemplateID != "" || tc.TemplateVariantsJSON != "" {
		return nil, 0, fmt.Errorf("private copy owner/node/resources binding mismatch")
	}
	if e = validateCreator(tc); e != nil {
		return nil, 0, e
	}
	if snapshot != nil {
		return snapshot, m.MaxReservedDiskGiB, nil
	}
	return &PrivateCopyAdmission{Input: v, ManifestSHA256: d.config.PrivateCopiesSHA256, MaxReservedDiskGiB: m.MaxReservedDiskGiB}, m.MaxReservedDiskGiB, nil
}
func (d *Driver) verifyPrivateSource(ctx context.Context, a *PrivateCopyAdmission) error {
	v := a.Input
	source, e := d.store.read(v.SourceKey)
	if e != nil {
		return fmt.Errorf("exact original source ledger unavailable")
	}
	if source.ResumeNext != nil || source.Instance != v.SourceInstance || source.AllocID != v.SourceAllocation || source.WorkerSessionID != v.SourceSession || source.CreatorAgentID != v.CreatorAgentID || source.CreatorSessionID != v.CreatorSessionID || source.Exit == nil || (source.Phase != "stopped" && source.Phase != "destroyed_disk_retained" && source.Phase != "exited") {
		return fmt.Errorf("original source identity/state changed")
	}
	info, e := d.owned(ctx, source)
	if e != nil || info.Status != "Stopped" || info.DriverPID > 0 || info.HostAgentPID > 0 {
		return fmt.Errorf("original source not physically stopped")
	}
	for _, path := range []string{v.CopyPath, filepath.Join(d.config.LimaHome, v.SourceInstance, "disk")} {
		stat, e := os.Lstat(path)
		if e != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("source/copy file must be immutable owned regular data")
		}
		got, size, e := privateDigest(path)
		if e != nil || got != v.CopySHA256 || size != v.CopySizeBytes {
			return fmt.Errorf("original or private copy bytes changed")
		}
	}
	f, e := os.Open(v.CopyPath)
	if e != nil {
		return e
	}
	defer f.Close()
	header := make([]byte, 32)
	if _, e = io.ReadFull(f, header); e != nil || string(header[:4]) != "QFI\xfb" || binary.BigEndian.Uint64(header[8:16]) != 0 || binary.BigEndian.Uint32(header[16:20]) != 0 || int64(binary.BigEndian.Uint64(header[24:32])) != v.VirtualSizeBytes {
		return fmt.Errorf("standalone qcow2 private copy required")
	}
	return nil
}
func (d *Driver) reservePrivateCopy(cfg *drivers.TaskConfig, tc TaskConfig) (*PrivateCopyAdmission, error) {
	a, cap, e := d.privateCopyInput(cfg, tc)
	if e != nil {
		return nil, e
	}
	if d.config.MaxTotalDiskGiB < 1 || cap > d.config.MaxTotalDiskGiB {
		return nil, fmt.Errorf("private copy requires an existing bounded node disk policy")
	}
	rows, e := d.ledger()
	if e != nil {
		return nil, e
	}
	total := tc.DiskGiB
	for _, s := range rows {
		if purged, err := d.store.purged(s); err != nil {
			return nil, err
		} else if !purged && s.Phase != "no_vm_failed" {
			total += s.DiskGiB
		}
		if s.PrivateCopy != nil && (s.PrivateCopy.Input.ID == a.Input.ID || s.PrivateCopy.Input.SourceKey == a.Input.SourceKey) {
			return nil, fmt.Errorf("private copy input already claimed; inspect exact allocation, never replay")
		}
	}
	if total > cap {
		return nil, fmt.Errorf("private recovery retained-disk cap exceeded")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Minute)
	defer cancel()
	if e = d.verifyPrivateSource(ctx, a); e != nil {
		return nil, e
	}
	return a, nil
}
func (d *Driver) materializePrivateCopy(ctx context.Context, cfg *drivers.TaskConfig, s *State) (string, error) {
	if e := d.verifyPrivateSource(ctx, s.PrivateCopy); e != nil {
		return "", e
	}
	cache := catalog.Cache{Dir: filepath.Join(d.config.LimaHome, "_catalog-cache"), Runtime: &catalog.Access{UID: d.runtimeIdentity.uid, GID: d.runtimeIdentity.gid}, Progress: func(stage string, n, total int64) {
		_ = d.stage(cfg, s, "private_copy_"+stage, map[string]any{"bytes": n, "total_bytes": total})
	}}
	v := s.PrivateCopy.Input
	path, e := cache.MaterializePrivateCopy(ctx, catalog.Artifact{URL: (&url.URL{Scheme: "file", Path: v.CopyPath}).String(), SHA256: v.CopySHA256, SizeBytes: v.CopySizeBytes}, v.Architecture)
	if e != nil {
		return "", e
	}
	if e = d.verifyPrivateSource(ctx, s.PrivateCopy); e != nil {
		return "", e
	}
	return path, nil
}

func (d *Driver) reservePrivateCopyIfRequested(cfg *drivers.TaskConfig, tc TaskConfig) (*PrivateCopyAdmission, error) {
	if tc.PrivateCopyID == "" {
		return nil, nil
	}
	return d.reservePrivateCopy(cfg, tc)
}

func (d *Driver) preparePrivateCopyGuest(ctx context.Context, cfg *drivers.TaskConfig, s *State) error {
	a := s.PrivateCopy
	if a.Phase == "ready" {
		return nil
	}
	if a.Phase == "" || a.Phase == "cleanup_dispatched" {
		action := "probe"
		if a.Phase == "" {
			a.Phase = "cleanup_dispatched"
			if e := d.store.write(s); e != nil {
				return e
			}
			action = "clean"
		}
		_ = d.stage(cfg, s, "private_copy_runtime_cleanup", nil)
		proof, e := d.client.PreparePrivateCopy(ctx, s.Instance, map[string]any{"action": action, "session_id": s.WorkerSessionID, "source_session": a.Input.SourceSession, "copy_sha256": a.Input.CopySHA256, "manifest_sha256": a.ManifestSHA256, "source_key": a.Input.SourceKey})
		if e != nil {
			return e
		}
		binding, ok := proof["binding"].(map[string]any)
		if !ok || binding["session_id"] != s.WorkerSessionID || binding["source_session"] != a.Input.SourceSession || binding["copy_sha256"] != a.Input.CopySHA256 || binding["manifest_sha256"] != a.ManifestSHA256 || binding["source_key"] != a.Input.SourceKey || proof["known_runtime_credentials_removed"] != true || proof["reserved_runtime_root_removed"] != true || proof["workspace_preserved"] != true || proof["identity_reset_pending"] != true {
			return fmt.Errorf("private copy cleanup proof binding mismatch")
		}
		a.InitialMachineID, _ = proof["machine_id"].(string)
		a.InitialBootID, _ = proof["boot_id"].(string)
		if a.InitialMachineID == "" || a.InitialBootID == "" {
			return fmt.Errorf("private copy initial identity missing")
		}
		raw, _ := json.Marshal(proof)
		a.CleanupReceiptSHA256 = lima.Hash(raw)
		a.Phase = "cleanup_confirmed"
		if e = d.store.write(s); e != nil {
			return e
		}
	}
	if a.Phase == "cleanup_confirmed" {
		a.Phase = "identity_stop_dispatched"
		if e := d.store.write(s); e != nil {
			return e
		}
		_ = d.stage(cfg, s, "private_copy_identity_reboot", nil)
		if e := d.client.Stop(ctx, s.Instance); e != nil {
			return fmt.Errorf("private copy identity stop unconfirmed; data retained")
		}
		v, e := d.owned(ctx, s)
		if e != nil || v.Status != "Stopped" || v.DriverPID > 0 || v.HostAgentPID > 0 {
			return fmt.Errorf("private copy stop not confirmed")
		}
		a.Phase = "identity_stopped"
		if e = d.store.write(s); e != nil {
			return e
		}
	}
	if a.Phase == "identity_stop_dispatched" {
		v, e := d.owned(ctx, s)
		if e != nil || v.Status != "Stopped" || v.DriverPID > 0 || v.HostAgentPID > 0 {
			return fmt.Errorf("private copy stop outcome unknown; no repeated stop")
		}
		a.Phase = "identity_stopped"
		if e = d.store.write(s); e != nil {
			return e
		}
	}
	if a.Phase == "identity_stopped" {
		a.Phase = "identity_start_dispatched"
		if e := d.store.write(s); e != nil {
			return e
		}
		if e := d.client.Start(ctx, s.Instance); e != nil {
			return fmt.Errorf("private copy identity start outcome unknown; no replay")
		}
	}
	if a.Phase == "identity_start_dispatched" {
		v, e := d.owned(ctx, s)
		if e != nil || v.Status != "Running" {
			return fmt.Errorf("private copy reboot outcome unknown; no replay")
		}
		_, id, e := d.client.Guest(ctx, s.Instance, d.payload(s, "identity"))
		if e != nil || id.MachineID == "" || id.BootID == "" || id.MachineID == a.InitialMachineID || id.BootID == a.InitialBootID {
			return fmt.Errorf("private copy fresh identity not confirmed; no token delivery")
		}
		a.Phase = "ready"
		if e = d.store.write(s); e != nil {
			return e
		}
		return d.stage(cfg, s, "private_copy_identity_verified", nil)
	}
	return fmt.Errorf("private copy preparation outcome unknown; explicit reconciliation required")
}
