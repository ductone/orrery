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
