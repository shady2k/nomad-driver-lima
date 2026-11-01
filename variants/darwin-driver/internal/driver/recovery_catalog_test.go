package driver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	log "github.com/hashicorp/go-hclog"
	trstate "github.com/hashicorp/nomad/client/allocrunner/taskrunner/state"
	nsstate "github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/catalog"
	"nomad-driver-lima/internal/lima"
)

func TestPinnedOriginalCatalogColdBoltRestoreWithoutNewLaunchAdmission(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{true: "legacy", false: "v2"}[legacy], func(t *testing.T) {
			root := t.TempDir()
			original, oldHash := autoCatalog(t, root)
			oldPath := filepath.Join(root, "original-catalog.config")
			oldBytes, e := os.ReadFile(filepath.Join(root, "catalog.json"))
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(oldPath, oldBytes, 0444)
			f := newFake()
			d := setup(t, f, root)
			d.config.CatalogFile = oldPath
			d.config.CatalogSHA256 = oldHash
			d.config.MaxTotalDiskGiB = 10
			d.config.VaultAddress = "https://secrets.example.invalid"
			cache := filepath.Join(d.config.LimaHome, "_catalog-cache")
			os.MkdirAll(cache, 0700)
			if e := os.Chown(cache, d.runtimeIdentity.uid, d.runtimeIdentity.gid); e != nil {
				t.Fatal(e)
			}
			cfg, tokenPath := tokenFixture(t)
			os.WriteFile(tokenPath, []byte("synthetic-catalog-cold-token"), 0600)
			cfg.Namespace = "workers"
			cfg.NodeID = "00000000-0000-4000-8000-000000000099"
			var tc TaskConfig
			cfg.DecodeDriverConfig(&tc)
			tc.Architecture = ""
			tc.TemplateID = "base"
			tc.TemplateVersion = "v1"
			tc.TemplateCatalogSHA256 = oldHash
			tc.CreatorAgentID = "00000000-0000-4000-8000-000000000001"
			tc.CreatorSessionID = "native-test"
			tc.Purpose = "catalog-cold"
			pins, _ := json.Marshal([]platformPin{pinFor(original.Templates[0]), pinFor(original.Templates[1])})
			tc.TemplateVariantsJSON = string(pins)
			cfg.EncodeConcreteDriverConfig(tc)
			h, _, e := d.StartTask(cfg)
			if e != nil {
				t.Fatal(e)
			}
			key := lima.Hash([]byte(cfg.ID))
			before, _ := d.store.read(key)
			if legacy {
				var fields map[string]string
				h.GetDriverState(&fields)
				delete(fields, "recovery")
				h = drivers.NewTaskHandle(1)
				h.Config = cfg
				h.SetDriverState(fields)
				before.CleanupVaultRequired = nil
				d.store.write(before)
			}
			dbRoot := t.TempDir()
			db, e := nsstate.NewBoltStateDB(log.NewNullLogger(), dbRoot)
			if e != nil {
				t.Fatal(e)
			}
			if e = db.PutTaskRunnerLocalState(cfg.AllocID, cfg.Name, &trstate.LocalState{TaskHandle: h}); e != nil {
				t.Fatal(e)
			}
			db.Close()
			d.Shutdown(t.Context())
			db, e = nsstate.NewBoltStateDB(log.NewNullLogger(), dbRoot)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			recovered, _, e := db.GetTaskRunnerState(cfg.AllocID, cfg.Name)
			if e != nil {
				t.Fatal(e)
			}
			var lost TaskConfig
			if e = recovered.TaskHandle.Config.DecodeDriverConfig(&lost); e == nil || !strings.Contains(e.Error(), "EOF") {
				t.Fatalf("exact SDK lost-private-config fixture: %v", e)
			}
			// Only the catalog pin changes. The old approved artifact remains separate.
			next := *original
			next.Templates = append([]catalog.Entry(nil), original.Templates...)
			next.Templates[0].Version = "v2"
			raw, _ := json.Marshal(next)
			newPath := filepath.Join(root, "new-catalog.config")
			os.WriteFile(newPath, raw, 0444)
			newHash := catalog.Sum(raw)
			cold := setup(t, f, root)
			cold.config.CatalogFile = newPath
			cold.config.CatalogSHA256 = newHash
			cold.config.RecoveryCatalogFile = oldPath
			cold.config.RecoveryCatalogSHA256 = oldHash
			cold.config.MaxTotalDiskGiB = 10
			cold.config.VaultAddress = "https://secrets.example.invalid"
			if e = os.Remove(tokenPath); e != nil {
				t.Fatal(e)
			} // Nomad Vault hook may restore the task file after RecoverTask returns.
			if e = cold.RecoverTask(recovered.TaskHandle); e != nil {
				t.Fatalf("native %s/%s original-pin recovery: %v", runtime.GOOS, runtime.GOARCH, e)
			}
			after, _ := cold.store.read(key)
			if after.Instance != before.Instance || after.MachineID != before.MachineID || after.BootID != before.BootID || after.TemplateCatalogSHA256 != oldHash || after.SpecHash != before.SpecHash || after.CreatorAgentID != before.CreatorAgentID {
				t.Fatal("recovery changed identity")
			}
			nextCfg := cfg.Copy()
			nextCfg.ID = cfg.AllocID + "/work/new-invocation"
			if _, _, e = cold.StartTask(nextCfg); e == nil {
				t.Fatal("old recovery pin became a launch permission")
			}
			f.mu.Lock()
			counts := []int{f.creates, f.starts, f.launches}
			f.mu.Unlock()
			if counts[0] != 1 || counts[1] != 1 || counts[2] != 1 {
				t.Fatal("replayed effects", counts)
			}
			if !bytes.Equal(recovered.TaskHandle.DriverState, h.DriverState) {
				t.Fatal("mutated durable SDK handle")
			}
			cold.Shutdown(t.Context())
		})
	}
}
func TestOriginalRecoveryCatalogRejectsUnapprovedOrChangedReceipt(t *testing.T) {
	root := t.TempDir()
	c, sum := autoCatalog(t, root)
	entry := c.Templates[0]
	if runtime.GOOS == "darwin" {
		entry = c.Templates[1]
	}
	p := filepath.Join(root, "catalog.json")
	d := setup(t, newFake(), root)
	d.config.CatalogFile = p
	d.config.CatalogSHA256 = strings.Repeat("f", 64)
	s := &State{TemplateCatalogSHA256: sum, TemplateID: entry.ID, TemplateVersion: entry.Version, ResolvedArchitecture: entry.Architecture, ResolvedVMType: entry.VMType, TemplateDiskSHA256: entry.Disk.SHA256, TemplateYAMLSHA256: entry.YAML.SHA256, DiskGiB: 1}
	if e := d.verifyRecoveryCatalog(s); e == nil {
		t.Fatal("unapproved previous catalog accepted")
	}
	d.config.RecoveryCatalogFile = p
	d.config.RecoveryCatalogSHA256 = sum
	if e := d.verifyRecoveryCatalog(s); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"disk", "yaml", "vm", "minimum"} {
		t.Run(kind, func(t *testing.T) {
			copy := *s
			switch kind {
			case "disk":
				copy.TemplateDiskSHA256 = strings.Repeat("a", 64)
			case "yaml":
				copy.TemplateYAMLSHA256 = strings.Repeat("b", 64)
			case "vm":
				copy.ResolvedVMType = "unknown"
			case "minimum":
				copy.DiskGiB = 0
			}
			if e := d.verifyRecoveryCatalog(&copy); e == nil {
				t.Fatal("changed selection allowed")
			}
		})
	}
	os.WriteFile(p, []byte("changed"), 0600)
	if e := d.verifyRecoveryCatalog(s); e == nil {
		t.Fatal("changed original receipt accepted")
	}
}
func TestRecoveryCatalogConfigPairsAndStateCollision(t *testing.T) {
	root := t.TempDir()
	_, sum := autoCatalog(t, root)
	p := filepath.Join(root, "catalog.json")
	base := Config{StateDir: filepath.Join(root, "state"), RecoveryCatalogFile: p, RecoveryCatalogSHA256: sum}
	if e := validateRecoveryCatalogConfig(base); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"missing-pin", "missing-file", "relative", "collision", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			c := base
			switch kind {
			case "missing-pin":
				c.RecoveryCatalogSHA256 = ""
			case "missing-file":
				c.RecoveryCatalogFile = ""
			case "relative":
				c.RecoveryCatalogFile = "catalog"
			case "collision":
				c.StateDir = root
			case "symlink":
				c.RecoveryCatalogFile = filepath.Join(root, "link")
				os.Symlink(p, c.RecoveryCatalogFile)
			}
			if e := validateRecoveryCatalogConfig(c); e == nil {
				t.Fatal("unsafe receipt config")
			}
		})
	}
}
