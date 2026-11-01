package driver

import (
	"math"
	"testing"
	"time"
)

func TestCPUPercentIsOneCoreAndMonotonic(t *testing.T) {
	v, e := cpuPercent(10, 14, 2*time.Second)
	if e != nil || v != 200 {
		t.Fatalf("two cores = 200%%: %v %v", v, e)
	}
	for _, x := range []float64{9, math.NaN(), math.Inf(1)} {
		if _, e := cpuPercent(10, x, time.Second); e == nil {
			t.Fatal("invalid sample accepted")
		}
	}
	if _, e := cpuPercent(1, 2, 0); e == nil {
		t.Fatal("zero interval accepted")
	}
}
