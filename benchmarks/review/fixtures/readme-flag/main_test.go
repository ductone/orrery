package main

import "testing"

func TestGreeting(t *testing.T) {
	if got := greeting("Ada", false); got != "Hello, Ada!" {
		t.Fatalf("greeting = %q", got)
	}
}
