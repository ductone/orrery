package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ductone/orrey/internal/provider"
	"gopkg.in/yaml.v3"
)

const (
	maxInstructionBytes            = 256 << 10
	maxProgressiveInstructionBytes = 512 << 10
)

type workspaceInstruction struct {
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
	Content     string `json:"content"`
}

type workspaceSkill struct {
	Name, Description, Path, Content string
}

type instructionDiscovery struct {
	root         string
	bootstrap    string
	skills       []workspaceSkill
	loadedPaths  map[string]bool
	loadedSkills map[string]bool
	loadedBytes  int
	mu           sync.Mutex
}

func (e *Engine) instructionDiscovery(sessionID, root, spec string) (*instructionDiscovery, error) {
	e.mu.Lock()
	if existing := e.discovery[sessionID]; existing != nil {
		e.mu.Unlock()
		return existing, nil
	}
	e.mu.Unlock()
	discovery, err := newInstructionDiscovery(root, spec)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if existing := e.discovery[sessionID]; existing != nil {
		e.mu.Unlock()
		return existing, nil
	}
	e.discovery[sessionID] = discovery
	e.mu.Unlock()
	return discovery, nil
}

func newInstructionDiscovery(root, spec string) (*instructionDiscovery, error) {
	if root == "" {
		return &instructionDiscovery{loadedPaths: map[string]bool{}, loadedSkills: map[string]bool{}}, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	d := &instructionDiscovery{root: filepath.Clean(abs), loadedPaths: map[string]bool{}, loadedSkills: map[string]bool{}}
	rootDocs, err := d.rootInstructions()
	if err != nil {
		return nil, err
	}
	d.skills, err = discoverWorkspaceSkills(d.root)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if len(rootDocs) > 0 {
		b.WriteString("\n\nWORKSPACE INSTRUCTIONS\nThese files were snapshotted at session start. Follow them for all workspace work; later, deeper AGENTS.md files override broader ones.\n")
		mounted := 0
		for _, doc := range rootDocs {
			mounted += len(doc.Content)
			if mounted > maxProgressiveInstructionBytes {
				return nil, fmt.Errorf("root workspace instructions exceed %d bytes", maxProgressiveInstructionBytes)
			}
			d.loadedPaths[doc.Path] = true
			writeInstruction(&b, doc)
		}
	}
	if len(d.skills) > 0 {
		b.WriteString("\n\nAVAILABLE WORKSPACE SKILLS\nOnly summaries are mounted initially. When the task names a skill or a summary clearly applies, call the skill tool to load its full SKILL.md before acting.\n")
		for _, skill := range d.skills {
			fmt.Fprintf(&b, "- %s (%s): %s\n", skill.Name, skill.Path, truncateInstructionSummary(skill.Description))
		}
		nameCount := map[string]int{}
		for _, skill := range d.skills {
			nameCount[strings.ToLower(skill.Name)]++
		}
		for _, skill := range d.skills {
			if nameCount[strings.ToLower(skill.Name)] == 1 && explicitlyRequestsSkill(spec, skill.Name) {
				d.loadedSkills[skill.Path] = true
				writeInstruction(&b, workspaceInstruction{Kind: "skill", Name: skill.Name, Description: skill.Description, Path: skill.Path, Content: skill.Content})
			}
		}
	}
	d.bootstrap = b.String()
	return d, nil
}

func (d *instructionDiscovery) Bootstrap() string {
	if d == nil {
		return ""
	}
	return d.bootstrap
}

func (d *instructionDiscovery) SkillCatalog() []map[string]any {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]map[string]any, 0, len(d.skills))
	for _, skill := range d.skills {
		out = append(out, map[string]any{"name": skill.Name, "description": truncateInstructionSummary(skill.Description), "path": skill.Path, "loaded": d.loadedSkills[skill.Path]})
	}
	return out
}

