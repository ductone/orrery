package slug

import "testing"

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Hello World":        "hello-world",
		"  Go -- 1.22 rocks": "go-1-22-rocks",
		"":                   "",
		"!!!":                "",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
