package volume

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shady2k/nomad-driver-lima/internal/identity"
)

// runCase is one invocation of the plugin under test.
//
// settings is the JSON of the node's settings file; an empty string writes an
// empty object, which is what a node with a plain installation has.
// missingSettings leaves the file out, which is the misconfiguration case.
type runCase struct {
	env             map[string]string
	argv            []string
	settings        string
	missingSettings bool
	limit           func(args []string, env []string, id *identity.Identity) error
}

// result is what a caller can observe.
type result struct {
	exit   int
	stdout string
	calls  [][]string
	envs   [][]string
	ids    []*identity.Identity
}

// run invokes the plugin with a settings file the way Nomad does: no environment
// of its own, so the plugin finds the settings path through BOX_VOLUME_CONFIG
// (the file next to the binary is the other way, see TestSettingsNextToBinary).
func run(t *testing.T, in runCase) result {
	t.Helper()
	env := map[string]string{}
	for k, v := range in.env {
		env[k] = v
	}
	if !in.missingSettings {
		settings := in.settings
		if settings == "" {
			settings = "{}"
		}
		path := filepath.Join(t.TempDir(), "volume.json")
		if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		env[settingsEnv] = path
	}
	var out bytes.Buffer
	var calls [][]string
	var envs [][]string
	var ids []*identity.Identity
	p := &Plugin{
		Getenv:     func(k string) string { return env[k] },
		Stdout:     &out,
		Executable: func() (string, error) { return "/nonexistent/box-volume", nil },
		Geteuid:    func() int { return os.Geteuid() },
		Limactl: func(args []string, env []string, id *identity.Identity) error {
			calls = append(calls, append([]string(nil), args...))
			envs = append(envs, append([]string(nil), env...))
			ids = append(ids, id)
			if in.limit != nil {
				return in.limit(args, env, id)
			}
			return nil
		},
	}
	exit := p.Run(in.argv)
	return result{exit: exit, stdout: out.String(), calls: calls, envs: envs, ids: ids}
}

// decode unmarshals stdout as a single JSON object.
func decode(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not a JSON object: %v (stdout=%q)", err, stdout)
	}
	return v
}

func TestFingerprint(t *testing.T) {
	got := run(t, runCase{argv: []string{"fingerprint"}, env: map[string]string{}})
	if got.exit != 0 {
		t.Fatalf("exit = %d, want 0", got.exit)
	}
	body := decode(t, got.stdout)
	if body["version"] != PluginVersion {
		t.Fatalf("version = %v, want %q", body["version"], PluginVersion)
	}
	if len(got.calls) != 0 {
		t.Fatalf("fingerprint called limactl: %v", got.calls)
	}
}

func TestCreate(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantBytes int64
	}{
		{
			name:      "max capacity wins",
			env:       map[string]string{envCapacityMin: "1000", envCapacityMax: "2000"},
			wantBytes: 2000,
		},
		{
			name:      "min only",
			env:       map[string]string{envCapacityMin: "1000", envCapacityMax: "0"},
			wantBytes: 1000,
		},
		{
			name:      "unset capacities",
			env:       map[string]string{},
			wantBytes: 0,
		},
		{
			name:      "negative capacities clamp to zero",
			env:       map[string]string{envCapacityMin: "-5", envCapacityMax: "-1"},
			wantBytes: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			volumesDir := t.TempDir()
			env := map[string]string{
				envVolumesDir: volumesDir,
				envVolumeID:   "vol-1234",
			}
			for k, v := range tc.env {
				env[k] = v
			}

			got := run(t, runCase{argv: []string{"create"}, env: env})
			if got.exit != 0 {
				t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
			}
			body := decode(t, got.stdout)
			wantPath := filepath.Join(volumesDir, "vol-1234")
			if body["path"] != wantPath {
				t.Fatalf("path = %v, want %q", body["path"], wantPath)
			}
			if body["bytes"] != json.Number(fmt.Sprintf("%d", tc.wantBytes)) {
				t.Fatalf("bytes = %v, want %d", body["bytes"], tc.wantBytes)
			}
			info, err := os.Stat(wantPath)
			if err != nil {
				t.Fatalf("volume dir not created: %v", err)
			}
			if !info.IsDir() {
				t.Fatalf("volume path is not a directory")
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
			}
			if len(got.calls) != 0 {
				t.Fatalf("create called limactl: %v", got.calls)
			}
		})
	}
}

