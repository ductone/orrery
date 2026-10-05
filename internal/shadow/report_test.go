package shadow

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/store"
)

func TestChecksCompareAnswersWithBaselinesAndOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  store.ShadowRecord
		want []Check
	}{
		{"phase disagrees", store.ShadowRecord{Site: Turn, Answers: `{"phase":{"type":"choice","choice":"diagnose","confidence":0.6}}`, Baseline: `{"routed_phase":"implement"}`}, []Check{{"phase_vs_routed", false, .6}}},
		{"review worker phase is forced", store.ShadowRecord{Site: Turn, Answers: `{"phase":{"type":"choice","choice":"explore","confidence":0.9}}`, Baseline: `{"routed_phase":"review","point":"review"}`}, nil},
		{"risk predicts rejection", store.ShadowRecord{Site: ReviewRisk, Answers: `{"introduces_bug":{"type":"noul","noul":0.7}}`, Outcome: `{"pass":false}`}, []Check{{"bug_vs_review", true, .4}}},
		{"risk before review lands", store.ShadowRecord{Site: ReviewRisk, Answers: `{"introduces_bug":{"type":"noul","noul":0.7}}`}, nil},
		{"verdict abstains", store.ShadowRecord{Site: ReviewVerdict, Answers: `{"verdict":{"type":"choice","choice":"no_verdict","confidence":0.9}}`, Outcome: `{"pass":true}`}, nil},
		{"verdict matches retry", store.ShadowRecord{Site: ReviewVerdict, Answers: `{"verdict":{"type":"choice","choice":"pass","confidence":0.9}}`, Outcome: `{"pass":true}`}, []Check{{"verdict_vs_retry", true, .9}}},
		{"unanswered", store.ShadowRecord{Site: Turn, Error: "timeout", Baseline: `{"routed_phase":"implement"}`}, nil},
	} {
		got := Checks(tc.rec)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i].Name != tc.want[i].Name || got[i].Agree != tc.want[i].Agree || got[i].Certainty-tc.want[i].Certainty > 1e-9 || tc.want[i].Certainty-got[i].Certainty > 1e-9 {
				t.Fatalf("%s: got %+v want %+v", tc.name, got, tc.want)
			}
		}
	}
}

func TestReportSummarisesSites(t *testing.T) {
	lat := int64(200)
	records := []store.ShadowRecord{
		{Site: Turn, Answers: `{"phase":{"type":"choice","choice":"implement","confidence":0.9}}`, Baseline: `{"routed_phase":"implement"}`, LatencyMS: &lat},
		{Site: Turn, Error: "HTTP 529"},
		{Site: Spawn, Answers: `{"difficulty":{"type":"score","score":1.5,"legend":{"0":"a","1":"b","2":"c","3":"d"}}}`, Baseline: `{"tier":"efficient"}`, Outcome: `{"status":"pass"}`, LatencyMS: &lat},
	}
	var buf bytes.Buffer
	WriteReport(&buf, records)
	out := buf.String()
	for _, want := range []string{"Shadow observations: 3", "phase_vs_routed", "100% (1/1)", "spawn, tier efficient, worker pass", "0.50 (n=1)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

func TestPhaseCheckSkipsTurnsWhosePhaseJevChose(t *testing.T) {
	r := store.ShadowRecord{Site: Turn, Answers: `{"phase":{"type":"choice","choice":"implement","confidence":0.9}}`, Baseline: `{"routed_phase":"implement","phase_source":"instruction:jev"}`}
	if checks := Checks(r); len(checks) != 0 {
		t.Fatalf("checks = %+v", checks)
	}
	r.Baseline = `{"routed_phase":"plan","phase_source":"instruction:default"}`
	if checks := Checks(r); len(checks) != 1 || checks[0].Agree {
		t.Fatalf("a defaulted instruction phase is still checked: %+v", checks)
	}
}
