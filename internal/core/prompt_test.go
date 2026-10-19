package core

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrery/agentproto"
	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/model"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/router"
	"github.com/ductone/orrery/internal/store"
)

func basePrompt() promptContext {
	return promptContext{
		decision:  router.Decision{EditDialect: model.HashlineJSON},
		depth:     2,
		workspace: "/work/repo",
		tools:     []string{"read", "search", "edit", "exec", "todo", "spawn"},
	}
}

func TestSystemPromptSections(t *testing.T) {
	p := systemPrompt(basePrompt())
	if !strings.HasPrefix(p, CodingProfile().Identity) || !strings.Contains(p, harnessMarker) {
		t.Fatal("the prompt must open with the profile's identity and contain the harness marker")
	}
	for _, want := range []string{
		`workspace at "/work/repo"`,
		"\n\nSCOPE\n", "nothing more, nothing less", "Do not write files for it", "Uncommitted changes you did not make",
		"\n\nTOOLS\n", "not for cat, grep, find, ls, or sed -n", "Do not open guessed paths", "Prefer edit for hand-written source changes", "may write files through exec", "Never discard uncommitted changes", "or force-push", "Hashline editing is strict", "pass the line number",
		"\n\nWORKFLOW\n", "Never spend a turn only on the todo list", "Do not re-read your own diff", "only to satisfy a harness check", "make at most two broad discovery calls yourself", "then use targeted reads and searches",
		"\n\nDELIVERY\n", "Never present stubs", "Mark claims you did not verify as inference", "Orrery independently reviews every completed change", "do not spawn a worker to review your own diff",
		"\n\nWORKERS\n", "A worker sees only its spec", "No lower-cost worker model is configured", "Spawn workers only for independent slices that run in parallel with work you continue yourself", "Never delegate the core of the task and then wait on it",
		"Edit dialect: hashline-json. Remaining spawn depth: 2.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"lsp", "ROLE", "REQUIRED RESULT SCHEMA", "DEPLOYMENT INSTRUCTIONS", "Delegate further broad discovery"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("prompt must not contain %q without its trigger", unwanted)
		}
	}
	if p != systemPrompt(basePrompt()) {
		t.Fatal("the prompt must be deterministic, or every turn misses the cache")
	}
}