func (d *instructionDiscovery) LoadSkill(query string) (workspaceInstruction, bool, error) {
	if d == nil {
		return workspaceInstruction{}, false, errors.New("workspace skill discovery unavailable")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return workspaceInstruction{}, false, errors.New("skill name or path required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var matches []workspaceSkill
	for _, skill := range d.skills {
		if strings.EqualFold(query, skill.Name) || filepath.ToSlash(query) == skill.Path {
			matches = append(matches, skill)
		}
	}
	if len(matches) == 0 {
		return workspaceInstruction{}, false, fmt.Errorf("unknown workspace skill %q", query)
	}
	if len(matches) > 1 {
		paths := make([]string, 0, len(matches))
		for _, skill := range matches {
			paths = append(paths, skill.Path)
		}
		return workspaceInstruction{}, false, fmt.Errorf("ambiguous skill %q; use one of these paths: %s", query, strings.Join(paths, ", "))
	}
	skill := matches[0]
	if d.loadedSkills[skill.Path] {
		return workspaceInstruction{Kind: "skill", Name: skill.Name, Description: skill.Description, Path: skill.Path}, false, nil
	}
	d.loadedSkills[skill.Path] = true
	return workspaceInstruction{Kind: "skill", Name: skill.Name, Description: skill.Description, Path: skill.Path, Content: skill.Content}, true, nil
}

func (d *instructionDiscovery) ForPath(raw string) []workspaceInstruction {
	return d.forPath(raw, false)
}

func (d *instructionDiscovery) ForTool(call provider.ToolCall) []workspaceInstruction {
	switch call.Name {
	case "read":
		return d.forPath(fmt.Sprint(call.Arguments["path"]), false)
	case "edit":
		return d.forPath(fmt.Sprint(call.Arguments["path"]), true)
	default:
		return nil
	}
}

func (d *instructionDiscovery) forPath(raw string, forceFile bool) []workspaceInstruction {
	if d == nil || d.root == "" || raw == "" || strings.Contains(raw, "://") {
		return nil
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(d.root, raw)
	}
	clean := filepath.Clean(raw)
	rel, err := filepath.Rel(d.root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	dir := clean
	if forceFile {
		dir = filepath.Dir(clean)
	} else if info, statErr := os.Stat(clean); statErr == nil && !info.IsDir() {
		dir = filepath.Dir(clean)
	} else if statErr != nil && filepath.Ext(clean) != "" {
		dir = filepath.Dir(clean)
	}
	var dirs []string
	for current := dir; ; current = filepath.Dir(current) {
		dirs = append(dirs, current)
		if current == d.root {
			break
		}
		if parent := filepath.Dir(current); parent == current {
			return nil
		}
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []workspaceInstruction
	for _, candidateDir := range dirs {
		candidate := filepath.Join(candidateDir, "AGENTS.md")
		relPath, _ := filepath.Rel(d.root, candidate)
		relPath = filepath.ToSlash(relPath)
		if d.loadedPaths[relPath] {
			continue
		}
		content, readErr := readWorkspaceInstruction(d.root, candidate)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			out = append(out, workspaceInstruction{Kind: "instruction_error", Path: relPath, Content: readErr.Error()})
			continue
		}
		if d.loadedBytes+len(content) > maxProgressiveInstructionBytes {
			break
		}
		d.loadedBytes += len(content)
		d.loadedPaths[relPath] = true
		out = append(out, workspaceInstruction{Kind: "agents", Path: relPath, Content: content})
	}
	return out
}

func (d *instructionDiscovery) ForSearchResults(value any) []workspaceInstruction {
	rows, ok := value.([]map[string]any)
	if !ok {
		return nil
	}
	var out []workspaceInstruction
	seen := map[string]bool{}
	for i, row := range rows {
		if i >= 32 {
			break
		}
		for _, doc := range d.ForPath(fmt.Sprint(row["path"])) {
			if !seen[doc.Path] {
				seen[doc.Path] = true
				out = append(out, doc)
			}
		}
	}
	return out
}

func (d *instructionDiscovery) rootInstructions() ([]workspaceInstruction, error) {
	paths := []string{filepath.ToSlash(filepath.Join(".github", "copilot-instructions.md")), "CLAUDE.md", "AGENTS.md"}
	var out []workspaceInstruction
	for _, rel := range paths {
		content, err := readWorkspaceInstruction(d.root, filepath.Join(d.root, filepath.FromSlash(rel)))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, workspaceInstruction{Kind: "workspace", Path: rel, Content: content})
	}
	return out, nil
}

func discoverWorkspaceSkills(root string) ([]workspaceSkill, error) {
	var out []workspaceSkill
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if path != root && ignoredInstructionDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "SKILL.md" || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || len(out) >= 64 {
			return nil
		}
		content, err := readInstructionFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		name, description := skillMetadata(content, filepath.Base(filepath.Dir(path)))
		out = append(out, workspaceSkill{Name: name, Description: description, Path: filepath.ToSlash(rel), Content: content})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if strings.EqualFold(out[i].Name, out[j].Name) {
			return out[i].Path < out[j].Path
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

func skillMetadata(content, fallback string) (string, string) {
	meta := struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}{}
	body := content
	if strings.HasPrefix(content, "---\n") {
		rest := content[4:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			_ = yaml.Unmarshal([]byte(rest[:end]), &meta)
			body = strings.TrimPrefix(rest[end+4:], "\n")
		}
	}
	if strings.TrimSpace(meta.Name) == "" {
		meta.Name = fallback
	}
	if strings.TrimSpace(meta.Description) == "" {
		meta.Description = firstDescription(body)
	}
	if meta.Description == "" {
		meta.Description = "Workspace-provided skill"
	}
	return strings.TrimSpace(meta.Name), strings.TrimSpace(meta.Description)
}

func firstDescription(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return trimmed
		}
	}
	return ""
}

func explicitlyRequestsSkill(spec, name string) bool {
	spec, name = strings.ToLower(spec), strings.ToLower(strings.TrimSpace(name))
	return name != "" && (strings.Contains(spec, "$"+name) || strings.Contains(spec, "skill:"+name) || strings.Contains(spec, "use the "+name+" skill") || strings.Contains(spec, "use "+name+" skill"))
}

func readInstructionFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("instruction file %s is not a regular file", path)
	}
	if info.Size() > maxInstructionBytes {
		return "", fmt.Errorf("instruction file %s exceeds %d bytes", path, maxInstructionBytes)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("instruction file %s is not UTF-8", path)
	}
	return string(content), nil
}

func readWorkspaceInstruction(root, path string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("instruction file %s escapes workspace", path)
	}
	return readInstructionFile(resolvedPath)
}

func truncateInstructionSummary(summary string) string {
	summary = strings.Join(strings.Fields(summary), " ")
	const max = 500
	if len(summary) <= max {
		return summary
	}
	return summary[:max] + "…"
}

func ignoredInstructionDir(name string) bool {
	switch name {
	case ".git", ".orrery", ".task-worktrees", "node_modules", "vendor", "local_vendor", "bazel-bin", "bazel-out", "bazel-testlogs", ".cache":
		return true
	default:
		return false
	}
}

func writeInstruction(b *strings.Builder, doc workspaceInstruction) {
	fmt.Fprintf(b, "\n--- %s: %s ---\n%s\n--- end %s ---\n", doc.Kind, doc.Path, doc.Content, doc.Path)
}

func instructionPayload(docs []workspaceInstruction) []map[string]any {
	out := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		out = append(out, map[string]any{"kind": doc.Kind, "name": doc.Name, "description": doc.Description, "path": doc.Path, "content": doc.Content})
	}
	return out
}
