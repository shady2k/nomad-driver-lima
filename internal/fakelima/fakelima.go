// Package fakelima provides a fake `limactl` for tests. The fake keeps its state
// in $LIMA_HOME, so the code under test drives it through the real command line
// boundary (exec, LIMA_HOME, stdout JSON) instead of a Go mock.
package fakelima

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed testdata/fake-limactl.py
var script string

// Fake is an installed fake limactl with its own LIMA_HOME.
type Fake struct {
	Bin  string
	Home string
}

// New installs the fake limactl in a fresh temp directory.
func New(t testing.TB) *Fake {
	t.Helper()
	// Not t.TempDir: its path carries the test's name, and the box manager
	// refuses a lima home too long for Lima's sockets, as it would on a node.
	root, err := os.MkdirTemp("", "fl")
	if err != nil {
		t.Fatalf("create fake lima root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake limactl directory: %v", err)
	}
	bin := filepath.Join(binDir, "limactl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake limactl: %v", err)
	}
	home := filepath.Join(root, "lima-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create fake lima home: %v", err)
	}
	return &Fake{Bin: bin, Home: home}
}

func (f *Fake) path(name string) string { return filepath.Join(f.Home, name) }

// SetMode installs failure switches for the next calls.
func (f *Fake) SetMode(t testing.TB, mode map[string]any) {
	t.Helper()
	data, err := json.Marshal(mode)
	if err != nil {
		t.Fatalf("encode fake mode: %v", err)
	}
	if err := os.WriteFile(f.path(".fake-mode.json"), data, 0o644); err != nil {
		t.Fatalf("write fake mode: %v", err)
	}
}

// State returns the fake instance table.
func (f *Fake) State(t testing.TB) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(f.path(".fake-state.json"))
	if os.IsNotExist(err) {
		return map[string]map[string]any{}
	}
	if err != nil {
		t.Fatalf("read fake state: %v", err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode fake state: %v", err)
	}
	return out
}

// SetState merges fields into one fake instance.
func (f *Fake) SetState(t testing.TB, name string, fields map[string]any) {
	t.Helper()
	state := f.State(t)
	inst, ok := state[name]
	if !ok {
		inst = map[string]any{}
	}
	for k, v := range fields {
		inst[k] = v
	}
	state[name] = inst
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode fake state: %v", err)
	}
	if err := os.WriteFile(f.path(".fake-state.json"), data, 0o644); err != nil {
		t.Fatalf("write fake state: %v", err)
	}
}

// RemoveInstance drops one fake instance from the table, as a `limactl delete`
// done behind the driver's back would.
func (f *Fake) RemoveInstance(t testing.TB, name string) {
	t.Helper()
	state := f.State(t)
	delete(state, name)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode fake state: %v", err)
	}
	if err := os.WriteFile(f.path(".fake-state.json"), data, 0o644); err != nil {
		t.Fatalf("write fake state: %v", err)
	}
}

// Calls returns every recorded limactl invocation.
func (f *Fake) Calls(t testing.TB) []string {
	t.Helper()
	return readLines(t, f.path(".fake-calls.log"))
}

// Copies returns every recorded `limactl copy` invocation and every file of a
// tar stream sent to `limactl shell`, as "name<TAB>mode<TAB>uid:gid<TAB>sha256".
func (f *Fake) Copies(t testing.TB) []string {
	t.Helper()
	return readLines(t, f.path(".fake-copies.log"))
}

func readLines(t testing.TB, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ResetCalls forgets the recorded calls.
func (f *Fake) ResetCalls(t testing.TB) {
	t.Helper()
	_ = os.Remove(f.path(".fake-calls.log"))
	_ = os.Remove(f.path(".fake-copies.log"))
}

// ImageServer serves a fixed image payload for tests and returns its URL.
func ImageServer(t testing.TB, payload string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/image.img"
}

// InstanceDir returns the instance directory the fake would use.
func (f *Fake) InstanceDir(name string) string { return filepath.Join(f.Home, name) }

// DiskPath returns the instance disk path.
func (f *Fake) DiskPath(name string) string { return filepath.Join(f.Home, name, "disk") }

// HasCall reports whether any recorded call starts with prefix.
func (f *Fake) HasCall(t testing.TB, prefix string) bool {
	t.Helper()
	return len(f.CallsWith(t, prefix)) > 0
}

// CallsWith returns the recorded calls that start with prefix.
func (f *Fake) CallsWith(t testing.TB, prefix string) []string {
	t.Helper()
	var out []string
	for _, c := range f.Calls(t) {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}
