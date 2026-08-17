package eval

import (
	"encoding/json"
	"testing"
)

func TestRunSyntheticEditBenchmarkQualitative(t *testing.T) {
	got := RunSyntheticEditBenchmark()
	if len(got.Dialects) != 3 {
		t.Fatalf("dialects = %d", len(got.Dialects))
	}

	for _, name := range []string{legacyDialect, contextualDialect, textDialect} {
		m, ok := got.Dialects[name]
		if !ok {
			t.Fatalf("missing dialect %q", name)
		}
		if m.CaseCount != 7 || m.FirstAttemptOpportunities != 2 {
			t.Errorf("%s case accounting = %+v", name, m)
		}
		if m.ToolCallProxyCount == 0 || m.EscalationsDetected != 1 || m.Retries != 1 {
			t.Errorf("%s did not exercise retry ladder: %+v", name, m)
		}
		if m.NoopLoopsDetected != 1 || m.ConflictsDetected != 1 || !m.LargeFileRecovery {
			t.Errorf("%s recovery metrics = %+v", name, m)
		}
		if m.FirstAttemptRate < 0 || m.FirstAttemptRate > 1 {
			t.Errorf("%s rate = %v", name, m.FirstAttemptRate)
		}
	}

	contextual := got.Dialects[contextualDialect]
	if contextual.FirstAttemptSuccesses != 2 || contextual.ContextStalenessDetected != 1 || contextual.AmbiguitiesDetected != 0 {
		t.Errorf("contextual behavior = %+v", contextual)
	}
	for _, name := range []string{legacyDialect, textDialect} {
		m := got.Dialects[name]
		if m.FirstAttemptSuccesses != 1 || m.AmbiguitiesDetected != 1 || m.ContextStalenessDetected != 0 {
			t.Errorf("%s behavior = %+v", name, m)
		}
	}

	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSyntheticEditDialects(b *testing.B) {
	for b.Loop() {
		_ = RunSyntheticEditBenchmark()
	}
}
