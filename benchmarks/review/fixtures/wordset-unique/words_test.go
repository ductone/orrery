package wordset

import "testing"

func TestCount(t *testing.T) {
	got := Count([]string{"Go", "go", "rust"})
	if got["go"] != 2 || got["rust"] != 1 || len(got) != 2 {
		t.Fatal(got)
	}
}
