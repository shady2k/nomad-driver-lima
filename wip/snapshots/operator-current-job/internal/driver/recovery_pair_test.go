package driver

import (
	"context"
	"encoding/json"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"testing"
)

type pairRunner struct {
	base       *fakeCLI
	identities map[string]*State
	stopped    map[string]bool
	revokes    int
	fault      string
}

func (r *pairRunner) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	var p map[string]any
	if len(args) > 2 && args[0] == "shell" && json.Unmarshal(in, &p) == nil {
		name := args[2]
		if p["action"] == "identity" {
			s := r.identities[name]
			return json.Marshal(map[string]string{"machine_id": s.MachineID, "boot_id": s.BootID})
		}
		if p["action"] == "stop-runtime" {
			if r.fault == "runtime-stop" {
				return []byte(`{"ok":false,"code":"OWNED_RUNTIME_STOP_UNCONFIRMED"}`), nil
			}
			r.stopped[name] = true
			return []byte(`{"ok":true}`), nil
		}
		if p["action"] == "revoke" {
			r.revokes++
			if len(r.stopped) != 2 {
				panic("revoke before both runtime stops")
			}
			if r.fault == "revoke" {
				return []byte(`{"ok":false,"code":"TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED"}`), nil
			}
		}
	}
	return r.base.Run(ctx, in, args...)
}
func pairFixture(t *testing.T) (*Driver, RecoveryPairPlan, *pairRunner) {
	t.Helper()
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	d.config.VaultAddress = "https://secrets.example.invalid"
	cfg, tokenPath := tokenFixture(t)
	var tc TaskConfig
	cfg.DecodeDriverConfig(&tc)
	tc.CreatorAgentID = "00000000-0000-4000-8000-000000000008"
	tc.CreatorSessionID = "native-owner-fixture"
	cfg.EncodeConcreteDriverConfig(tc)
	cfg.NodeID = "node-fixture"
	cfg.Namespace = "workers-fixture"
	os.WriteFile(tokenPath, []byte("synthetic-shared-token"), 0600)
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	if err := d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	original, err := d.store.read(lima.Hash([]byte(cfg.ID)))
	if err != nil {
		t.Fatal(err)
	}
	original.VaultTokenSHA256 = lima.Hash([]byte("synthetic-shared-token"))
	if err = d.store.write(original); err != nil {
		t.Fatal(err)
	}
	duplicate := *original
	duplicate.TaskID = cfg.AllocID + "/work/restored-invocation"
	duplicate.Key = lima.Hash([]byte(duplicate.TaskID))
	duplicate.Instance = instanceName(TaskConfig{}, duplicate.Key)
	duplicate.MachineID = "different-machine"
	duplicate.BootID = "different-boot"
	dir := filepath.Join(d.config.LimaHome, duplicate.Instance)
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte("fixture config"), 0600)
	os.WriteFile(filepath.Join(dir, "disk"), []byte("empty duplicate fixture disk"), 0600)
	info := f.infos[original.Instance]
	info.Name = duplicate.Instance
	info.Dir = dir
	info.Param = map[string]string{"nomad_driver_id": duplicate.Owner, "nomad_task_key": duplicate.Key}
	f.infos[duplicate.Instance] = info
	if err = d.store.write(&duplicate); err != nil {
		t.Fatal(err)
	}
	if err = d.store.recordVaultExposure(&duplicate, duplicate.VaultTokenSHA256); err != nil {
		t.Fatal(err)
	}
	d2 := setup(t, f, root)
	d2.config.VaultAddress = d.config.VaultAddress
	runner := &pairRunner{base: f, identities: map[string]*State{original.Instance: original, duplicate.Instance: &duplicate}, stopped: map[string]bool{}}
	d2.client.Runner = runner
	target := func(s *State) RecoveryPairTarget {
		raw, _ := json.Marshal(s)
		return RecoveryPairTarget{TaskID: s.TaskID, Key: s.Key, Instance: s.Instance, MachineID: s.MachineID, BootID: s.BootID, LedgerSHA256: lima.Hash(raw)}
	}
	return d2, RecoveryPairPlan{Version: 1, Original: target(original), Duplicate: target(&duplicate)}, runner
}
func TestRecoveryPairPreflightHasNoEffects(t *testing.T) {
	d, plan, r := pairFixture(t)
	result, err := d.recoverPair(plan, false)
	if err != nil || !result.SharedToken || result.Phase != "preflight" {
		t.Fatal(result, err)
	}
	if r.revokes != 0 || len(r.stopped) != 0 || r.base.stops != 0 {
		t.Fatal("preflight effects")
	}
}
func TestRecoveryPairQuiescesBothBeforeRevokeAndRetainsDisks(t *testing.T) {
	d, plan, r := pairFixture(t)
	result, err := d.recoverPair(plan, true)
	if err != nil || !result.OriginalRetainedReceipt || !result.DuplicateRetained {
		t.Fatal(result, err)
	}
	source, _ := d.store.read(plan.Original.Key)
	duplicate, _ := d.store.read(plan.Duplicate.Key)
	if source.StopReceipt == nil || !source.StopReceipt.RevocationConfirmed || duplicate.StopReceipt != nil || duplicate.Exit == nil || duplicate.Phase != "stopped" {
		t.Fatal("incorrect retained receipts")
	}
	if r.revokes != 1 || r.base.stops != 2 || r.base.deletes != 0 {
		t.Fatalf("effects revoke/stop/delete %d/%d/%d", r.revokes, r.base.stops, r.base.deletes)
	}
	for _, s := range []*State{source, duplicate} {
		if _, err := os.Stat(filepath.Join(d.config.LimaHome, s.Instance, "disk")); err != nil {
			t.Fatal("disk lost")
		}
	}
	// A repeat is not an instruction to replay a partially known lifecycle.
	if _, err = d.recoverPair(plan, true); err == nil {
		t.Fatal("old plan reused after effects")
	}
}
func TestRecoveryPairFailuresRetainAndNeverRevokeBeforeQuiescence(t *testing.T) {
	for _, fault := range []string{"runtime-stop", "revoke", "changed-plan", "client-live"} {
		t.Run(fault, func(t *testing.T) {
			d, plan, r := pairFixture(t)
			r.fault = fault
			if fault == "changed-plan" {
				plan.Duplicate.MachineID = "wrong-machine"
			}
			if fault == "client-live" {
				lock, err := d.store.lock(plan.Duplicate.Key)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock(lock)
			}
			if _, err := d.recoverPair(plan, true); err == nil {
				t.Fatal("failure accepted")
			}
			if fault != "revoke" && r.revokes != 0 {
				t.Fatal("unexpected revoke")
			}
			if r.base.stops != 0 || r.base.deletes != 0 {
				t.Fatal("unconfirmed lifecycle stopped/deleted VM")
			}
		})
	}
}

func TestRecoveryPairCannotRevokeTokenExposedOutsideApprovedPair(t *testing.T) {
	d, plan, r := pairFixture(t)
	original, err := d.store.read(plan.Original.Key)
	if err != nil {
		t.Fatal(err)
	}
	other := *original
	other.TaskID = "another-allocation/work/attempt"
	other.Key = lima.Hash([]byte(other.TaskID))
	other.Instance = "unrelated-runtime"
	if err = d.store.write(&other); err != nil {
		t.Fatal(err)
	}
	if err = d.store.recordVaultExposure(&other, original.VaultTokenSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err = d.recoverPair(plan, true); err == nil {
		t.Fatal("unrelated shared token revoked")
	}
	if r.revokes != 0 || len(r.stopped) != 0 || r.base.stops != 0 {
		t.Fatal("effect outside safe pair scope")
	}
}