func TestCreateIdempotent(t *testing.T) {
	volumesDir := t.TempDir()
	dir := filepath.Join(volumesDir, "vol-1234")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "box-a.qcow2")
	if err := os.WriteFile(marker, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		envVolumesDir:  volumesDir,
		envVolumeID:    "vol-1234",
		envCapacityMax: "4096",
	}
	for i := 0; i < 2; i++ {
		got := run(t, runCase{argv: []string{"create"}, env: env})
		if got.exit != 0 {
			t.Fatalf("run %d: exit = %d, stdout = %s", i, got.exit, got.stdout)
		}
		body := decode(t, got.stdout)
		if body["path"] != dir {
			t.Fatalf("run %d: path = %v, want %q", i, body["path"], dir)
		}
		if body["bytes"] != json.Number("4096") {
			t.Fatalf("run %d: bytes = %v, want 4096", i, body["bytes"])
		}
	}

	// The existing content must survive: create never wipes a volume.
	if data, err := os.ReadFile(marker); err != nil || string(data) != "disk" {
		t.Fatalf("existing disk file damaged: data=%q err=%v", data, err)
	}
}

func TestCreateErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "missing volumes dir", env: map[string]string{envVolumeID: "vol-1234"}},
		{name: "missing volume id", env: map[string]string{envVolumesDir: "/tmp/whatever"}},
		{name: "volume id traverses", env: map[string]string{envVolumesDir: "/tmp/x", envVolumeID: "../escape"}},
		{name: "volume id has a separator", env: map[string]string{envVolumesDir: "/tmp/x", envVolumeID: "a/b"}},
		{name: "volume id is a parent dir", env: map[string]string{envVolumesDir: "/tmp/x", envVolumeID: ".."}},
		{name: "capacity is not a number", env: map[string]string{
			envVolumesDir: "/tmp/x", envVolumeID: "vol-1234", envCapacityMax: "lots",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, runCase{argv: []string{"create"}, env: tc.env})
			if got.exit == 0 {
				t.Fatalf("exit = 0, want non-zero")
			}
			body := decode(t, got.stdout)
			if msg, _ := body["error"].(string); msg == "" {
				t.Fatalf("no error message in %s", got.stdout)
			}
		})
	}
}

func TestDeleteRemovesBoxInstancesAndDir(t *testing.T) {
	volumesDir := t.TempDir()
	dir := filepath.Join(volumesDir, "vol-1234")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"box-a.qcow2", "box-b.qcow2", "notes.txt", "box.bak", "other-c.qcow2", "box-B9.qcow2"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	env := map[string]string{
		envVolumesDir:  volumesDir,
		envVolumeID:    "vol-1234",
		envCreatedPath: dir,
	}
	settings := `{"limactl":"/opt/limactl","lima_home":"/srv/lima"}`
	got := run(t, runCase{argv: []string{"delete"}, env: env, settings: settings})
	if got.exit != 0 {
		t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
	}
	if body := decode(t, got.stdout); body["error"] != "" {
		t.Fatalf("error = %v, want empty", body["error"])
	}

	wantCalls := [][]string{
		{"/opt/limactl", "stop", "-f", "a"},
		{"/opt/limactl", "delete", "a"},
		{"/opt/limactl", "stop", "-f", "b"},
		{"/opt/limactl", "delete", "b"},
	}
	if len(got.calls) != len(wantCalls) {
		t.Fatalf("limactl calls = %v, want %v", got.calls, wantCalls)
	}
	for i, want := range wantCalls {
		if strings.Join(got.calls[i], " ") != strings.Join(want, " ") {
			t.Fatalf("call %d = %v, want %v", i, got.calls[i], want)
		}
		joined := strings.Join(got.envs[i], " ")
		if !strings.Contains(joined, "LIMA_HOME=/srv/lima") {
			t.Fatalf("call %d env = %v, want LIMA_HOME=/srv/lima", i, got.envs[i])
		}
		if !strings.Contains(joined, "PATH=") || !strings.Contains(joined, "HOME=") {
			t.Fatalf("call %d env = %v, want PATH and HOME", i, got.envs[i])
		}
	}

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("volume dir still present: err = %v", err)
	}
}

