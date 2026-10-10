package sum

import "testing"

func TestInts(t *testing.T) {
	if got := Ints([]int{1, 2, 3}); got != 6 {
		t.Fatalf("Ints = %d", got)
	}
}
