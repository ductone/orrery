package core

import (
	"encoding/json"
	"testing"
)

func TestModelJSON(t *testing.T) {
	const obj = `{"memory_candidates":[{"kind":"fact","text":"a } in text"}]}`
	for name, c := range map[string]struct{ in, want string }{
		"bare":            {obj, obj},
		"bare padded":     {"\n  " + obj + "\n", obj},
		"array":           {`[1,2]`, `[1,2]`},
		"fenced json":     {"```json\n" + obj + "\n```", obj},
		"fenced":          {"```\n" + obj + "\n```", obj},
		"fenced one line": {"```" + obj + "```", obj},
		"fence in prose":  {"Here you go:\n```json\n" + obj + "\n```\nLet me know.", obj},
		"prose":           {"Here are the candidates: " + obj + " That is all.", obj},
		"unclosed fence":  {"```json\n" + obj, obj},
		"two values":      {`first {"a":1} then {"b":2}`, `first {"a":1} then {"b":2}`},
		"no json":         {"  nothing here ", "nothing here"},
	} {
		got := modelJSON(c.in)
		if got != c.want {
			t.Errorf("%s: modelJSON = %q, want %q", name, got, c.want)
		}
	}
	var out struct {
		Candidates []MemoryCandidate `json:"memory_candidates"`
	}
	if err := json.Unmarshal([]byte(modelJSON("```json\n"+obj+"\n```")), &out); err != nil || len(out.Candidates) != 1 {
		t.Fatalf("fenced extraction output: %v %+v", err, out)
	}
}