func TestDeleteFallsBackToVolumesDir(t *testing.T) {
	volumesDir := t.TempDir()
	dir := filepath.Join(volumesDir, "vol-1234")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "box-a.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run(t, runCase{argv: []string{"delete"}, env: map[string]string{
		envVolumesDir: volumesDir,
		envVolumeID:   "vol-1234",
	}})
	if got.exit != 0 {
		t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
	}
	if len(got.calls) != 2 || got.calls[0][0] != "limactl" || got.calls[0][1] != "stop" {
		t.Fatalf("limactl calls = %v", got.calls)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("volume dir still present: err = %v", err)
	}
}

func TestDeleteMissingDirSucceeds(t *testing.T) {
	volumesDir := t.TempDir()
	got := run(t, runCase{argv: []string{"delete"}, env: map[string]string{
		envVolumesDir: volumesDir,
		envVolumeID:   "vol-1234",
	}})
	if got.exit != 0 {
		t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
	}
	if len(got.calls) != 0 {
		t.Fatalf("limactl called for a missing dir: %v", got.calls)
	}
}

func TestDeleteSurvivesLimactlFailures(t *testing.T) {
	volumesDir := t.TempDir()
	dir := filepath.Join(volumesDir, "vol-1234")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "box-a.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	failing := func(args []string, env []string, id *identity.Identity) error {
		if args[1] == "delete" {
			return errors.New("instance not found")
		}
		return errors.New("no such instance")
	}
	got := run(t, runCase{argv: []string{"delete"}, env: map[string]string{
		envVolumesDir: volumesDir,
		envVolumeID:   "vol-1234",
	}, limit: failing})
	if got.exit != 0 {
		t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
	}
	if len(got.calls) != 2 {
		t.Fatalf("limactl calls = %v, want stop and delete", got.calls)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("volume dir still present: err = %v", err)
	}
}

func TestDeleteRejectsPathOutsideVolumesDir(t *testing.T) {
	volumesDir := t.TempDir()
	outside := t.TempDir()
	keeper := filepath.Join(outside, "box-a.qcow2")
	if err := os.WriteFile(keeper, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run(t, runCase{argv: []string{"delete"}, env: map[string]string{
		envVolumesDir:  volumesDir,
		envVolumeID:    "vol-1234",
		envCreatedPath: outside,
	}})
	if got.exit == 0 {
		t.Fatalf("exit = 0, want non-zero")
	}
	if len(got.calls) != 0 {
		t.Fatalf("limactl called for a rejected path: %v", got.calls)
	}
	if _, err := os.Stat(keeper); err != nil {
		t.Fatalf("file outside the volumes dir was touched: %v", err)
	}
}

func TestSettingsFile(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name         string
		settings     string
		wantLimactl  string
		wantLimaHome string
		wantErr      bool
	}{
		{
			name:         "empty file: the defaults",
			settings:     "{}",
			wantLimactl:  "limactl",
			wantLimaHome: filepath.Join(home, ".lima"),
		},
		{
			name:         "explicit values",
			settings:     `{"limactl":"/usr/bin/limactl","lima_home":"/srv/lima"}`,
			wantLimactl:  "/usr/bin/limactl",
			wantLimaHome: "/srv/lima",
		},
		{
			name:         "an empty string is not a value",
			settings:     `{"limactl":"","lima_home":""}`,
			wantLimactl:  "limactl",
			wantLimaHome: filepath.Join(home, ".lima"),
		},
		{
			name:     "an unknown key is a typo, not a setting",
			settings: `{"limactl":"limactl","lima_hom":"/srv/lima"}`,
			wantErr:  true,
		},
		{
			name:     "not json",
			settings: "limactl=/usr/bin/limactl",
			wantErr:  true,
		},
		{
			name:     "a value of the wrong type",
			settings: `{"lima_home":42}`,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "volume.json")
			if err := os.WriteFile(path, []byte(tc.settings), 0o600); err != nil {
				t.Fatal(err)
			}
			p := &Plugin{
				Getenv: func(k string) string {
					switch k {
					case "HOME":
						return home
					case settingsEnv:
						return path
					}
					return ""
				},
				Executable: func() (string, error) { return "/nonexistent/box-volume", nil },
				Geteuid:    func() int { return os.Geteuid() },
			}
			s, err := p.settings()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("no error, got %+v", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.Path != path {
				t.Fatalf("Path = %q, want %q", s.Path, path)
			}
			if s.Limactl != tc.wantLimactl {
				t.Fatalf("Limactl = %q, want %q", s.Limactl, tc.wantLimactl)
			}
			if s.LimaHome != tc.wantLimaHome {
				t.Fatalf("LimaHome = %q, want %q", s.LimaHome, tc.wantLimaHome)
			}
		})
	}
}

