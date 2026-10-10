package users

import "testing"

func TestValidUser(t *testing.T) {
	if err := Validate(map[string]string{"id": "1", "name": "Ada", "email": "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingID(t *testing.T) {
	if Validate(map[string]string{"name": "Ada", "email": "ada@example.com"}) == nil {
		t.Fatal("a user without an id must be invalid")
	}
}
