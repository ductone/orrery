package core

import "testing"

func TestRelevanceExperimentFailsOpenWithoutClient(t *testing.T) {
	if relevanceKeeps(t.Context(), nil, "objective", "read", "{}", "content") || lostFact(t.Context(), nil, DurableState{}, "fact") {
		t.Fatal("disabled Jev relevance must not retain or reject")
	}
}
