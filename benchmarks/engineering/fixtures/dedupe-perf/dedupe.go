// Package dedupe removes duplicate values.
package dedupe

// Ints returns xs without duplicates, keeping the first occurrence of each
// value in its original order.
func Ints(xs []int) []int {
	var out []int
	for _, x := range xs {
		seen := false
		for _, y := range out {
			if x == y {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, x)
		}
	}
	return out
}
