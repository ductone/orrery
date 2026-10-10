// Package sum adds numbers.
package sum

// Ints returns the sum of v.
func Ints(v []int) int {
	t := 0
	for _, x := range v {
		t += x
	}
	return t
}
