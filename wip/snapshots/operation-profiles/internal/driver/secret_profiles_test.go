package driver

import (
	"context"
	"encoding/json"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretProfileConfigPinnedSafeAndDisabled(t *testing.T) {
	if raw, e := loadSecretProfiles("", ""); e != nil || raw != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "profiles.config")
	raw := []byte(`{"schema_version":1,"profiles":[{"id":"operator-fixed"}]}`)
	os.WriteFile(path, raw, 0600)
	if _, e := loadSecretProfiles(path, lima.Hash(raw)); e != nil {
		t.Fatal(e)
	}
	for _, hash := range []string{"", strings.Repeat("0", 64)} {
		if _, e := loadSecretProfiles(path, hash); e == nil {
			t.Fatal("unpinned accepted")
		}
	}
	os.Chmod(path, 0666)
	if _, e := loadSecretProfiles(path, lima.Hash(raw)); e == nil {
		t.Fatal("writable accepted")
	}
	os.Chmod(path, 0600)
	link := path + "-link"
	os.Symlink(path, link)
	if _, e := loadSecretProfiles(link, lima.Hash(raw)); e == nil {
		t.Fatal("symlink accepted")
	}
}

type secretProfileRecorder struct {
	payload map[string]any
	args    []string
}

func (r *secretProfileRecorder) Run(_ context.Context, in []byte, args ...string) ([]byte, error) {
	json.Unmarshal(in, &r.payload)
	r.args = args
	return []byte(`{"ok":true}`), nil
}
func TestSecretProfileDeliveryUsesPinnedStateAndExactTaskIdentity(t *testing.T) {
	r := &secretProfileRecorder{}
	d := &Driver{client: &lima.Client{Runner: r}}
	s := &State{SecretProfiles: json.RawMessage(`{"schema_version":1,"profiles":[{}]}`), SecretBaoBin: "/opt/bao", SecretVaultAddress: "https://secrets.example.invalid", Instance: "nomad-000000000000000000000001", WorkerSessionID: "00000000-0000-4000-8000-000000000003", CreatorAgentID: "creator", CreatorSessionID: "native", AllocID: "allocation", MachineID: "machine", BootID: "boot"}
	cfg := &drivers.TaskConfig{Name: "worker", Namespace: "workers", Env: map[string]string{"NOMAD_META_creator_generation": "00000000-0000-4000-8000-000000000004"}}
	if e := d.deliverSecretProfiles(context.Background(), cfg, s); e != nil {
		t.Fatal(e)
	}
	binding := r.payload["binding"].(map[string]any)
	if binding["creator_agent_id"] != "creator" || binding["session_id"] != s.WorkerSessionID || r.payload["namespace"] != "workers" {
		t.Fatal("binding lost")
	}
	if strings.Contains(strings.Join(r.args, " "), "models/data") {
		t.Fatal("record in argv")
	}
	cfg.Env = map[string]string{}
	if e := d.deliverSecretProfiles(context.Background(), cfg, s); e == nil {
		t.Fatal("unbound generation accepted")
	}
}
func TestSecretProfileSchemaFivePreservesPinAndRejectsDisguisedOldSchema(t *testing.T) {
	d := setup(t, newFake(), "")
	s := &State{Key: strings.Repeat("a", 64), Owner: d.store.Owner, Version: 1, SecretProfiles: json.RawMessage(`{"schema_version":1,"profiles":[{}]}`)}
	if e := d.store.write(s); e != nil {
		t.Fatal(e)
	}
	got, e := d.store.read(s.Key)
	if e != nil || got.Version != 5 {
		t.Fatalf("profile pin not schema5: %v", e)
	}
	got.Version = 4
	raw, _ := json.Marshal(got)
	os.WriteFile(filepath.Join(d.store.Root, s.Key+".json"), raw, 0600)
	if _, e = d.store.read(s.Key); e == nil {
		t.Fatal("disguised old schema accepted")
	}
}

type secretFinishRunner struct{ inner lima.Runner }

