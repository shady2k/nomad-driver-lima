package driver

import (
	"bytes"
	"github.com/hashicorp/nomad/plugins/drivers"
	"testing"

	log "github.com/hashicorp/go-hclog"
	trstate "github.com/hashicorp/nomad/client/allocrunner/taskrunner/state"
	nsstate "github.com/hashicorp/nomad/client/state"
	"nomad-driver-lima/internal/lima"
	"os"
	"strings"
	"time"
)

func TestIndependentColdBoltReopen(t *testing.T) { testColdBoltReopen(t, false, false) }
func TestLegacyColdBoltReopen(t *testing.T)      { testColdBoltReopen(t, true, false) }
func TestVaultColdBoltReopen(t *testing.T)       { testColdBoltReopen(t, false, true) }
func TestLegacyVaultColdBoltReopen(t *testing.T) { testColdBoltReopen(t, true, true) }
func testColdBoltReopen(t *testing.T, legacy bool, vault bool) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	if vault {
		var tokenPath string
		cfg, tokenPath = tokenFixture(t)
		os.WriteFile(tokenPath, []byte("synthetic-cold-bolt-token"), 0600)
		d.config.VaultAddress = "https://secrets.example.invalid"
	}
	h, _, e := d.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if legacy {
		var fields map[string]string
		if e = h.GetDriverState(&fields); e != nil {
			t.Fatal(e)
		}
		delete(fields, "recovery")
		h = drivers.NewTaskHandle(1)
		h.Config = cfg
		if e = h.SetDriverState(fields); e != nil {
			t.Fatal(e)
		}
	}
	original := append([]byte(nil), h.DriverState...)
	if len(original) == 0 {
		t.Fatal("empty handle at StartTask")
	}
	dbdir := t.TempDir()
	db, e := nsstate.NewBoltStateDB(log.NewNullLogger(), dbdir)
	if e != nil {
		t.Fatal(e)
	}
	local := trstate.NewLocalState()
	local.TaskHandle = h
	if e = db.PutTaskRunnerLocalState(cfg.AllocID, cfg.Name, local); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	if e = d.Shutdown(t.Context()); e != nil {
		t.Fatal(e)
	}
	if legacy {
		old, err := d.store.read(lima.Hash([]byte(cfg.ID)))
		if err != nil {
			t.Fatal(err)
		}
		old.CleanupVaultRequired = nil
		if err = d.store.write(old); err != nil {
			t.Fatal(err)
		}
	}
	db, e = nsstate.NewBoltStateDB(log.NewNullLogger(), dbdir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	recovered, _, e := db.GetTaskRunnerState(cfg.AllocID, cfg.Name)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(original, recovered.TaskHandle.DriverState) {
		t.Fatal("Bolt reopen changed DriverState")
	}
	var beforeCfg, afterCfg TaskConfig
	if e = h.Config.DecodeDriverConfig(&beforeCfg); e != nil {
		t.Fatal("original config decode", e)
	}
	if e = recovered.TaskHandle.Config.DecodeDriverConfig(&afterCfg); e == nil || !strings.Contains(e.Error(), "EOF") {
		t.Fatalf("expected persisted raw config loss, got %v", e)
	}
	var stateMap map[string]string
	if e = recovered.TaskHandle.GetDriverState(&stateMap); e != nil {
		t.Fatal("DriverState decode", e)
	}
	if strings.Contains(stateMap["recovery"], "/bin/sh") {
		t.Fatal("command persisted")
	}
	d2 := setup(t, f, root)
	d2.config.VaultAddress = d.config.VaultAddress
	if e = d2.RecoverTask(recovered.TaskHandle); e != nil {
		t.Fatalf("cold recovery: %v", e)
	}
	f.mu.Lock()
	t.Logf("handle bytes=%d; creates=%d starts=%d launches=%d", len(original), f.creates, f.starts, f.launches)
	if f.creates != 1 || f.starts != 1 || f.launches != 1 {
		t.Fatal("replayed main")
	}
	f.mu.Unlock()
	if e = d2.StopTask(cfg.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
}
