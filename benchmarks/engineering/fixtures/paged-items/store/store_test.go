package store

import "testing"

func TestAllOrdered(t *testing.T) {
	s := New()
	s.Add(Item{ID: "b", Name: "B"})
	s.Add(Item{ID: "a", Name: "A"})
	all := s.All()
	if len(all) != 2 || all[0].ID != "a" || all[1].ID != "b" {
		t.Fatalf("All = %v", all)
	}
}

func TestDelete(t *testing.T) {
	s := New()
	s.Add(Item{ID: "a"})
	if !s.Delete("a") || s.Delete("a") {
		t.Fatal("Delete reported wrong existence")
	}
}
