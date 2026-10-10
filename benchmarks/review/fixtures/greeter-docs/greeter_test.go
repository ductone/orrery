package greeter

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet("Ada"); got != "Hello, Ada!" {
		t.Fatal(got)
	}
	if got := Greet(""); got != "Hello, world!" {
		t.Fatal(got)
	}
}

func TestGreetAll(t *testing.T) {
	got := GreetAll([]string{"a", "b"})
	if len(got) != 2 || got[1] != "Hello, b!" {
		t.Fatal(got)
	}
}
