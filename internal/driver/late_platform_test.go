package driver

import (
	"encoding/json"
	"nomad-driver-lima/internal/catalog"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func pinFor(e catalog.Entry) platformPin {
	return platformPin{e.Architecture, e.GuestOS, e.VMType, e.Disk.SHA256, e.YAML.SHA256, e.Disk.MinimumDiskGiB}
}
func autoCatalog(t *testing.T, root string) (*catalog.Catalog, string) {
	t.Helper()
	out := &catalog.Catalog{SchemaVersion: 1}
	for _, v := range []struct{ arch, vm string }{{"amd64", "qemu"}, {"arm64", "vz"}} {
		disk := []byte("disk-" + v.arch)
		yaml := []byte("minimumLimaVersion: 2.2.1\n")
		dp := filepath.Join(root, v.arch+".disk")
		yp := filepath.Join(root, v.arch+".yaml")
		os.WriteFile(dp, disk, 0600)
		os.WriteFile(yp, yaml, 0600)
		out.Templates = append(out.Templates, catalog.Entry{ID: "base", Version: "v1", Architecture: v.arch, VMType: v.vm, GuestOS: "linux", Acceptance: catalog.Acceptance{Sealed: true, IndependentBoots: 2, UniqueIdentities: true}, Disk: catalog.Artifact{URL: "file://" + dp, SHA256: catalog.Sum(disk), SizeBytes: int64(len(disk)), VirtualSizeBytes: 1 << 30, MinimumDiskGiB: 1, Format: "qcow2"}, YAML: catalog.Artifact{URL: "file://" + yp, SHA256: catalog.Sum(yaml), SizeBytes: int64(len(yaml))}})
	}
	raw, _ := json.Marshal(out)
	p := filepath.Join(root, "catalog.json")
	os.WriteFile(p, raw, 0600)
	return out, catalog.Sum(raw)
}
func TestLateResolvePinsAndPlatformNoPaths(t *testing.T) {
	c, _ := autoCatalog(t, t.TempDir())
	pins := []platformPin{pinFor(c.Templates[0]), pinFor(c.Templates[1])}
	raw, _ := json.Marshal(pins)
	tc := TaskConfig{TemplateID: "base", TemplateVersion: "v1", TemplateVariantsJSON: string(raw), DiskGiB: 1}
	for _, p := range []struct{ arch, os string }{{"amd64", "linux"}, {"arm64", "darwin"}} {
		e, err := resolvePin(c, tc, p.arch, p.os)
		if err != nil || e.Architecture != p.arch {
			t.Fatal(e, err)
		}
	}
	tc.Architecture = "amd64"
	if _, e := resolvePin(c, tc, "arm64", "darwin"); e == nil {
		t.Fatal("explicit filter lost")
	}
	tc.Architecture = ""
	tc.DiskGiB = 0
	if _, e := resolvePin(c, tc, "amd64", "linux"); e == nil {
		t.Fatal("minimum bypass")
	}
	tc.DiskGiB = 1
	for _, bad := range []string{strings.Replace(string(raw), pins[0].DiskSHA256, strings.Repeat("0", 64), 1), string(raw) + "{}", `[{"url":"file:///etc/passwd"}]`} {
		tc.TemplateVariantsJSON = bad
		if _, e := resolvePin(c, tc, "amd64", "linux"); e == nil {
			t.Fatal("bad candidate accepted")
		}
	}
	pins = append(pins, pins[0])
	raw, _ = json.Marshal(pins)
	tc.TemplateVariantsJSON = string(raw)
	if _, e := resolvePin(c, tc, "amd64", "linux"); e == nil {
		t.Fatal("duplicate accepted")
	}
}
func TestLateSelectionDurableRecoverSameVMAndCatalogDriftReject(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("native fixture")
	}
	if (runtime.GOOS == "linux" && runtime.GOARCH != "amd64") || (runtime.GOOS == "darwin" && runtime.GOARCH != "arm64") {
		t.Skip("fixture platform pair")
	}
	root := t.TempDir()
	c, sum := autoCatalog(t, root)
	pins := []platformPin{pinFor(c.Templates[0]), pinFor(c.Templates[1])}
	raw, _ := json.Marshal(pins)
	f := newFake()
	d := setup(t, f, root)
	d.config.CatalogFile = filepath.Join(root, "catalog.json")
	d.config.CatalogSHA256 = sum
	d.config.MaxTotalDiskGiB = 10
	cacheDir := filepath.Join(d.config.LimaHome, "_catalog-cache")
	if e := os.MkdirAll(cacheDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chown(cacheDir, d.runtimeIdentity.uid, d.runtimeIdentity.gid); e != nil {
		t.Fatal(e)
	}
	cfg := fixture(t)
	var tc TaskConfig
	cfg.DecodeDriverConfig(&tc)
	tc.Architecture = ""
	tc.TemplateID = "base"
	tc.TemplateVersion = "v1"
	tc.TemplateVariantsJSON = string(raw)
	tc.TemplateCatalogSHA256 = sum
	tc.CreatorAgentID = "00000000-0000-4000-8000-000000000001"
	tc.CreatorSessionID = "native-session"
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000002"
	tc.Purpose = "test"
	cfg.EncodeConcreteDriverConfig(tc)
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	s, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || s.ResolvedArchitecture != runtime.GOARCH || s.TemplateCatalogSHA256 != sum || s.TemplateYAMLSHA256 == "" {
		t.Fatal(s, e)
	}
	details := selectionAnnotations(s, "started")
	if details["template_disk_sha256"] != s.TemplateDiskSHA256 {
		t.Fatal(details)
	}
	d.Shutdown(t.Context())
	d2 := setup(t, f, root)
	d2.config.CatalogFile = d.config.CatalogFile
	d2.config.CatalogSHA256 = sum
	d2.config.MaxTotalDiskGiB = 10
	if e = d2.RecoverTask(h); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	counts := []int{f.creates, f.starts, f.launches}
	f.mu.Unlock()
	if counts[0] != 1 || counts[1] != 1 || counts[2] != 1 {
		t.Fatal("duplicate", counts)
	}
	d2.Shutdown(t.Context())
	d3 := setup(t, f, root)
	d3.config.CatalogFile = d.config.CatalogFile
	d3.config.CatalogSHA256 = strings.Repeat("0", 64)
	d3.config.MaxTotalDiskGiB = 10
	if e = d3.RecoverTask(h); e == nil {
		t.Fatal("catalog drift recovery reselected")
	}
}