// TestSettingsNextToTheBinary covers the second place a settings file may live:
// next to the plugin executable, which is how a release install and the
// integration harness carry their own. The fixed path is not writable for an
// unprivileged install, and Nomad passes no environment to a host volume
// plugin, so this is the only place such an install can configure itself.
func TestSettingsNextToTheBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "box-volume")
	if err := os.WriteFile(exe+settingsSuffix, []byte(`{"limactl":"/opt/limactl","lima_home":"/opt/lima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{
		Getenv:     func(string) string { return "" },
		Executable: func() (string, error) { return exe, nil },
		Geteuid:    func() int { return os.Geteuid() },
	}
	s, err := p.settings()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Path != exe+settingsSuffix {
		t.Fatalf("Path = %q, want %q", s.Path, exe+settingsSuffix)
	}
	if s.Limactl != "/opt/limactl" || s.LimaHome != "/opt/lima" {
		t.Fatalf("settings = %+v", s)
	}
}

// TestSettingsMissingIsAPlainError keeps a node without the file from running
// limactl with settings nobody chose.
func TestSettingsMissingIsAPlainError(t *testing.T) {
	got := run(t, runCase{argv: []string{"create"}, missingSettings: true, env: map[string]string{
		envVolumesDir: t.TempDir(),
		envVolumeID:   "vol-1234",
	}})
	if got.exit == 0 {
		t.Fatalf("create without a settings file succeeded")
	}
	msg, _ := decode(t, got.stdout)["error"].(string)
	if !strings.Contains(msg, defaultSettingsPath) {
		t.Fatalf("error does not name %s: %q", defaultSettingsPath, msg)
	}

	// The same for delete: the instance cannot be removed without the settings.
	got = run(t, runCase{argv: []string{"delete"}, missingSettings: true, env: map[string]string{
		envVolumesDir: t.TempDir(),
		envVolumeID:   "vol-1234",
	}})
	if got.exit == 0 {
		t.Fatalf("delete without a settings file succeeded")
	}
}

// TestVolumeParametersAreRejected is the boundary this plugin is built around:
// nothing that a volume request carries may decide which binary runs on the node,
// which Lima home it uses or which user owns the volumes.
func TestVolumeParametersAreRejected(t *testing.T) {
	cases := map[string]string{
		"the old plugin keys":   `{"limactl":"/opt/limactl","lima_home":"/srv/lima","runtime_user":"dev"}`,
		"one of them":           `{"runtime_user":"dev"}`,
		"a key nobody knows":    `{"something_else":"x"}`,
		"an empty-looking file": `{"":""}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			for _, op := range []string{"create", "delete"} {
				got := run(t, runCase{argv: []string{op}, env: map[string]string{
					envVolumesDir: t.TempDir(),
					envVolumeID:   "vol-1234",
					envParameters: raw,
				}})
				if got.exit == 0 {
					t.Fatalf("%s accepted the volume parameters %s", op, raw)
				}
				msg, _ := decode(t, got.stdout)["error"].(string)
				if !strings.Contains(msg, "parameters") {
					t.Fatalf("%s error does not explain the refusal: %q", op, msg)
				}
				if len(got.calls) != 0 {
					t.Fatalf("%s called limactl anyway: %v", op, got.calls)
				}
			}
		})
	}

	// An empty parameter set is what Nomad sends for a volume without any, and
	// is not a refusal.
	got := run(t, runCase{argv: []string{"create"}, env: map[string]string{
		envVolumesDir: t.TempDir(),
		envVolumeID:   "vol-1234",
		envParameters: `{}`,
	}})
	if got.exit != 0 {
		t.Fatalf("create with {} parameters failed: %s", got.stdout)
	}
}

