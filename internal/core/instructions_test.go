package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ductone/orrey/internal/provider"
)

func TestInstructionDiscoveryLoadsRootAndProgressesByPath(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "root rules")
	writeTestFile(t, filepath.Join(root, "pkg", "AGENTS.md"), "package rules")
	writeTestFile(t, filepath.Join(root, "pkg", "nested", "AGENTS.md"), "nested rules")

	d, err := newInstructionDiscovery(root, "fix the package")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Bootstrap(), "root rules") || strings.Contains(d.Bootstrap(), "package rules") {
		t.Fatalf("unexpected bootstrap: %s", d.Bootstrap())
	}
	docs := d.ForPath("pkg/nested/file.go")
	if len(docs) != 2 || docs[0].Path != "pkg/AGENTS.md" || docs[1].Path != "pkg/nested/AGENTS.md" {
		t.Fatalf("documents=%#v", docs)
	}
	if again := d.ForPath("pkg/nested/other.go"); len(again) != 0 {
		t.Fatalf("instructions loaded twice: %#v", again)
	}
}

func TestRootInstructionCompatibilityOrderEndsWithAgents(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".github", "copilot-instructions.md"), "copilot rules")
	writeTestFile(t, filepath.Join(root, "CLAUDE.md"), "claude rules")
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "agents rules")
	d, err := newInstructionDiscovery(root, "task")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := d.Bootstrap()
	copilot := strings.Index(bootstrap, "copilot rules")
	claude := strings.Index(bootstrap, "claude rules")
	agents := strings.Index(bootstrap, "agents rules")
	if copilot < 0 || !(copilot < claude && claude < agents) {
		t.Fatalf("unexpected compatibility instruction order: %s", bootstrap)
	}
}

func TestEditDiscoversRulesForNewExtensionlessFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "scripts", "AGENTS.md"), "script rules")
	d, err := newInstructionDiscovery(root, "add a script")
	if err != nil {
		t.Fatal(err)
	}
	docs := d.ForTool(provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "scripts/tool"}})
	if len(docs) != 1 || docs[0].Path != "scripts/AGENTS.md" {
		t.Fatalf("documents=%#v", docs)
	}
}

func TestInstructionBoundaryBlocksAllLaterEditsInResponse(t *testing.T) {
	edit := provider.ToolCall{Name: "edit", Arguments: map[string]any{"path": "pkg/file.go"}}
	if !shouldBlockEditForInstructions(edit, true) {
		t.Fatal("edit was allowed after instructions were disclosed in the same response")
	}
	if shouldBlockEditForInstructions(provider.ToolCall{Name: "read"}, true) || shouldBlockEditForInstructions(edit, false) {
		t.Fatal("instruction boundary blocked an unrelated call")
	}
}

func TestSkillsAreCatalogedThenLoadedProgressively(t *testing.T) {
	root := t.TempDir()
	skillPath := filepath.Join(root, ".agents", "skills", "release", "SKILL.md")
	writeTestFile(t, skillPath, "---\nname: release\ndescription: Prepare a safe release\n---\n# Release\nFULL SECRET SAUCE\n")
	d, err := newInstructionDiscovery(root, "prepare the project")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Bootstrap(), "Prepare a safe release") || strings.Contains(d.Bootstrap(), "FULL SECRET SAUCE") {
		t.Fatalf("skill was not progressively disclosed: %s", d.Bootstrap())
	}
	catalog := d.SkillCatalog()
	if len(catalog) != 1 || catalog[0]["loaded"] != false {
		t.Fatalf("catalog=%#v", catalog)
	}
	doc, loaded, err := d.LoadSkill("release")
	if err != nil || !loaded || !strings.Contains(doc.Content, "FULL SECRET SAUCE") {
		t.Fatalf("doc=%#v err=%v", doc, err)
	}
	if _, loaded, err = d.LoadSkill("release"); err != nil || loaded {
		t.Fatalf("skill loaded twice: loaded=%v err=%v", loaded, err)
	}
	if d.SkillCatalog()[0]["loaded"] != true {
		t.Fatal("loaded skill was not tracked")
	}
}

func TestExplicitSkillRequestMountsFullSkillInBootstrap(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "skills", "release", "SKILL.md"), "---\nname: release\ndescription: Ship safely\n---\nFULL RELEASE WORKFLOW\n")
	d, err := newInstructionDiscovery(root, "Use $release for this task")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Bootstrap(), "FULL RELEASE WORKFLOW") || d.SkillCatalog()[0]["loaded"] != true {
		t.Fatalf("explicit skill not mounted: %s", d.Bootstrap())
	}
}

func TestDuplicateSkillNameRequiresPath(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"one", "two"} {
		writeTestFile(t, filepath.Join(root, dir, "SKILL.md"), "---\nname: shared\ndescription: Shared workflow\n---\n"+dir+" body\n")
	}
	d, err := newInstructionDiscovery(root, "Use $shared")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Bootstrap(), "one body") || strings.Contains(d.Bootstrap(), "two body") {
		t.Fatal("ambiguous skill was mounted automatically")
	}
	if _, _, err = d.LoadSkill("shared"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous skill name error=%v", err)
	}
	doc, loaded, err := d.LoadSkill("one/SKILL.md")
	if err != nil || !loaded || !strings.Contains(doc.Content, "one body") {
		t.Fatalf("path load doc=%#v loaded=%v err=%v", doc, loaded, err)
	}
}

func TestInstructionDiscoveryIgnoresDependencyAndRuntimeSkills(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node_modules", "bad", "SKILL.md"), "bad")
	writeTestFile(t, filepath.Join(root, ".orrery", "bad", "SKILL.md"), "bad")
	d, err := newInstructionDiscovery(root, "task")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.SkillCatalog()) != 0 {
		t.Fatalf("ignored skills discovered: %#v", d.SkillCatalog())
	}
}

func TestNestedAgentsSymlinkCannotEscapeWorkspace(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(outside, "AGENTS.md"), "outside rules")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	d, err := newInstructionDiscovery(root, "task")
	if err != nil {
		t.Fatal(err)
	}
	docs := d.ForPath("linked/file.go")
	if len(docs) != 1 || docs[0].Kind != "instruction_error" || strings.Contains(docs[0].Content, "outside rules") {
		t.Fatalf("escaped instruction was not safely rejected: %#v", docs)
	}
	if again := d.ForPath("linked/other.go"); len(again) != 1 || again[0].Kind != "instruction_error" {
		t.Fatalf("invalid instruction boundary became bypassable: %#v", again)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
