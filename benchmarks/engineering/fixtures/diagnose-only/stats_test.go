package stats

import "testing"

func TestMean(t *testing.T) {
	if got := Mean([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Fatalf("Mean = %v", got)
	}
	if Mean(nil) != 0 {
		t.Fatal("Mean(nil) != 0")
	}
}

func TestMedianOdd(t *testing.T) {
	if got := Median([]float64{5, 1, 3}); got != 3 {
		t.Fatalf("Median = %v", got)
	}
	if Median(nil) != 0 {
		t.Fatal("Median(nil) != 0")
	}
}
