package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"nomad-driver-lima/internal/lima"
	"nomad-driver-lima/internal/repositories"
	guestassets "nomad-driver-lima/plugin/guest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type repositoryCLI struct {
	*fakeCLI
	stages    []string
	failApply bool
	applied   int
	execArgs  []string
}

func (f *repositoryCLI) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	if len(in) > 0 {
		var p map[string]any
		_ = json.Unmarshal(in, &p)
		if _, ok := p["profile"]; ok {
			f.stages = append(f.stages, "profile")
			f.applied++
			if f.failApply {
				return nil, fmt.Errorf("fixture profile failed")
			}
			id := p["expected_guest_identity"].(map[string]any)
			if id["machine_id"] != "machine" || id["boot_id"] != fmt.Sprint("boot-", f.starts) {
				return nil, fmt.Errorf("identity not checked")
			}
			raw, _ := json.Marshal(map[string]any{"enabled": true, "profile_sha256": p["profile_sha256"], "environment_sha256": strings.Repeat("e", 64), "executor_sha256": lima.Hash([]byte(guestassets.RepositoryProfile))})
			return raw, nil
		}
		if action, ok := p["action"].(string); ok && (action == "identity" || action == "launch") {
			f.stages = append(f.stages, action)
		}
	}
	if len(in) == 0 && len(args) > 0 && args[0] == "shell" {
		f.execArgs = append([]string(nil), args...)
	}
	return f.fakeCLI.Run(ctx, in, args...)
}
func configureRepositoryFixture(t *testing.T, d *Driver) {
	t.Helper()
	raw := []byte(`{"repository_profile_default":"cache","repository_profiles":{"cache":{"schema_version":1,"id":"cache","version":"v1","npm":{"registry":"https://cache.example.invalid/npm/"},"go":{"proxy":"https://cache.example.invalid/go-modules/"}}}}`)
	path := filepath.Join(t.TempDir(), "profiles.json")
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	d.config.RepositoryProfilesFile = path
	d.config.RepositoryProfilesSHA256 = repositories.Hash(raw)
}
func TestRepositoryIdentityApplyLaunchAndNativeExec(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	configureRepositoryFixture(t, d)
	r := &repositoryCLI{fakeCLI: f}
	d.client.Runner = r
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	if slices.Index(r.stages, "identity") < 0 || slices.Index(r.stages, "profile") <= slices.Index(r.stages, "identity") || slices.Index(r.stages, "launch") <= slices.Index(r.stages, "profile") {
		t.Fatal(r.stages)
	}
	if r.applied != 1 || !strings.Contains(strings.Join(f.command, " "), "repository_bound_command") {
		t.Fatal("workload not wrapped")
	}
	state, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || state.RepositoryProfile == nil || state.RepositoryProfile.EnvironmentSHA256 == "" {
		t.Fatal(e, "missing pinned state")
	}
	if _, e = d.ExecTask(cfg.ID, []string{"go", "env", "GOPROXY"}, 0); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(strings.Join(r.execArgs, " "), "repository_bound_command") {
		t.Fatal("native exec not wrapped")
	}
	raw, _ := os.ReadFile(cfg.StdoutPath)
	if strings.Contains(string(raw), "cache.example.invalid") || !strings.Contains(string(raw), "repository_profile_ready") {
		t.Fatal("unsafe or missing stage logs")
	}
	// A changed operator default cannot change this task's durable profile on lookup.
	d.config.RepositoryProfilesFile = ""
	d.config.RepositoryProfilesSHA256 = ""
	if _, _, e = d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	if r.applied != 1 {
		t.Fatal("profile reapplied to live task")
	}
}
func TestRepositoryFailureNeverLaunchesOrDeliversVault(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	configureRepositoryFixture(t, d)
	r := &repositoryCLI{fakeCLI: f, failApply: true}
	d.client.Runner = r
	cfg := fixture(t)
	if _, _, e := d.StartTask(cfg); e == nil {
		t.Fatal("profile failure accepted")
	}
	if f.launches != 0 || f.vaultSyncs != 0 {
		t.Fatal("workload/secret before profile readiness")
	}
	if f.creates != 1 {
		t.Fatal("fixture did not reach identity gate")
	}
}
func TestRepositoryBadConfigRejectsBeforeCreateAndDisabledNoCalls(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	configureRepositoryFixture(t, d)
	d.config.RepositoryProfilesSHA256 = strings.Repeat("0", 64)
	if _, _, e := d.StartTask(fixture(t)); e == nil {
		t.Fatal("wrong config digest accepted")
	}
	if f.creates != 0 {
		t.Fatal("created before profile validation")
	}
	f = newFake()
	d = setup(t, f, "")
	r := &repositoryCLI{fakeCLI: f}
	d.client.Runner = r
	if _, _, e := d.StartTask(fixture(t)); e != nil {
		t.Fatal(e)
	}
	if r.applied != 0 || strings.Contains(strings.Join(f.command, " "), "repository_bound_command") {
		t.Fatal("disabled profile changed old command")
	}
}

