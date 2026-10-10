package shipping

import "testing"

func TestCost(t *testing.T) {
	cases := []struct {
		grams   int
		express bool
		want    int
	}{
		{0, false, 500}, {500, false, 500}, {500, true, 1200},
		{501, false, 900}, {2000, false, 900}, {2000, true, 1800},
		{2001, false, 1500}, {2001, true, 3000}, {-5, false, 500},
	}
	for _, c := range cases {
		if got := Cost(c.grams, c.express); got != c.want {
			t.Errorf("Cost(%d, %v) = %d, want %d", c.grams, c.express, got, c.want)
		}
	}
}
