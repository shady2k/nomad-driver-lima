package lima

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeResourceEditArgsAreStoppedAndExactMiB(t *testing.T) {
	r := &recording{}
	c := &Client{Runner: r}
	if e := c.EditResources(t.Context(), "nomad-aaaaaaaaaaaaaaaaaaaaaaaa", 2, 513, 30); e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(r.args, " ")
	for _, want := range []string{"edit --tty=false", "--cpus=2", "--set=.memory = \"513MiB\"", "--disk=30"} {
		if !strings.Contains(joined, want) {
			t.Fatal(joined)
		}
	}
	if strings.Contains(joined, "--start") || strings.Contains(joined, "--force") {
		t.Fatal("implicit restart/force exposed")
	}
}
func TestNonResourceFingerprintCoversUnknownSettings(t *testing.T) {
	root := t.TempDir()
	name := "nomad-aaaaaaaaaaaaaaaaaaaaaaaa"
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "lima.yaml")
	c := &Client{Home: root}
	initial := "cpus: 1\nmemory: 512MiB\ndisk: 1GiB\nparam:\n  owner: immutable\n"
	os.WriteFile(path, []byte(initial), 0600)
	before, e := c.NonResourceConfigHash(name)
	if e != nil {
		t.Fatal(e)
	}
	changed := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(initial, "cpus: 1", "cpus: 3"), "512MiB", "513MiB"), "1GiB", "30GiB")
	os.WriteFile(path, []byte(changed), 0600)
	after, e := c.NonResourceConfigHash(name)
	if e != nil || before != after {
		t.Fatal("authorized resource-only edit changed semantic projection", e)
	}
	os.WriteFile(path, []byte(changed+"mounts: [unexpected]\n"), 0600)
	after, e = c.NonResourceConfigHash(name)
	if e != nil || before == after {
		t.Fatal("unrelated setting unbound", e)
	}
	os.WriteFile(path, []byte("cpus: 1\ncpus: 2\n"), 0600)
	if _, e = c.NonResourceConfigHash(name); e == nil {
		t.Fatal("ambiguous YAML accepted")
	}
}
