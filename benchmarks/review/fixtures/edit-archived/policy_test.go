package policy

import "testing"

func TestCanEdit(t *testing.T) {
	owner := User{ID: "o"}
	admin := User{ID: "a", Admin: true}
	other := User{ID: "x"}
	d := Doc{Owner: "o"}
	if !CanEdit(owner, d) || !CanEdit(admin, d) || CanEdit(other, d) {
		t.Fatal("unexpected result for live doc")
	}
}