func TestSystemPromptConditionalSections(t *testing.T) {
	c := basePrompt()
	c.tools = []string{"read", "search", "edit", "exec", "todo"}
	if p := systemPrompt(c); strings.Contains(p, "WORKERS") || strings.Contains(p, "Delegate further broad discovery") {
		t.Fatal("without spawn there is no workers section or discovery delegation advice")
	}
	c = basePrompt()
	c.efficientWorker = true
	c.tools = append(c.tools, "lsp")
	p := systemPrompt(c)
	if strings.Contains(p, "Delegate further broad discovery") || strings.Contains(p, "No lower-cost worker") || !strings.Contains(p, "Use lsp for definitions") || !strings.Contains(p, "run in parallel with work you continue yourself") || !strings.Contains(p, "do not spawn a worker to review your own diff") {
		t.Fatal("efficient workers and lsp change their advice")
	}
	c = basePrompt()
	c.readOnlyWorker = true
	c.decision.EditDialect = model.TextAnchor
	c.resultSchema = map[string]any{"type": "object"}
	c.deployment = []string{"Use the staging database."}
	c.bootstrap = "\n\nWORKSPACE INSTRUCTIONS\nbe nice"
	p = systemPrompt(c)
	for _, want := range []string{"\n\nROLE\n- You are a bounded read-only worker", "Text-anchor editing is strict", "REQUIRED RESULT SCHEMA", `{"type":"object"}`, "Return the final result as the JSON object", "DEPLOYMENT INSTRUCTIONS\nUse the staging database.", "WORKSPACE INSTRUCTIONS\nbe nice"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Index(p, "DEPLOYMENT INSTRUCTIONS") > strings.Index(p, "WORKSPACE INSTRUCTIONS") {
		t.Fatal("deployment instructions precede workspace instructions, as before")
	}
}

// fullPrompt is a context that exercises every conditional part of the prompt.
func fullPrompt() promptContext {
	c := basePrompt()
	c.tools = append(c.tools, "lsp", "consult")
	c.efficientWorker = true
	c.readOnlyWorker = true
	c.decision.EditDialect = model.TextAnchor
	c.resultSchema = map[string]any{"type": "object"}
	c.deployment = []string{"Use the staging database."}
	c.bootstrap = "\n\nWORKSPACE INSTRUCTIONS\nbe nice"
	return c
}

func sortedLines(s string) []string {
	lines := strings.Split(s, "\n")
	slices.Sort(lines)
	return lines
}

// The golden files are the default prompt as it was before profiles existed.
// Harness and coding rules now merge by section, so rules within a section may
// come in a different order; no line may be added, dropped or reworded, and
// the sections keep their order.
func TestDefaultPromptKeepsPreProfileContent(t *testing.T) {
	for name, c := range map[string]promptContext{"base": basePrompt(), "full": fullPrompt()} {
		raw, err := os.ReadFile(filepath.Join("testdata", "prompt_coding_"+name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		got, want := systemPrompt(c), string(raw)
		if !slices.Equal(sortedLines(got), sortedLines(want)) {
			t.Errorf("%s: default prompt lines changed\n got: %q\nwant: %q", name, got, want)
		}
		heading := regexp.MustCompile(`(?m)^[A-Z][A-Z ]+$`)
		if g, w := heading.FindAllString(got, -1), heading.FindAllString(want, -1); !slices.Equal(g, w) {
			t.Errorf("%s: section order = %v, want %v", name, g, w)
		}
		if len(got) != len(want) {
			t.Errorf("%s: length %d, want %d", name, len(got), len(want))
		}
	}
	// With no profile given, the prompt is the coding profile's.
	c := fullPrompt()
	c.profile = CodingProfile()
	if systemPrompt(c) != systemPrompt(fullPrompt()) {
		t.Fatal("the zero profile must be the coding profile")
	}
}

func TestGeneralProfilePrompt(t *testing.T) {
	c := fullPrompt()
	c.profile = GeneralProfile()
	p := systemPrompt(c)
	if !strings.HasPrefix(p, "You are Orrery, an autonomous assistant working in the workspace at") {
		t.Fatalf("prompt opens %q", p[:80])
	}
	coding := regexp.MustCompile(`(?i)\b(repository|repositories|checkout|tests?|commit(ted)?|git|diff|lint(er)?|build|builds|refactors?)\b`)
	// The engine's own rule about reviewing the "diff" is harness text; the
	// profile must add no coding vocabulary of its own.
	for _, s := range GeneralProfile().Sections {
		for _, r := range s.Rules {
			if m := coding.FindString(r); m != "" {
				t.Errorf("general rule %q has coding vocabulary %q", r, m)
			}
		}
	}
	for _, want := range []string{
		harnessMarker,
		"Keep the todo plan as the truth", "Text-anchor editing is strict", "Use lsp for definitions", "Use $ORRERY_SCRATCH",
		"\n\nHARD DECISIONS\n", "\n\nWORKERS\n", "A worker sees only its spec", "\n\nROLE\n", "REQUIRED RESULT SCHEMA", "Return the final result as the JSON object",
		"DEPLOYMENT INSTRUCTIONS\nUse the staging database.", "WORKSPACE INSTRUCTIONS\nbe nice",
		"Edit dialect: text-anchor. Remaining spawn depth: 2.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("general prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"repository", "checkout", "Uncommitted", "git ", "verification that exercises"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("general prompt contains %q", unwanted)
		}
	}
	// Without spawn there is still no workers section.
	c.tools = []string{"read", "todo"}
	if p := systemPrompt(c); strings.Contains(p, "WORKERS") || !strings.Contains(p, "Keep the todo plan") {
		t.Fatal("tool-conditional harness sections follow the tools offered")
	}
}

func TestProfileSectionsMergeAndRequireTools(t *testing.T) {
	c := basePrompt()
	c.profile = Profile{Identity: "You are a gardener", Sections: []ProfileSection{
		{Title: "GARDEN", Rules: []string{"Water early."}},
		{Title: "TOOLS", Rules: []string{"Prefer shears."}},
		{Title: "BROWSING", Requires: "browser", Rules: []string{"Mind the cookies."}},
	}}
	p := systemPrompt(c)
	if !strings.HasPrefix(p, "You are a gardener working in the workspace at") || !strings.Contains(p, "\n\nGARDEN\n- Water early.") {
		t.Fatalf("custom identity or section missing: %q", p)
	}
	if i, j := strings.Index(p, "Do not open guessed paths"), strings.Index(p, "Prefer shears."); i < 0 || j < i {
		t.Fatal("a profile section adds its rules after the engine's in a section of the same title")
	}
	if strings.Contains(p, "BROWSING") {
		t.Fatal("a section requiring an absent tool must be left out")
	}
	for _, title := range []string{"WORKFLOW", "DELIVERY"} {
		if !strings.Contains(p, "\n\n"+title+"\n") {
			t.Errorf("engine section %s must appear even when the profile omits it", title)
		}
	}
	c.tools = append(c.tools, "browser")
	if !strings.Contains(systemPrompt(c), "Mind the cookies.") {
		t.Fatal("a section appears when its tool is offered")
	}
}

func TestEngineUsesTheProfile(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := New(config.Config{}, st, nil, nil, WithProfile(Profile{Identity: "You are a custom persistent agent"}))
	s := &scriptedResponses{reply: func(int, map[string]any) map[string]any { return responsesText("done") }}
	workspace := t.TempDir()
	cfg := gateConfig(workspace, s.serve(t).URL)
	e.ReplaceRuntime(cfg, provider.New(cfg), nil)
	_, results, err := e.Start(context.Background(), agentproto.TaskRequest{
		Spec:      "say done",
		Budget:    agentproto.Budget{MaxUSD: 5, MaxTokens: 1_000_000, MaxWallClock: time.Minute},
		Workspace: agentproto.Workspace{Path: workspace, Mode: "shared-write", Ownership: "external"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := <-results; r.Status != agentproto.Pass {
		t.Fatalf("result = %+v", r)
	}
	if len(s.requests) == 0 {
		t.Fatal("the main loop made no request")
	}
	instructions, _ := s.requests[0]["instructions"].(string)
	if !strings.HasPrefix(instructions, "You are a custom persistent agent working in the workspace at") || strings.Contains(instructions, "coding agent") || !strings.Contains(instructions, harnessMarker) {
		t.Fatalf("instructions = %q", instructions)
	}
}
