package kv

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	in := `# comment
a=1
empty=
quoted="hello world"
eq="x=y"
blank=""
 spaced = 2

url=http://h/?q=1
`
	want := map[string]string{
		"a":      "1",
		"empty":  "",
		"quoted": "hello world",
		"eq":     "x=y",
		"blank":  "",
		"spaced": "2",
		"url":    "http://h/?q=1",
	}
	if got := Parse(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %#v, want %#v", got, want)
	}
}
