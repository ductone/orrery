package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ductone/orrey/internal/hashline"
	"github.com/ductone/orrey/internal/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Handler func(context.Context, map[string]any) (any, error)

var fileLocks [256]sync.Mutex

func fileLock(path string) *sync.Mutex {
	sum := sha256.Sum256([]byte(path))
	return &fileLocks[sum[0]]
}

type Registry struct {
	root     string
	defs     []provider.Tool
	handlers map[string]Handler
	schemes  map[string]Handler
	mu       sync.Mutex
	jobs     map[string]*commandJob
	state    *SessionState
	dialect  anchorDialect
}

// SessionState holds edit recovery state across model turns. Engine tool
// registries are intentionally rebuilt each turn, so this state must be owned
// by the session rather than by an individual Registry.
type SessionState struct {
	mu             sync.Mutex
	anchorFailures map[string]int
	noop           noopState
	snapshots      map[string]fileSnapshot
}

type noopState struct {
	signature string
	streak    int
}

type fileSnapshot struct {
	version string
	content []string
}

type anchorDialect string

const (
	anchorHashline   anchorDialect = "hashline-json"
	anchorContextual anchorDialect = "hashline-contextual"
	anchorText       anchorDialect = "text-anchor"
)

func (d anchorDialect) mode() hashline.AnchorMode {
	if d == anchorContextual {
		return hashline.AnchorContextual
	}
	if d == anchorText {
		return hashline.AnchorText
	}
	return hashline.AnchorLine
}

func fileVersion(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16], nil
}

func lineText(lines []hashline.Line) []string {
	text := make([]string, len(lines))
	for i := range lines {
		text[i] = lines[i].Text
	}
	return text
}

func editDescription(d anchorDialect) string {
	if d == anchorContextual {
		return "Apply contextual hashline hunks. Anchors are 8-character hashes of the previous, current, and next visible lines from the latest read; changing either neighbor makes an anchor stale. For a new file use e3b0c442 with delete=0."
	}
	if d == anchorText {
		return "Apply exact-text anchor hunks. Copy the complete line text from the latest read into anchor; repeated identical lines are ambiguous. For a new file use an empty anchor with delete=0."
	}
	return "Apply content-anchored hashline hunks. You MUST read the exact target window immediately before editing and copy its latest 8-character hash into anchor. To create a new file, use anchor e3b0c442 with delete=0. Re-read after compaction or a stale error. Structural declaration deletion is rejected unless explicitly allowed."
}

func readDescription(d anchorDialect) string {
	if d == anchorContextual {
		return "Read a file or directory. File anchors hash the previous, current, and next visible lines; a changed neighbor invalidates an anchor."
	}
	if d == anchorText {
		return "Read a file or directory. The hash field contains the exact line text to copy into edit.anchor; repeated identical lines may be ambiguous."
	}
	return "Read a file or directory. Files include hashline anchors. around_line returns a small 1-based window around a requested line."
}

type commandJob struct {
	cmd  *exec.Cmd
	path string
	done chan error
}

func New(root string) *Registry {
	return NewWithStateDialect(root, nil, string(anchorHashline))
}

func NewWithState(root string, state *SessionState) *Registry {
	return NewWithStateDialect(root, state, string(anchorHashline))
}