func TestBadInvocations(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		env  map[string]string
	}{
		{name: "no operation", argv: nil},
		{name: "unknown operation", argv: []string{"destroy"}},
		{name: "empty operation", argv: []string{""}},
		{name: "delete without volumes dir", argv: []string{"delete"}, env: map[string]string{envVolumeID: "vol-1234"}},
		{name: "delete with a volume request that carries parameters", argv: []string{"delete"}, env: map[string]string{
			envVolumesDir: t.TempDir(), envVolumeID: "vol-1234", envParameters: `{"limactl":"/opt/limactl"}`,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == nil {
				tc.env = map[string]string{}
			}
			got := run(t, runCase{argv: tc.argv, env: tc.env})
			if got.exit == 0 {
				t.Fatalf("exit = 0, want non-zero")
			}
			body := decode(t, got.stdout)
			if msg, _ := body["error"].(string); msg == "" {
				t.Fatalf("no error message in %s", got.stdout)
			}
		})
	}
}

func TestValidVolumeID(t *testing.T) {
	cases := map[string]bool{
		"c9b69c08-0c33-4d29-9d2c-0d3df1f2a1b0": true,
		"boxdisk":                              true,
		"Box_Disk.1":                           true,
		"":                                     false,
		".":                                    false,
		"..":                                   false,
		"-flag":                                false,
		"a/b":                                  false,
		`a\\b`:                                 false,
	}
	for id, want := range cases {
		if got := validVolumeID(id); got != want {
			t.Fatalf("validVolumeID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestValidBoxID(t *testing.T) {
	cases := map[string]bool{
		"a":                     true,
		"box-01":                true,
		strings.Repeat("a", 40): true,
		"":                      false,
		strings.Repeat("a", 41): false,
		"-a":                    false,
		"a/b":                   false,
		"a.b":                   false,
		"Box":                   false,
		"a b":                   false,
	}
	for id, want := range cases {
		if got := validBoxID(id); got != want {
			t.Fatalf("validBoxID(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestRuntimeUserSetting checks the runtime_user setting: it is the user the
// plugin gives the volume directory to and runs limactl as.
func TestRuntimeUserSetting(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	volumesDir := t.TempDir()
	env := map[string]string{
		envVolumesDir:  volumesDir,
		envVolumeID:    "vol-1234",
		envCapacityMax: "4096",
	}
	settings := `{"runtime_user":"` + me.Username + `"}`
	got := run(t, runCase{argv: []string{"create"}, env: env, settings: settings})
	if got.exit != 0 {
		t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
	}

	// delete runs limactl as that user, with that user's environment.
	if err := os.WriteFile(filepath.Join(volumesDir, "vol-1234", "box-a.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = run(t, runCase{argv: []string{"delete"}, env: env, settings: settings})
	if got.exit != 0 {
		t.Fatalf("delete: exit = %d, stdout = %s", got.exit, got.stdout)
	}
	if len(got.ids) != 2 || got.ids[0] == nil {
		t.Fatalf("limactl ran without an identity: %v", got.ids)
	}
	if got.ids[0].Name != me.Username {
		t.Fatalf("limactl identity = %q, want %q", got.ids[0].Name, me.Username)
	}
	seen := map[string]string{}
	for _, kv := range got.envs[0] {
		name, value, _ := strings.Cut(kv, "=")
		seen[name] = value
	}
	if seen["HOME"] != me.HomeDir {
		t.Fatalf("limactl HOME = %q, want %q", seen["HOME"], me.HomeDir)
	}
	if seen["LIMA_HOME"] == "" {
		t.Fatalf("limactl runs without LIMA_HOME: %v", got.envs[0])
	}
	if _, ok := seen["DHV_OPERATION"]; ok {
		t.Fatalf("limactl inherited Nomad's environment: %v", got.envs[0])
	}
}

// TestCreateWithoutRuntimeUserUsesThePluginUser covers a plugin that runs as an
// unprivileged user and names no runtime_user: the volume directory belongs to
// that user, and the box driver gives it to the runtime user at the first start.
func TestCreateWithoutRuntimeUserUsesThePluginUser(t *testing.T) {
	volumesDir := t.TempDir()
	got := run(t, runCase{argv: []string{"create"}, env: map[string]string{
		envVolumesDir: volumesDir,
		envVolumeID:   "vol-1234",
	}})
	if got.exit != 0 {
		t.Fatalf("exit = %d, stdout = %s", got.exit, got.stdout)
	}
	if info, err := os.Stat(filepath.Join(volumesDir, "vol-1234")); err != nil || !info.IsDir() {
		t.Fatalf("volume dir not created: %v", err)
	}
}

// TestRuntimeUserRootIsRejected checks that a typo or root is reported instead of
// silently ignored: Lima does not run as root.
func TestRuntimeUserRootIsRejected(t *testing.T) {
	volumesDir := t.TempDir()
	env := map[string]string{
		envVolumesDir: volumesDir,
		envVolumeID:   "vol-1234",
	}
	settings := `{"runtime_user":"root"}`
	for _, op := range []string{"create", "delete"} {
		got := run(t, runCase{argv: []string{op}, env: env, settings: settings})
		if got.exit == 0 {
			t.Fatalf("%s with runtime_user=root succeeded", op)
		}
		msg, _ := decode(t, got.stdout)["error"].(string)
		if !strings.Contains(msg, "root") {
			t.Fatalf("%s error does not explain the root refusal: %q", op, msg)
		}
		if len(got.calls) != 0 {
			t.Fatalf("%s called limactl anyway: %v", op, got.calls)
		}
	}

	got := run(t, runCase{argv: []string{"create"}, env: env, settings: `{"runtime_user":"box-no-such-user-77bd"}`})
	if got.exit == 0 {
		t.Fatalf("create with an unknown runtime_user succeeded")
	}
}

// TestRootPluginWithoutRuntimeUserIsRefused is the case that used to fail late:
// a plugin that runs as root (Nomad's normal client) with no runtime_user would
// run limactl as root and only discover it at delete time.
func TestRootPluginWithoutRuntimeUserIsRefused(t *testing.T) {
	p := &Plugin{
		Getenv:  func(string) string { return "" },
		Geteuid: func() int { return 0 },
		Executable: func() (string, error) {
			return "/nonexistent/box-volume", nil
		},
	}
	settings := Settings{Path: "/etc/box/volume.json", Limactl: "limactl", LimaHome: "/srv/lima"}
	_, err := p.identityFor(settings)
	if err == nil {
		t.Fatal("a root plugin without runtime_user resolved an identity")
	}
	if !strings.Contains(err.Error(), "runtime_user") || !strings.Contains(err.Error(), settings.Path) {
		t.Fatalf("error does not name the setting and the file: %v", err)
	}
}
