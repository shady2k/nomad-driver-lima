package driver

import (
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryFailureCannotCreateSecondInvocation(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	if err := d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	cold := setup(t, f, root)
	// This is Nomad's failed-handle restore followed by a new invocation ID,
	// not an allocation reschedule or task restart event.
	h := drivers.NewTaskHandle(1)
	h.Config = cfg
	if err := cold.RecoverTask(h); err == nil || !strings.Contains(err.Error(), "recover handle decode") {
		t.Fatalf("decode evidence missing: %v", err)
	}
	next := cfg.Copy()
	next.ID = cfg.AllocID + "/work/new-invocation"
	if _, _, err := cold.StartTask(next); err == nil || !strings.Contains(err.Error(), "previous") && !strings.Contains(err.Error(), "already has") {
		t.Fatalf("duplicate not refused: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 1 || f.launches != 1 {
		t.Fatalf("duplicate effects: create=%d start=%d launch=%d", f.creates, f.starts, f.launches)
	}
}

func TestRecoveryDistinguishesLedgerEOFWithoutCreatingVM(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	h, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := lima.Hash([]byte(cfg.ID))
	if err = os.WriteFile(filepath.Join(d.store.Root, key+".json"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cold := setup(t, f, root)
	if err = cold.RecoverTask(h); err == nil || !strings.Contains(err.Error(), "ledger decode (bytes=0)") {
		t.Fatalf("wrong failure stage: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 1 || f.launches != 1 {
		t.Fatal("recovery replayed effects")
	}
}

func TestColdDestroyStopsExactOwnedRuntimeWithoutReplay(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	if err := d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	cold := setup(t, f, root)
	if err := cold.DestroyTask(cfg.ID, true); err != nil {
		t.Fatal(err)
	}
	s, err := cold.store.read(lima.Hash([]byte(cfg.ID)))
	if err != nil || !s.Destroyed || s.Phase != "destroyed_disk_retained" {
		t.Fatalf("cleanup state: %v %v", s, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 1 || f.launches != 1 || f.stops != 1 || f.deletes != 0 {
		t.Fatalf("unexpected cold effects create/start/launch/stop/delete=%d/%d/%d/%d/%d", f.creates, f.starts, f.launches, f.stops, f.deletes)
	}
}

func TestVersionedRecoveryRejectsChangedIdentityAndOmitsSecrets(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	cfg.Env = map[string]string{"SYNTHETIC_TEST_VALUE": "synthetic-not-for-driver-state"}
	h, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(h.DriverState), "synthetic-not-for-driver-state") || strings.Contains(string(h.DriverState), "/bin/sh") {
		t.Fatal("env or command leaked into DriverState")
	}
	if err = d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Config = h.Config.Copy()
	h.Config.AllocID = "another-allocation"
	cold := setup(t, f, root)
	if err = cold.RecoverTask(h); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("wrong identity accepted: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.launches != 1 {
		t.Fatal("recovery replay")
	}
}