func NewWithStateDialect(root string, state *SessionState, dialect string) *Registry {
	r := NewReadOnlyWithStateDialect(root, state, dialect)
	anchor := map[string]any{"type": "string", "pattern": "^[0-9a-f]{8}$", "description": "Exact 8-character hash copied from the latest read result. Never use line text, a line number, or a placeholder."}
	if r.dialect == anchorText {
		anchor = map[string]any{"type": "string", "description": "Complete exact line text copied from the latest read result. Never use a line number or placeholder."}
	}
	r.add("edit", editDescription(r.dialect), schema(map[string]any{"path": str(), "hunks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"anchor": anchor, "offset": num(), "delete": num(), "insert": map[string]any{"type": "array", "items": str()}, "allow_structural_change": boolean()}, "required": []string{"anchor", "delete", "insert"}, "additionalProperties": false}}}, "path", "hunks"), r.edit)
	r.add("exec", "Run a shell command in the workspace. Use background=true for long jobs.", schema(map[string]any{"command": str(), "background": boolean(), "timeout_seconds": num()}, "command"), r.run)
	r.add("job", "Wait for, cancel, or read logs from a background exec job.", schema(map[string]any{"id": str(), "action": map[string]any{"type": "string", "enum": []string{"wait", "cancel", "logs"}}}, "id", "action"), r.job)
	return r
}

func NewReadOnly(root string) *Registry {
	return NewReadOnlyWithStateDialect(root, nil, string(anchorHashline))
}

func NewReadOnlyWithState(root string, state *SessionState) *Registry {
	return NewReadOnlyWithStateDialect(root, state, string(anchorHashline))
}

func NewReadOnlyWithStateDialect(root string, state *SessionState, dialect string) *Registry {
	if state == nil {
		state = &SessionState{}
	}
	state.mu.Lock()
	if state.anchorFailures == nil {
		state.anchorFailures = map[string]int{}
	}
	if state.snapshots == nil {
		state.snapshots = map[string]fileSnapshot{}
	}
	state.mu.Unlock()
	r := &Registry{root: root, handlers: map[string]Handler{}, schemes: map[string]Handler{}, jobs: map[string]*commandJob{}, state: state, dialect: anchorDialect(dialect)}
	r.add("read", readDescription(r.dialect), schema(map[string]any{"path": str(), "start": num(), "limit": num(), "around_line": num()}, "path"), r.read)
	r.add("search", "Regex search file contents with optional glob.", schema(map[string]any{"pattern": str(), "glob": str(), "max_results": num()}, "pattern"), r.search)
	return r
}
func (r *Registry) add(n, d string, s map[string]any, h Handler) {
	r.defs = append(r.defs, provider.Tool{Name: n, Description: d, InputSchema: s})
	r.handlers[n] = h
}
func (r *Registry) Add(n, d string, s map[string]any, h Handler) { r.add(n, d, s, h) }
func (r *Registry) AddScheme(name string, h Handler)             { r.schemes[name] = h }

// AddFileScheme exposes an exact, read-only set of supervisor-provided files
// without widening the workspace boundary. Keys become <name>://<key>.
func (r *Registry) AddFileScheme(name string, files map[string]string) {
	allowed := make(map[string]string, len(files))
	for key, value := range files {
		allowed[key] = value
	}
	r.AddScheme(name, func(_ context.Context, args map[string]any) (any, error) {
		key := asString(args["path"])
		path, ok := allowed[key]
		if !ok || key == "" {
			return nil, errors.New("unknown attachment")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("attachment is not a regular file")
		}
		if info.Size() > 25<<20 {
			return nil, errors.New("attachment exceeds 25 MiB")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if utf8.Valid(data) {
			return map[string]any{"text": string(data), "bytes": len(data)}, nil
		}
		if len(data) > 10<<20 {
			return map[string]any{"binary": true, "bytes": len(data), "error": "binary attachment exceeds inline limit"}, nil
		}
		return map[string]any{"binary": true, "bytes": len(data), "base64": base64.StdEncoding.EncodeToString(data)}, nil
	})
}
func (r *Registry) Definitions() []provider.Tool { return append([]provider.Tool(nil), r.defs...) }
func (r *Registry) DefinitionsExcept(names ...string) []provider.Tool {
	excluded := map[string]bool{}
	for _, name := range names {
		excluded[name] = true
	}
	out := []provider.Tool{}
	for _, definition := range r.defs {
		if !excluded[definition.Name] {
			out = append(out, definition)
		}
	}
	return out
}
func (r *Registry) DefinitionsOnly(names ...string) []provider.Tool {
	included := map[string]bool{}
	for _, name := range names {
		included[name] = true
	}
	out := []provider.Tool{}
	for _, definition := range r.defs {
		if included[definition.Name] {
			out = append(out, definition)
		}
	}
	return out
}
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	ctx, span := otel.Tracer("orrery/tools").Start(ctx, "tool.call")
	defer span.End()
	span.SetAttributes(attribute.String("tool", name))
	h, ok := r.handlers[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return h(ctx, args)
}
func (r *Registry) safe(path string) (string, error) {
	if strings.Contains(path, "\x00") {
		return "", errors.New("invalid path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.root, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(r.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("path escapes workspace")
	}
	return path, nil
}
func (r *Registry) read(ctx context.Context, a map[string]any) (any, error) {
	rawPath := asString(a["path"])
	if parts := strings.SplitN(rawPath, "://", 2); len(parts) == 2 {
		if h := r.schemes[parts[0]]; h != nil {
			return h(ctx, map[string]any{"path": parts[1]})
		}
		return nil, fmt.Errorf("unknown internal scheme %q", parts[0])
	}
	p, err := r.safe(rawPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, e := range ents {
			out = append(out, map[string]any{"name": e.Name(), "dir": e.IsDir()})
		}
		return out, nil
	}
	lock := fileLock(p)
	lock.Lock()
	defer lock.Unlock()
	lines, err := hashline.ReadWithMode(p, r.dialect.mode())
	if err != nil {
		return nil, err
	}
	currentVersion, err := fileVersion(p)
	if err != nil {
		return nil, err
	}
	r.state.mu.Lock()
	r.state.snapshots[p] = fileSnapshot{version: currentVersion, content: lineText(lines)}
	r.state.mu.Unlock()
	if around, ok := a["around_line"]; ok && around != nil {
		if len(lines) == 0 {
			return lines, nil
		}
		line := min(len(lines), max(1, asInt(around, 1)))
		window := asInt(a["limit"], 9)
		window = min(200, max(1, window))
		lo := max(0, line-1-window/2)
		hi := min(len(lines), lo+window)
		lo = max(0, hi-window)
		return lines[lo:hi], nil
	}
	start := max(1, asInt(a["start"], 1))
	limit := asInt(a["limit"], 400)
	if len(lines) > 2000 && a["start"] == nil {
		outline := []hashline.Line{}
		for _, l := range lines {
			t := strings.TrimSpace(l.Text)
			if strings.HasPrefix(t, "func ") || strings.HasPrefix(t, "type ") || strings.HasPrefix(t, "class ") || strings.HasPrefix(t, "interface ") || strings.HasPrefix(t, "package ") || strings.HasPrefix(t, "#") {
				outline = append(outline, l)
			}
		}
		return map[string]any{"summarized": true, "line_count": len(lines), "outline": outline, "hint": "request a start/limit window or an around_line window"}, nil
	}
	lo := min(len(lines), start-1)
	hi := min(len(lines), lo+limit)
	return lines[lo:hi], nil
}
func (r *Registry) search(ctx context.Context, a map[string]any) (any, error) {
	re, err := regexp.Compile(asString(a["pattern"]))
	if err != nil {
		return nil, err
	}
	glob := asString(a["glob"])
	maxResults := asInt(a["max_results"], 200)
	out := []map[string]any{}
	err = filepath.WalkDir(r.root, func(p string, d fs.DirEntry, e error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e != nil {
			return nil
		}
		if d.IsDir() {
			if ignoredSearchDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(r.root, p)
		if glob != "" {
			ok := globMatch(glob, filepath.ToSlash(rel))
			if !ok {
				return nil
			}
		}
		b, e := os.ReadFile(p)
		if e != nil || len(b) > 4<<20 {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				out = append(out, map[string]any{"path": rel, "line": i + 1, "text": line})
				if len(out) >= maxResults {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	return out, err
}

func ignoredSearchDir(name string) bool {
	switch name {
	case ".git", ".orrery", ".task-worktrees", "node_modules", "vendor", "local_vendor", "bazel-bin", "bazel-out", "bazel-testlogs", ".cache":
		return true
	default:
		return false
	}
}

func globMatch(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	if open := strings.IndexByte(pattern, '{'); open >= 0 {
		if close := strings.IndexByte(pattern[open+1:], '}'); close >= 0 {
			close += open + 1
			for _, alternative := range strings.Split(pattern[open+1:close], ",") {
				if globMatch(pattern[:open]+alternative+pattern[close+1:], name) {
					return true
				}
			}
			return false
		}
	}
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(name))
		return ok
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(name)
}
func (r *Registry) edit(_ context.Context, a map[string]any) (any, error) {
	b, _ := json.Marshal(a)
	var p hashline.Patch
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	safe, err := r.safe(p.Path)
	if err != nil {
		return nil, err
	}
	p.Path = safe
	signature := string(b)
	lock := fileLock(p.Path)
	lock.Lock()
	defer lock.Unlock()
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if r.state.noop.signature != signature {
		r.state.noop = noopState{}
	}
	current, readErr := hashline.ReadWithMode(p.Path, r.dialect.mode())
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if readErr == nil {
		currentVersion, err := fileVersion(p.Path)
		if err != nil {
			return nil, err
		}
		snapshot, ok := r.state.snapshots[p.Path]
		if !ok {
			details := map[string]any{
				"error":           "E_FILE_NOT_READ: file must be read before editing",
				"current_version": currentVersion,
				"directive":       "read the exact target window, then retry with a returned anchor",
			}
			return details, errors.New("E_FILE_NOT_READ: file must be read before editing")
		}
		if currentVersion != snapshot.version {
			details := map[string]any{
				"error":            "E_FILE_CHANGED: file changed since this session last read it",
				"expected_version": snapshot.version,
				"current_version":  currentVersion,
				"diff_since_read":  diffSince(snapshot.content, lineText(current)),
				"directive":        "re-read the target region and retry using fresh anchors",
			}
			return details, errors.New("E_FILE_CHANGED: file changed since this session last read it")
		}
	}
	result, err := hashline.ApplyWithMode(p, r.dialect.mode())
	if errors.Is(err, hashline.ErrNoChanges) {
		r.state.noop.signature, r.state.noop.streak = signature, r.state.noop.streak+1
		if r.state.noop.streak >= 3 {
			return nil, errors.New("E_NOOP_LOOP: three consecutive identical no-op edits")
		}
		return nil, err
	}
	// Any result other than the same no-op breaks the consecutive no-op streak,
	// even when the file changed externally and the same patch is now stale.
	r.state.noop = noopState{}
	if err != nil {
		var stale *hashline.StaleError
		if errors.As(err, &stale) {
			key := p.Path + "|" + stale.Anchor
			r.state.anchorFailures[key]++
			if r.state.anchorFailures[key] > 1 {
				lines, _ := hashline.ReadWithMode(p.Path, r.dialect.mode())
				occurrences := []int{}
				for _, line := range lines {
					if r.anchorMatches(line.Hash, stale.Anchor) {
						occurrences = append(occurrences, line.Number)
					}
				}
				region := stale.Fresh
				if len(occurrences) > 0 {
					lo := max(0, occurrences[0]-11)
					hi := min(len(lines), occurrences[0]+10)
					region = lines[lo:hi]
				} else if len(lines) > 0 {
					region = lines[:min(len(lines), 20)]
				}
				return map[string]any{"error": err.Error(), "region": region, "occurrence_lines": occurrences, "directive": "choose a fresh unique anchor from this region before retrying"}, fmt.Errorf("%w; occurrence line numbers: %v; choose a fresh unique anchor from the larger region before retrying", err, occurrences)
			}
			fresh, _ := json.Marshal(stale.Fresh)
			return map[string]any{"error": err.Error(), "fresh_anchors": stale.Fresh}, fmt.Errorf("%w; fresh anchors near the lookup point: %s; call read on the exact target window before retrying", err, fresh)
		}
		return nil, err
	}
	r.state.anchorFailures = map[string]int{}
	currentVersion, err := fileVersion(p.Path)
	if err != nil {
		return nil, err
	}
	r.state.snapshots[p.Path] = fileSnapshot{version: currentVersion, content: lineText(result.Lines)}
	windows := computeFreshWindows(result.Lines, result.Affected)
	return map[string]any{"applied": len(p.Hunks), "fresh_anchors": windows}, nil
}

func (r *Registry) anchorMatches(candidate, anchor string) bool {
	if r.dialect == anchorText {
		return candidate == anchor
	}
	return strings.HasPrefix(candidate, anchor)
}

func diffSince(before, after []string) map[string]any {
	changes := make([]map[string]any, 0, 20)
	lineCount := max(len(before), len(after))
	truncated := false
	for i := 0; i < lineCount; i++ {
		oldText, newText := "", ""
		if i < len(before) {
			oldText = before[i]
		}
		if i < len(after) {
			newText = after[i]
		}
		if oldText == newText {
			continue
		}
		if len(changes) == cap(changes) {
			truncated = true
			break
		}
		changes = append(changes, map[string]any{"line": i + 1, "before": oldText, "after": newText})
	}
	return map[string]any{"changes": changes, "truncated": truncated}
}

func computeFreshWindows(lines []hashline.Line, affected []hashline.AffectedRegion) [][]hashline.Line {
	if len(affected) == 0 {
		return nil
	}
	var out [][]hashline.Line
	for _, r := range affected {
		lo := max(0, r.Start-2)
		hi := min(len(lines), r.End+2)
		out = append(out, lines[lo:hi])
	}
	return out
}
func (r *Registry) run(ctx context.Context, a map[string]any) (any, error) {
	cmdText := asString(a["command"])
	if cmdText == "" {
		return nil, errors.New("command required")
	}
	if execMutatesSource(cmdText) {
		return nil, errors.New("exec source mutation rejected; use the edit tool so changes are anchored, reviewable, and measured")
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", cmdText)
	cmd.Dir = r.root
	logDir := filepath.Join(r.root, ".orrery", "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("cmd-%d", time.Now().UnixNano())
	path := filepath.Join(logDir, id+".log")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err = cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	j := &commandJob{cmd: cmd, path: path, done: make(chan error, 1)}
	r.mu.Lock()
	r.jobs[id] = j
	r.mu.Unlock()
	go func() { err := cmd.Wait(); f.Close(); j.done <- err; close(j.done) }()
	if bg, _ := a["background"].(bool); bg {
		return map[string]any{"id": id, "log": path}, nil
	}
	timeout := time.Duration(asInt(a["timeout_seconds"], 120)) * time.Second
	select {
	case err := <-j.done:
		return commandSummary(path, err)
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("command timed out; log: %s", path)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func execMutatesSource(command string) bool {
	s := strings.ToLower(command)
	patterns := []string{
		".write_text(", "os.writefile(", "ioutil.writefile(", "os.create(",
		"sed -i", "sed --in-place", "perl -pi", "gofmt -w", "go fmt ",
	}
	for _, pattern := range patterns {
		if strings.Contains(s, pattern) {
			return true
		}
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(^|[;&|]\s*)touch\s+`),
		regexp.MustCompile(`(^|[;&|]\s*)(cat|printf|echo)\b[^\n]*>{1,2}\s*[^&]`),
		regexp.MustCompile(`(^|[;&|]\s*)tee\s+`),
	} {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
func (r *Registry) job(ctx context.Context, a map[string]any) (any, error) {
	id := asString(a["id"])
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j == nil {
		return nil, errors.New("job not found")
	}
	switch asString(a["action"]) {
	case "logs":
		return commandSummary(j.path, nil)
	case "cancel":
		return map[string]any{"cancelled": j.cmd.Process.Kill() == nil}, nil
	case "wait":
		select {
		case err := <-j.done:
			return commandSummary(j.path, err)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		return nil, errors.New("invalid action")
	}
}
func commandSummary(path string, runErr error) (any, error) {
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	summary := lines
	if len(lines) > 20 {
		summary = append(append(append([]string{}, lines[:10]...), fmt.Sprintf("... %d lines omitted; full output: %s ...", len(lines)-20, path)), lines[len(lines)-10:]...)
	}
	out := map[string]any{"ok": runErr == nil, "summary": strings.Join(summary, "\n"), "log": path}
	if runErr != nil {
		out["error"] = runErr.Error()
		return out, fmt.Errorf("command failed: %v; summary: %s; log: %s", runErr, strings.Join(summary, "\n"), path)
	}
	return out, nil
}
func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func str() map[string]any     { return map[string]any{"type": "string"} }
func num() map[string]any     { return map[string]any{"type": "integer"} }
func boolean() map[string]any { return map[string]any{"type": "boolean"} }
func asString(v any) string   { s, _ := v.(string); return s }
func asInt(v any, d int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return d
}
func SortDefinitions(xs []provider.Tool) {
	sort.Slice(xs, func(i, j int) bool { return xs[i].Name < xs[j].Name })
}
