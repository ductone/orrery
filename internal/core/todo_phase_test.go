package core

import "testing"

func TestClampPhase(t *testing.T) {
	floors := []string{"plan", "diagnose"}
	for _, tc := range []struct {
		name, old, next string
		floors          []string
		want            string
	}{
		{"escalate into plan is clamped", "implement", "plan", floors, "implement"},
		{"escalate into diagnose is clamped", "review", "diagnose", floors, "review"},
		{"de-escalate out of plan is allowed", "plan", "implement", floors, "implement"},
		{"floored to floored is allowed", "plan", "diagnose", floors, "diagnose"},
		{"ordinary move is allowed", "implement", "review", floors, "review"},
		{"no floors configured clamps nothing", "implement", "plan", nil, "plan"},
	} {
		if got := clampPhase(tc.old, tc.next, tc.floors); got != tc.want {
			t.Errorf("%s: clampPhase(%q, %q) = %q, want %q", tc.name, tc.old, tc.next, got, tc.want)
		}
	}
}
