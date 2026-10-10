// Package stats has small numeric helpers.
package stats

import "sort"

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

// Median returns the median of v, or 0 for an empty slice.
func Median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}
