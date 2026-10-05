package core

import (
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/router"
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
	if !strings.HasPrefix(p, systemPromptLead) {
		t.Fatal("the prompt must open with the lead")
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
