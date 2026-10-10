package stats

import "testing"

func TestSumMean(t *testing.T) {
	v := []float64{1, 2, 3, 4}
	if Sum(v) != 10 {
		t.Fatalf("sum = %v", Sum(v))
	}
	if Mean(v) != 2.5 {
		t.Fatalf("mean = %v", Mean(v))
	}
	if Mean(nil) != 0 {
		t.Fatal("mean of nil should be 0")
	}
}

func TestMedian(t *testing.T) {
	if got := Median([]float64{5, 1, 3}); got != 3 {
		t.Fatalf("odd: %v", got)
	}
	if got := Median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Fatalf("even: %v", got)
	}
	if got := Median(nil); got != 0 {
		t.Fatalf("empty: %v", got)
	}
}