func TestDisabledProfilePreservesLegacyTaskSpecJSON(t *testing.T) {
	cfg := fixture(t)
	var tc TaskConfig
	if e := cfg.DecodeDriverConfig(&tc); e != nil {
		t.Fatal(e)
	}
	typ := reflect.TypeOf(tc)
	value := reflect.ValueOf(tc)
	fields := []reflect.StructField{}
	indices := []int{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Name == "RepositoryProfileID" || field.Name == "RepositoryProfileSHA256" {
			continue
		}
		if field.PkgPath != "" {
			continue
		}
		fields = append(fields, field)
		indices = append(indices, i)
	}
	legacy := reflect.New(reflect.StructOf(fields)).Elem()
	for i, j := range indices {
		legacy.Field(i).Set(value.Field(j))
	}
	oldJSON, _ := json.Marshal(legacy.Interface())
	newJSON, _ := json.Marshal(tc)
	if string(oldJSON) != string(newJSON) {
		t.Fatal("disabled profile changed legacy task spec encoding", string(oldJSON), string(newJSON))
	}
}

func TestRepositoryRecoveryKeepsSnapshotWithoutReselectingDefault(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	configureRepositoryFixture(t, d)
	r := &repositoryCLI{fakeCLI: f}
	d.client.Runner = r
	cfg := fixture(t)
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	before, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || before.Version != 2 {
		t.Fatal(e, "profile state schema not protected")
	}
	if e = d.Shutdown(context.Background()); e != nil {
		t.Fatal(e)
	}
	fresh := setup(t, f, root)
	fresh.client.Runner = r
	if e = fresh.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	after, e := fresh.store.read(before.Key)
	if e != nil {
		t.Fatal(e)
	}
	if after.RepositoryProfile.SHA256 != before.RepositoryProfile.SHA256 || r.applied != 1 || f.creates != 1 || f.launches != 1 {
		t.Fatal("recovery changed profile or replayed workload")
	}
	if _, e = fresh.ExecTask(cfg.ID, []string{"npm", "config", "get", "registry"}, 0); e != nil {
		t.Fatal(e)
	}
}

func TestRepositoryConfigDoesNotCollideWithLedgerGlob(t *testing.T) {
	for _, name := range []string{"/state/profiles.json", "/state/profile.JSON"} {
		if validateRepositoryStatePath(Config{StateDir: "/state", RepositoryProfilesFile: name}) == nil {
			t.Fatal("ledger collision accepted", name)
		}
	}
	for _, name := range []string{"/state/profiles.config", "/etc/profiles.json", ""} {
		if e := validateRepositoryStatePath(Config{StateDir: "/state", RepositoryProfilesFile: name}); e != nil {
			t.Fatal(e)
		}
	}
}
