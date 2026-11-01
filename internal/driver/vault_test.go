package driver

import (
	"context"
	"encoding/json"
	"github.com/hashicorp/nomad/plugins/drivers"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type vaultRecorder struct {
	payloads []map[string]string
	args     [][]string
	fail     bool
}

func (r *vaultRecorder) Run(_ context.Context, in []byte, args ...string) ([]byte, error) {
	var raw map[string]any
	if json.Unmarshal(in, &raw) == nil && raw["action"] == "identity" {
		return []byte(`{"machine_id":"machine","boot_id":"boot"}`), nil
	}
	var p map[string]string
	if e := json.Unmarshal(in, &p); e != nil {
		return nil, e
	}
	r.payloads = append(r.payloads, p)
	r.args = append(r.args, args)
	if r.fail {
		return []byte(`{"ok":false}`), nil
	}
	return []byte(`{"ok":true}`), nil
}
func tokenFixture(t *testing.T) (*drivers.TaskConfig, string) {
	t.Helper()
	cfg := fixture(t)
	cfg.AllocDir = t.TempDir()
	var tc TaskConfig
	cfg.DecodeDriverConfig(&tc)
	tc.VaultRequired = true
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000003"
	cfg.EncodeConcreteDriverConfig(tc)
	dir := cfg.TaskDir().SecretsDir
	os.MkdirAll(dir, 0700)
	return cfg, filepath.Join(dir, "vault_token")
}
func TestTaskTokenMissingSymlinkSizeAndWhitespace(t *testing.T) {
	cfg, p := tokenFixture(t)
	if _, e := readVaultToken(cfg); e == nil {
		t.Fatal("missing accepted")
	}
	os.WriteFile(p, []byte("synthetic-only\n"), 0600)
	v, e := readVaultToken(cfg)
	if e != nil || v != "synthetic-only" {
		t.Fatal("valid token rejected")
	}
	os.WriteFile(p, []byte(strings.Repeat("x", 16385)), 0600)
	if _, e = readVaultToken(cfg); e == nil {
		t.Fatal("oversize accepted")
	}
	os.WriteFile(p, []byte("two words"), 0600)
	if _, e = readVaultToken(cfg); e == nil {
		t.Fatal("whitespace accepted")
	}
	os.Remove(p)
	os.Symlink("/does-not-exist", p)
	if _, e = readVaultToken(cfg); e == nil {
		t.Fatal("symlink accepted")
	}
}
func TestTaskTokenReplacementRecoveryAndClearUseStdinOnly(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, p := tokenFixture(t)
	rec := &vaultRecorder{}
	d.client.Runner = rec
	s := &State{Instance: "vault-aaaaaaaaaaaa", MachineID: "machine", BootID: "boot", WorkerSessionID: "00000000-0000-4000-8000-000000000003", Owner: strings.Repeat("a", 32)}
	os.WriteFile(p, []byte("synthetic-one"), 0600)
	h, e := d.syncVault(t.Context(), cfg, s, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.syncVault(t.Context(), cfg, s, h); e != nil || len(rec.payloads) != 1 {
		t.Fatal("unchanged retransmitted")
	}
	os.WriteFile(p, []byte("synthetic-two"), 0600)
	h2, e := d.syncVault(t.Context(), cfg, s, h)
	if e != nil || h2 == h {
		t.Fatal("replacement not delivered")
	}
	if _, e = d.syncVault(t.Context(), cfg, s, ""); e != nil || len(rec.payloads) != 3 {
		t.Fatal("recovery did not refresh")
	}
	if e = d.clearVault(t.Context(), cfg, s); e != nil || rec.payloads[3]["action"] != "clear" || rec.payloads[3]["token"] != "" {
		t.Fatal("clear failed")
	}
	for _, args := range rec.args {
		if strings.Contains(strings.Join(args, " "), "synthetic-") {
			t.Fatal("token leaked into argv")
		}
	}
	rec.fail = true
	if _, e = d.syncVault(t.Context(), cfg, s, ""); e == nil || strings.Contains(e.Error(), "synthetic") {
		t.Fatal("delivery failure not sanitized")
	}
}
func TestVaultAdmissionRejectsGeneralEnvironmentBeforeCreate(t *testing.T) {
	d := setup(t, newFake(), "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, _ := tokenFixture(t)
	cfg.Env = map[string]string{"LLM_API_KEY": "synthetic"}
	if _, _, e := d.validate(cfg); e == nil {
		t.Fatal("general secret env accepted")
	}
}

func TestVaultTaskRecoverDoesNotReplayAndStopClears(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, p := tokenFixture(t)
	os.WriteFile(p, []byte("synthetic-one"), 0600)
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.Shutdown(t.Context()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	before := f.vaultSyncs
	f.mu.Unlock()
	os.WriteFile(p, []byte("synthetic-two"), 0600)
	d2 := setup(t, f, root)
	d2.config.VaultAddress = "https://secrets.example.invalid"
	if e = d2.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		synced := f.vaultSyncs > before
		f.mu.Unlock()
		if synced || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	syncs, creates, launches := f.vaultSyncs, f.creates, f.launches
	f.mu.Unlock()
	if syncs <= before || creates != 1 || launches != 1 {
		t.Fatal("recover missed token refresh or replayed workload")
	}
	if e = d2.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	clears := f.vaultClears
	f.mu.Unlock()
	if clears != 1 {
		t.Fatal("stop did not clear task token")
	}
}

func TestUncertainSelfRevokeRetainsVMThenSafeRetry(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, p := tokenFixture(t)
	os.WriteFile(p, []byte("synthetic-one"), 0600)
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.fault = "revoke-uncertain"
	f.mu.Unlock()
	if e := d.StopTask(cfg.ID, time.Second, ""); e == nil {
		t.Fatal("unknown revoke accepted")
	}
	f.mu.Lock()
	stops, clears := f.stops, f.vaultClears
	f.fault = ""
	f.mu.Unlock()
	if stops != 0 || clears != 0 {
		t.Fatal("unknown revoke discarded tokens or stopped VM")
	}
	if e := d.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stops != 1 || f.vaultClears != 1 {
		t.Fatal("safe cleanup retry failed")
	}
}

func TestFinalizationRecoveryNeverDeliversFreshToken(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, p := tokenFixture(t)
	os.WriteFile(p, []byte("synthetic-one"), 0600)
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.fault = "revoke-uncertain"
	f.mu.Unlock()
	if e = d.StopTask(cfg.ID, time.Second, ""); e == nil {
		t.Fatal("unknown revoke accepted")
	}
	if e = d.Shutdown(t.Context()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	before := f.vaultSyncs
	f.mu.Unlock()
	os.WriteFile(p, []byte("synthetic-replacement-host-only"), 0600)
	d2 := setup(t, f, root)
	d2.config.VaultAddress = "https://secrets.example.invalid"
	if e = d2.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	after := f.vaultSyncs
	f.fault = ""
	f.mu.Unlock()
	if before != after {
		t.Fatal("finalization recovery exposed a fresh token")
	}
	if e = d2.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
}

func TestRecoveryKeepsOwnedGuestManagedWhileTaskTokenFileTemporarilyUnavailable(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, path := tokenFixture(t)
	os.WriteFile(path, []byte("synthetic-recovery-token"), 0600)
	h, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cold := setup(t, f, root)
	cold.config.VaultAddress = d.config.VaultAddress
	if err = cold.RecoverTask(h); err != nil {
		t.Fatalf("external token availability caused destructive recovery failure: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	creates, launches, stops := f.creates, f.launches, f.stops
	f.mu.Unlock()
	if creates != 1 || launches != 1 || stops != 0 {
		t.Fatal("owned guest was replayed or stopped")
	}
	if _, err = cold.get(cfg.ID); err != nil {
		t.Fatal("handle not retained for monitor retry")
	}
}
