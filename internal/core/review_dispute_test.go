package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/provider"
)

func TestReviewDispute(t *testing.T) {
	for _, mode := range []string{"overturned", "upheld", "contradictory", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			upheld := mode != "overturned"
			e, st := testEngine(t)
			workspace, git := gitRepo(t)
			writeFile(t, workspace, "Makefile", "test:\n\t@true\n")
			git("add", "-A")
			git("commit", "-qm", "base")
			mainTurns, reviews, adjudications := 0, 0, 0
			var implementer, reviewer model.Family
			s := &scriptedResponses{reply: func(_ int, body map[string]any) map[string]any {
				instructions := body["instructions"].(string)
				m, ok := model.Get("ramp/" + body["model"].(string))
				if !ok {
					t.Fatalf("unknown model %v", body["model"])
				}
				if strings.Contains(instructions, "Adjudicate disputed") {
					adjudications++
					if m.Family == implementer || m.Family == reviewer {
						t.Errorf("adjudicator %s must differ from implementer %s and reviewer %s", m.Family, implementer, reviewer)
					}
					for _, want := range []string{"main.go: wrong value", "I dispute", "package main"} {
						if !strings.Contains(instructions, want) {
							t.Errorf("adjudication missing %q", want)
						}
					}
					if mode == "unavailable" {
						return emptyReply()
					}
					if mode == "contradictory" {
						return verdictJSON(true, "main.go: contradictory finding")
					}
					if upheld {
						return verdictJSON(false, "main.go: upheld wrong value")
					}
					return verdictJSON(true)
				}
				if strings.Contains(instructions, "Review this proposed workspace diff") {
					reviews++
					reviewer = m.Family
					return verdictJSON(false, "main.go: wrong value")
				}
				implementer = m.Family
				mainTurns++
				switch mainTurns {
				case 1:
					return responsesCall("e1", "edit", map[string]any{"path": "main.go", "hunks": []any{map[string]any{"anchor": "e3b0c442", "delete": 0, "insert": []any{"package main"}}}})
				case 2:
					return responsesCall("x1", "exec", map[string]any{"command": "make test"})
				case 3:
					return responsesText("Implemented main.go; make test passed.")
				default:
					return responsesText("Implemented main.go; make test passed. I dispute the finding: it is a false positive.")
				}
			}}
			srv := s.serve(t)
			cfg := gateConfig(workspace, srv.URL)
			cfg.Providers = map[string]config.ProviderConfig{"ramp": {APIKey: "test", BaseURL: srv.URL}}
			cfg.Router.DisableSwitch = false
			cfg.Router.DefaultModel = "ramp/gpt-5.6-terra"
			e.ReplaceRuntime(cfg, provider.New(cfg), nil)
			req := agentproto.TaskRequest{Spec: "Write main.go", Budget: agentproto.Budget{MaxUSD: 20, MaxTokens: 10_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
			sid, results, err := e.Start(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			var result agentproto.TaskResult
			select {
			case result = <-results:
			case <-time.After(45 * time.Second):
				t.Fatal("dispute must terminate")
			}
			expectedCalls := 1
			if mode == "unavailable" {
				expectedCalls = 3
			} // Empty completions within the same worker.
			if jobs, _ := st.Jobs(context.Background(), sid); len(jobs) != 2 {
				t.Fatalf("one review and one adjudication job required, got %d", len(jobs))
			}
			if reviews != 1 || adjudications != expectedCalls {
				t.Fatalf("reviews=%d adjudications=%d result=%+v", reviews, adjudications, result)
			}
			if !result.Outcome.ReviewDisputed {
				t.Fatal("terminal outcome must record the dispute")
			}
			if upheld {
				expectedRejects := maxReviewRejections
				if mode == "unavailable" || mode == "contradictory" {
					expectedRejects++
				}
				if result.Status != agentproto.InputRequired || result.Outcome.CompletionRejects != expectedRejects {
					t.Fatalf("upheld and subsequent unchanged rejections must reach cap: %+v", result)
				}
			} else if result.Status != agentproto.Pass || !result.Outcome.IndependentlyReviewed {
				t.Fatalf("overturned finding must pass: %+v", result)
			}
			events, _ := st.EventsAfter(context.Background(), sid, 0)
			disputed, unchanged := false, 0
			for _, ev := range events {
				if ev.Type == "review.outcome" {
					var out map[string]any
					_ = json.Unmarshal(ev.Data, &out)
					if out["disputed"] == true {
						disputed = true
					}
				}
				if ev.Type == "completion.rejected" && strings.Contains(string(ev.Data), "adjudication inconclusive") {
					var rejected struct {
						Review string `json:"review"`
					}
					_ = json.Unmarshal(ev.Data, &rejected)
					if strings.Contains(rejected.Review, "contradictory") || !strings.Contains(rejected.Review, "implementer_family") {
						t.Fatalf("inconclusive adjudication changed original review: %s", rejected.Review)
					}
				}
				if ev.Type == "completion.rejected" && strings.Contains(string(ev.Data), "diff unchanged") {
					unchanged++
				}
			}
			if !disputed {
				t.Fatal("outcome must record dispute")
			}
			expectedUnchanged := maxReviewRejections - 2
			if mode == "unavailable" || mode == "contradictory" {
				expectedUnchanged++
			}
			if upheld && unchanged != expectedUnchanged {
				t.Fatalf("unchanged rejections=%d", unchanged)
			}
		})
	}
}

func TestAdjudicationDoesNotRelaxFamilyExclusions(t *testing.T) {
	h := newReviewHarness(t, false)
	h.write("a.go", "package a\n")
	session, _ := h.st.Session(context.Background(), h.sid)
	session.Model = "openai/gpt-5.6-terra"
	if err := h.st.UpdateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	h.reviewer = func(string, int) map[string]any { t.Fatal("excluded family must not adjudicate"); return nil }
	req := agentproto.TaskRequest{Depth: 1, Budget: agentproto.Budget{MaxUSD: 20, MaxTokens: 100000, MaxWallClock: time.Second}, Workspace: agentproto.Workspace{Path: h.workspace, Mode: "shared-write"}}
	original := `{"pass":false,"findings":["a.go: bug"],"families":["anthropic"],"implementer_family":"openai"}`
	passed, text, err := h.e.adjudicateReview(context.Background(), h.sid, "", req, original, "I dispute the finding", nil)
	if passed || text != original || err == nil {
		t.Fatalf("unavailable adjudication must preserve rejection: %v %s", passed, text)
	}
}

func TestDisputesReviewRequiresRebuttal(t *testing.T) {
	for _, answer := range []string{"dispute", "I disagree", "not a bug", "Implemented dispute resolution.", "I dispute the finding", "The review is complete; I disagree with the old API because it is slow."} {
		if disputesReview(answer) {
			t.Errorf("not a review rebuttal: %q", answer)
		}
	}
	for _, answer := range []string{"I dispute the finding: errors.Is walks the unwrap chain.", "The reviewer is wrong because the tests exercise this case.", "The finding is not a bug since the pointer is unchanged."} {
		if !disputesReview(answer) {
			t.Errorf("missed rebuttal: %q", answer)
		}
	}
}
