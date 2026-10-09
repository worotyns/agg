package model

import (
	"math"
	"testing"
)

func TestQuantileAccuracy(t *testing.T) {
	// Values spread over eight orders of magnitude: every reported quantile stays within 2.5% of the exact one.
	var vals []float64
	counts := map[int]int64{}
	for v := 0.001; v < 1e7; v *= 1.37 {
		vals = append(vals, v)
		counts[HistIndex(v)]++
	}
	var bins []HistBin
	for i := histMinIdx; i <= histMaxIdx; i++ {
		if n := counts[i]; n > 0 {
			bins = append(bins, HistBin{Idx: i, Count: n})
		}
	}
	for _, q := range []float64{0.01, 0.5, 0.9, 0.95, 0.99, 1} {
		exact := vals[int(math.Ceil(q*float64(len(vals))))-1]
		if got := Quantile(bins, q); math.Abs(got-exact)/exact > 0.025 {
			t.Errorf("q=%v: got %v, exact %v", q, got, exact)
		}
	}
}

func TestHistEdges(t *testing.T) {
	if HistIndex(0) != HistZeroBin || HistIndex(-3) != HistZeroBin || HistIndex(math.NaN()) != HistZeroBin {
		t.Error("zero, negative and NaN values must share the zero bin")
	}
	if HistValue(HistZeroBin) != 0 {
		t.Error("zero bin value must be 0")
	}
	if HistIndex(1e30) != histMaxIdx || HistIndex(1e-30) != histMinIdx {
		t.Error("out of range values must clamp")
	}
	if Quantile(nil, 0.95) != 0 {
		t.Error("empty histogram must give 0")
	}
}