func (r *secretFinishRunner) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	var p map[string]json.RawMessage
	if json.Unmarshal(in, &p) == nil && len(p["catalogue"]) > 0 {
		return []byte(`{"ok":true}`), nil
	}
	return r.inner.Run(ctx, in, args...)
}
func TestSecretProfileFinishPurgesSchemaFive(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	profile := []byte(`{"schema_version":1,"profiles":[{}]}`)
	profilePath := filepath.Join(t.TempDir(), "profiles.config")
	os.WriteFile(profilePath, profile, 0600)
	d.config.SecretProfilesFile = profilePath
	d.config.SecretProfilesSHA256 = lima.Hash(profile)
	d.config.SecretProfilesBaoBin = "/opt/bao"
	cfg, token := tokenFixture(t)
	cfg.NodeID = "00000000-0000-4000-8000-000000000045"
	cfg.Namespace = "workers"
	var tc TaskConfig
	cfg.DecodeDriverConfig(&tc)
	tc.CreatorAgentID = "00000000-0000-4000-8000-000000000041"
	tc.CreatorSessionID = "native-source"
	cfg.EncodeConcreteDriverConfig(tc)
	gen := "00000000-0000-4000-8000-000000000042"
	cfg.Env = map[string]string{"NOMAD_META_creator_generation": gen}
	os.WriteFile(token, []byte("synthetic-task-token"), 0600)
	inner := &finishCLI{base: f}
	d.client.Runner = &secretFinishRunner{inner: inner}
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	receipt := FinishRequest{Action: "arm", Allocation: cfg.AllocID, Session: tc.WorkerSessionID, CreatorAgent: tc.CreatorAgentID, CreatorSession: tc.CreatorSessionID, CreatorGeneration: gen, Request: "00000000-0000-4000-8000-000000000043", Snapshot: "00000000-0000-4000-8000-000000000044", ManifestSHA: strings.Repeat("a", 64), ReceiptSHA: strings.Repeat("b", 64), Backend: "s3", ActivityRevision: 1}
	if _, err := d.ExecTask(cfg.ID, finishCommand(receipt), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.StopTask(cfg.ID, time.Second, ""); err != nil {
		t.Fatal(err)
	}
	state, err := d.store.read(lima.Hash([]byte(cfg.ID)))
	if err != nil || state.Version != 5 || state.Phase != "destroyed_disk_purged" || state.DiskGiB != 0 {
		t.Fatalf("schema5 finish failed: %v %#v", err, state)
	}
}
func TestSecretProfileResumeBindingCannotDropOrChangeCatalogue(t *testing.T) {
	source := &State{Version: 5, Key: strings.Repeat("a", 64), Owner: "owner", TaskID: "old-task", AllocID: "00000000-0000-4000-8000-000000000001", WorkerSessionID: "00000000-0000-4000-8000-000000000002", Instance: "nomad-000000000000000000000001", Generation: 1, DiskGiB: 8, SecretProfiles: json.RawMessage(`{"schema_version":1,"profiles":[{}]}`), SecretBaoBin: "/opt/bao", SecretVaultAddress: "https://secrets.example.invalid"}
	next := *source
	next.TaskID = "next-task"
	next.Key = lima.Hash([]byte(next.TaskID))
	next.AllocID = "00000000-0000-4000-8000-000000000003"
	next.WorkerSessionID = "00000000-0000-4000-8000-000000000004"
	next.Generation = 2
	next.ResumedFrom = source.Key
	next.InstanceKey = source.Key
	next.SpecHash = strings.Repeat("b", 64)
	next.Phase = "resume_intent"
	if err := validResumeIntent(source, &next); err != nil {
		t.Fatal(err)
	}
	next.SecretProfiles = nil
	if err := validResumeIntent(source, &next); err == nil {
		t.Fatal("profile pin dropped")
	}
	next.SecretProfiles = source.SecretProfiles
	next.SecretVaultAddress = "https://other.example.invalid"
	if err := validResumeIntent(source, &next); err == nil {
		t.Fatal("profile endpoint rebound")
	}
}

func TestSecretProfileOriginChangeRejectsBeforeGuestDelivery(t *testing.T) {
	cfg, _ := tokenFixture(t)
	d := &Driver{config: Config{VaultAddress: "https://new.example.invalid"}}
	state := &State{SecretProfiles: json.RawMessage(`{"schema_version":1,"profiles":[{}]}`), SecretVaultAddress: "https://original.example.invalid"}
	if _, err := d.syncVault(context.Background(), cfg, state, ""); err == nil {
		t.Fatal("origin drift accepted")
	}
}
