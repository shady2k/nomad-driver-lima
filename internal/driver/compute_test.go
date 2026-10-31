package driver

import (
	"fmt"
	"math"
	"testing"
)

func TestComputeUsesNativeSchedulerScale(t *testing.T) {
	for _, v := range []struct {
		total, cores uint64
		want         int
	}{{48, 14, 4}, {55488, 14, 3964}, {22400, 8, 2800}} {
		got, e := nomadComputePerCore(v.total, v.cores)
		if e != nil || got != v.want {
			t.Fatal(v, got, e)
		}
	}
	if _, e := nomadComputePerCore(0, 14); e == nil {
		t.Fatal("unknown compute accepted")
	}
	if _, e := nomadComputePerCore(48, 0); e == nil {
		t.Fatal("unknown core count accepted")
	}
}
func TestMetricsDeclareProcessScopeNotGuestRAM(t *testing.T) {
	a := metricAttributes(4)
	scope, ok := a["driver.lima.metrics.memory.scope"].GetString()
	if !ok || scope != "host_vm_process_rss" {
		t.Fatal(scope)
	}
	for _, k := range []string{"driver.lima.metrics.memory.guest_usage", "driver.lima.metrics.memory.complete_vm_memory"} {
		v, ok := a[k].GetBool()
		if !ok || v {
			t.Fatal(k)
		}
	}
	n, _ := a["driver.lima.cpu.nomad_compute_per_core"].GetInt()
	if n != 4 {
		t.Fatal("hidden CPU scale repair", n)
	}
}
func TestRecordedM4DependencyParserDefect(t *testing.T) {
	var gen int
	n, e := fmt.Sscanf("Apple M4 Pro", "Apple M%", &gen)
	if e == nil || n != 0 || gen != 0 {
		t.Fatal("recorded upstream defect no longer reproduced", n, e, gen)
	}
	if _, e = fmt.Sscanf("Apple M4 Pro", "Apple M%d", &gen); e != nil || gen != 4 {
		t.Fatal(gen, e)
	}
	// This only reproduces an upstream bug; no dependency or live Nomad is patched.
}

func TestReservationCannotWrapCPUOrRAM(t *testing.T) {
	if !reservationCovers(2, 2800, 1024, 128, 5600, 1152) {
		t.Fatal("covered request rejected")
	}
	if reservationCovers(math.MaxInt64, 2800, 1024, 128, 5600, 1152) {
		t.Fatal("CPU multiplication wrap admitted")
	}
	if reservationCovers(1, 2800, math.MaxInt64, 128, 2800, 1152) {
		t.Fatal("memory addition wrap admitted")
	}
	if reservationCovers(1, 2800, 1024, 128, 2800, math.MinInt64) {
		t.Fatal("negative memory admitted")
	}
}
