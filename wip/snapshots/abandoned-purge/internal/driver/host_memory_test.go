package driver

import (
	"nomad-driver-lima/internal/lima"
	"testing"
)

func TestRuntimeMemoryIncludesSurvivorsAndUntrackedGuests(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	f.infos["surviving"] = lima.Info{Name: "surviving", Status: "Running", CPUs: 1, Memory: 12 * 1024 * 1048576}
	f.infos["untracked"] = lima.Info{Name: "untracked", Status: "Running", CPUs: 1, Memory: 20 * 1024 * 1048576}
	f.infos["stopped"] = lima.Info{Name: "stopped", Status: "Stopped", CPUs: 1, Memory: 4 * 1024 * 1048576}
	rows := []*State{{Instance: "surviving", Destroyed: true, Phase: "stopped", MemoryMiB: 12 * 1024}, {Instance: "stopped", MemoryMiB: 4 * 1024}}
	got, err := d.runtimeMemoryCommitment(rows)
	want := int64(32*1024 + 2*d.config.MemoryOverheadMiB)
	if err != nil || got != want {
		t.Fatalf("commitment=%d want=%d err=%v", got, want, err)
	}
}

func TestRuntimeMemoryFailsClosedForUnknownConfiguredMemory(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	f.infos["unknown"] = lima.Info{Name: "unknown", Status: "Running"}
	if _, err := d.runtimeMemoryCommitment(nil); err == nil {
		t.Fatal("unknown running memory accepted")
	}
}

func TestIncidentConfiguredCommitmentExceedsOperatorAllocatable(t *testing.T) {
	f := newFake()
	d := setup(t, f, "")
	d.config.MaxTotalMemoryMiB = 30053
	for i, size := range []int64{4, 12, 4, 20} {
		name := string(rune('a' + i))
		f.infos[name] = lima.Info{Name: name, Status: "Running", CPUs: 1, Memory: size * 1024 * 1048576}
	}
	used, err := d.runtimeMemoryCommitment(nil)
	if err != nil || used != 41472 {
		t.Fatalf("incident commitment=%d err=%v", used, err)
	}
	if err = d.checkBudget(TaskConfig{CPUs: 1, MemoryMiB: 256, DiskGiB: 1}); err == nil {
		t.Fatal("host overcommit admitted")
	}
	// A fresh physical stop observation releases RAM without deleting its disk or
	// rewriting an old running ledger row.
	old := f.infos["b"]
	old.Status = "Stopped"
	f.infos["b"] = old
	used, err = d.runtimeMemoryCommitment([]*State{{Instance: "b", MemoryMiB: 12288, Phase: "running"}})
	if err != nil || used != 29056 {
		t.Fatalf("stopped OOM guest still charged RAM: %d %v", used, err)
	}
	resources, err := d.runtimeResourceCommitment([]*State{{Instance: "b", CPUs: 12, MemoryMiB: 12288, Phase: "running"}})
	if err != nil || resources.VMs != 3 || resources.CPUs != 3 {
		t.Fatalf("stopped OOM guest still charged compute: %+v %v", resources, err)
	}

}
