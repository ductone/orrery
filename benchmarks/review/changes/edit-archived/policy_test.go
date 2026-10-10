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

func TestCanEditArchived(t *testing.T) {
	admin := User{ID: "a", Admin: true}
	other := User{ID: "x"}
	d := Doc{Owner: "o", Archived: true}
	if !CanEdit(admin, d) {
		t.Fatal("admin should edit archived doc")
	}
	if CanEdit(other, d) {
		t.Fatal("stranger must not edit archived doc")
	}
}
