package driver

import (
	"nomad-driver-lima/internal/lima"
	"strings"
	"testing"
)

func TestQuotaRefusalReportsExactBoundedNumbersWithoutReservation(t *testing.T) {
	d := setup(t, newFake(), "")
	d.config.MaxTotalDiskGiB = 100
	s := &State{Version: 1, Key: lima.Hash([]byte("retained-quota")), Owner: d.store.Owner, TaskID: "retained-quota", Phase: "destroyed_disk_retained", Destroyed: true, DiskGiB: 88}
	if e := d.store.write(s); e != nil {
		t.Fatal(e)
	}
	e := d.checkBudget(TaskConfig{CPUs: 2, MemoryMiB: 4096, DiskGiB: 30})
	if e == nil || !strings.Contains(e.Error(), "resource=disk_gib requested=30 used=88 total=118 limit=100") {
		t.Fatal(e)
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].DiskGiB != 88 {
		t.Fatal("refusal mutated reservation", e)
	}
}
