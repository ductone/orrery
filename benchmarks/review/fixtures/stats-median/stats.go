// Package stats has small numeric helpers.
package stats

// Sum returns the sum of v.
func Sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

// Mean returns the arithmetic mean of v, or 0 for an empty slice.
func Mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return Sum(v) / float64(len(v))
}
