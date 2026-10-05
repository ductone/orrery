package mathx

import "testing"

func TestClamp(t *testing.T) {
	for _, tt := range [][4]int{{5, 0, 10, 5}, {-1, 0, 10, 0}, {11, 0, 10, 10}} {
		if got := Clamp(tt[0], tt[1], tt[2]); got != tt[3] {
			t.Errorf("Clamp(%d, %d, %d) = %d, want %d", tt[0], tt[1], tt[2], got, tt[3])
		}
	}
}
