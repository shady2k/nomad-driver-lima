package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (Entry, Cache) {
	t.Helper()
	d := t.TempDir()
	a := func(name, body string) Artifact {
		p := filepath.Join(d, name)
		os.WriteFile(p, []byte(body), 0600)
		return Artifact{URL: "file://" + p, SHA256: Sum([]byte(body)), SizeBytes: int64(len(body))}
	}
	disk := a("disk", "synthetic boot disk")
	disk.Format = "qcow2"
	disk.VirtualSizeBytes = 1 << 30
	disk.MinimumDiskGiB = 1
	return Entry{ID: "agent-base", Version: "v1", GuestOS: "linux", Architecture: "amd64", VMType: "qemu", Disk: disk, YAML: a("runtime.yaml", "minimumLimaVersion: 2.2.1\n"), Acceptance: Acceptance{true, 2, true}}, Cache{Dir: filepath.Join(d, "cache")}
}
func TestPinnedCatalogRejectsUnacceptedAndWrongPlatform(t *testing.T) {
	v, c := fixture(t)
	raw, _ := json.Marshal(Catalog{1, []Entry{v}})
	p := filepath.Join(c.Dir, "..", "catalog.json")
	os.WriteFile(p, raw, 0600)
	doc, e := Read(p, Sum(raw))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = doc.Select(v.ID, v.Version, "arm64", "vz"); e == nil {
		t.Fatal("cross arch accepted")
	}
	if _, e = Read(p, strings.Repeat("0", 64)); e == nil {
		t.Fatal("unpinned accepted")
	}
	v.Acceptance.Sealed = false
	if v.Validate() == nil {
		t.Fatal("unsealed accepted")
	}
}
func TestCachePartialMismatchAndOfflineVerifiedHit(t *testing.T) {
	v, c := fixture(t)
	p, e := c.Fetch(t.Context(), v.Disk)
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(strings.TrimPrefix(v.Disk.URL, "file://"))
	if _, e = c.Fetch(t.Context(), v.Disk); e != nil {
		t.Fatal("verified cache hit fetched storage", e)
	}
	os.Chmod(p, 0600)
	os.WriteFile(p, []byte("tampered"), 0600)
	if _, e = c.Fetch(t.Context(), v.Disk); e == nil {
		t.Fatal("corrupt cache accepted")
	}
	matches, _ := filepath.Glob(filepath.Join(c.Dir, ".download-*"))
	if len(matches) != 0 {
		t.Fatal(matches)
	}
}
func TestMaterializeNoProvisionNoHostMountAndRepeat(t *testing.T) {
	v, c := fixture(t)
	p, e := c.Materialize(t.Context(), v)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), v.Disk.SHA256) || !strings.Contains(string(raw), "forwardAgent: false") {
		t.Fatal(string(raw))
	}
	if _, e = c.Materialize(t.Context(), v); e != nil {
		t.Fatal(e)
	}
	bad := []byte("provision: []\n")
	source := strings.TrimPrefix(v.YAML.URL, "file://")
	os.WriteFile(source, bad, 0600)
	v.YAML.SHA256 = Sum(bad)
	v.YAML.SizeBytes = int64(len(bad))
	if _, e = c.Materialize(context.Background(), v); e == nil {
		t.Fatal("boot install accepted")
	}
}
func TestNoSecretURLInUnavailableError(t *testing.T) {
	_, c := fixture(t)
	a := Artifact{URL: "http://invalid/secret?token=do-not-log", SHA256: strings.Repeat("a", 64), SizeBytes: 1}
	_, e := c.Fetch(t.Context(), a)
	if e == nil || strings.Contains(e.Error(), "do-not-log") {
		t.Fatal(e)
	}
}
