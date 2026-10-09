package model

import "math"

// Percentiles are estimated from a log-scaled histogram kept per bucket: bin i covers (γ^(i-1), γ^i] with γ = 1.05,
// so a reported percentile is within ≈2.5% of the exact value. Values ≤ 0 share one bin and count as 0.
const (
	histGamma   = 1.05
	histMinIdx  = -430 // ≈ 1e-9
	histMaxIdx  = 570  // ≈ 1e12
	HistZeroBin = -1000
)

var histLogGamma = math.Log(histGamma)

// HistBin is one histogram bin: its index and how many values fell into it.
type HistBin struct {
	Idx   int
	Count int64
}

// HistIndex returns the histogram bin of a value.
func HistIndex(v float64) int {
	if v <= 0 || math.IsNaN(v) {
		return HistZeroBin
	}
	i := int(math.Ceil(math.Log(v) / histLogGamma))
	if i < histMinIdx {
		return histMinIdx
	}
	if i > histMaxIdx {
		return histMaxIdx
	}
	return i
}

// HistValue returns the representative value of a bin (the point with the smallest relative error).
func HistValue(idx int) float64 {
	if idx == HistZeroBin {
		return 0
	}
	return 2 * math.Pow(histGamma, float64(idx)) / (histGamma + 1)
}

// Quantile estimates the q-quantile (0..1) of the values in bins, which must be sorted by Idx ascending.
// It returns 0 for an empty histogram.
func Quantile(bins []HistBin, q float64) float64 {
	var total int64
	for _, b := range bins {
		total += b.Count
	}
	if total == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for _, b := range bins {
		seen += b.Count
		if seen >= rank {
			return HistValue(b.Idx)
		}
	}
	return HistValue(bins[len(bins)-1].Idx)
}
