package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/provider"
)

func TestExecChangesRequireVerificationAndReview(t *testing.T) {
	for _, changed := range []bool{true, false} {
		name := "unchanged"
		if changed {
			name = "changed"
		}
		t.Run(name, func(t *testing.T) {
			e, st := testEngine(t)
			workspace, git := gitRepo(t)
			writeFile(t, workspace, "Makefile", "test:\n\t@true\n")
			git("add", "-A")
			git("commit", "-qm", "base")
			// Pre-existing work must not trigger review on the unchanged run.
			writeFile(t, workspace, "user.go", "package user\n")
			reviews, turns := 0, 0
			s := &scriptedResponses{reply: func(_ int, body map[string]any) map[string]any {
				if strings.Contains(body["instructions"].(string), "Review this proposed workspace diff") {
					reviews++
					return verdictJSON(true)
				}
				turns++
				if turns == 1 {
					command := "true"
					if changed {
						command = "printf 'package main\\n' > main.go"
					}
					return responsesCall("write", "exec", map[string]any{"command": command})
				}
				if changed && turns == 3 {
					return responsesCall("check", "exec", map[string]any{"command": "make test"})
				}
				return responsesText("Done.")
			}}
			srv := s.serve(t)
			cfg := gateConfig(workspace, srv.URL)
			e.ReplaceRuntime(cfg, provider.New(cfg), nil)
			req := agentproto.TaskRequest{Spec: "Write main.go", Budget: agentproto.Budget{MaxUSD: 20, MaxTokens: 10_000_000, MaxWallClock: time.Minute}, Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"}}
			sid, results, err := e.Start(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-results:
				if result.Status != agentproto.Pass {
					t.Fatalf("result = %+v", result)
				}
			case <-time.After(45 * time.Second):
				t.Fatal("the run must end")
			}
			want := 0
			if changed {
				want = 1
			}
			if reviews != want {
				t.Fatalf("reviews = %d, want %d", reviews, want)
			}
			scopes, rejections := 0, 0
			events, err := st.EventsAfter(context.Background(), sid, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range events {
				if ev.Type == "review.scope" {
					scopes++
				}
				if ev.Type == "completion.rejected" && strings.Contains(string(ev.Data), "workspace changed without verification") {
					rejections++
				}
			}
			if scopes != want || rejections != want {
				t.Fatalf("review scopes = %d, verification rejections = %d, want %d each", scopes, rejections, want)
			}
		})
	}
}

func TestWorkspaceChangesInvalidatePriorChecksAndReview(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()
	workspace, git := gitRepo(t)
	writeFile(t, workspace, "main.go", "package main\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	e.setBaseline("run", snapshotWorkspace(ctx, workspace))
	p := newProgressTracker()
	e.syncWorkspaceChanges(ctx, "run", workspace, p)
	writeFile(t, workspace, "main.go", "package main // first change\n")
	e.syncWorkspaceChanges(ctx, "run", workspace, p)
	p.verified, p.reviewed, p.formatVerified = true, true, true
	p.checksSinceEdit = []commandRecord{{Command: "check"}}
	e.syncWorkspaceChanges(ctx, "run", workspace, p)
	if !p.verified || !p.reviewed {
		t.Fatal("unchanged workspace must preserve checks and review")
	}
	writeFile(t, workspace, "main.go", "package main // second change\n")
	e.syncWorkspaceChanges(ctx, "run", workspace, p)
	if p.verified || p.reviewed || p.formatVerified || len(p.checksSinceEdit) != 0 {
		t.Fatal("new workspace contents must invalidate prior verification and review")
	}
	if !p.edited || !p.editedPaths["main.go"] {
		t.Fatal("workspace writes must record edited paths")
	}
}
