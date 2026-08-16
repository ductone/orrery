package immutablelabelmerge

import (
	"reflect"
	"testing"
)

func TestMergeLabelsDoesNotMutateInputs(t *testing.T) {
	base := map[string]string{"region": "west", "tier": "standard"}
	overlay := map[string]string{"tier": "premium", "owner": "platform"}
	got := MergeLabels(base, overlay)
	want := map[string]string{"region": "west", "tier": "premium", "owner": "platform"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	if !reflect.DeepEqual(base, map[string]string{"region": "west", "tier": "standard"}) {
		t.Fatalf("base was mutated: %v", base)
	}
	if !reflect.DeepEqual(overlay, map[string]string{"tier": "premium", "owner": "platform"}) {
		t.Fatalf("overlay was mutated: %v", overlay)
	}
}

func TestMergeLabelsHandlesNilInputs(t *testing.T) {
	if got := MergeLabels(nil, map[string]string{"ready": "true"}); !reflect.DeepEqual(got, map[string]string{"ready": "true"}) {
		t.Fatalf("got=%v", got)
	}
	if got := MergeLabels(map[string]string{"ready": "true"}, nil); !reflect.DeepEqual(got, map[string]string{"ready": "true"}) {
		t.Fatalf("got=%v", got)
	}
}
