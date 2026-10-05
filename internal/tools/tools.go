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
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
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
	state    *SessionState
	dialect  anchorDialect
	ranker   SearchRanker
	// jobFallback serves job ids this registry did not start, such as the
	// worker jobs spawn creates.
	jobFallback func(ctx context.Context, id, action string) (any, error)
}

// SetJobFallback lets the job tool serve ids it did not start itself.
func (r *Registry) SetJobFallback(f func(ctx context.Context, id, action string) (any, error)) {
	r.jobFallback = f
}

// SessionState holds edit recovery and command jobs across model turns. Engine tool
// registries are intentionally rebuilt each turn, so this state must be owned
// by the session rather than by an individual Registry.
type SessionState struct {
	mu             sync.Mutex
	anchorFailures map[string]int
	noop           noopState
	snapshots      map[string]fileSnapshot
	jobs           map[string]*commandJob
}

type noopState struct {
	signature string
	streak    int
}

type fileSnapshot struct {
	version string
	content []string
	// history remembers the anchors this session handed out for the last few
	// versions of the file, so an edit invalidated by the session's own later
	// edits can be translated instead of failing as stale.
	history []anchorVersion
}

// anchorHistoryVersions bounds how many versions of a file the session
// remembers anchors for.
const anchorHistoryVersions = 8

type rememberedAnchor struct {
	text   string
	number int
}

// anchorVersion records the anchors of one version of a file: the line hashes
// of a read result or of a successful edit's result.
type anchorVersion struct {
	version string
	// byEdit marks a version this session's own successful edit produced.
	byEdit  bool
	anchors map[string]rememberedAnchor
	// content is the version's text, to align it with the current file.
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
	return "Apply content-anchored hashline hunks. You MUST read the exact target window immediately before editing and copy its latest 8-character hash into anchor. To create a new file, use anchor e3b0c442 with delete=0. Identical lines share a hash; when an anchor is ambiguous, also pass its line number from the read as line. Re-read after compaction or a stale error. Structural declaration deletion is rejected unless explicitly allowed."
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
	done chan struct{}
	err  error // Written before done closes; readers wait for done.
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
	r.add("edit", editDescription(r.dialect), schema(map[string]any{"path": str(), "hunks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"anchor": anchor, "line": map[string]any{"type": "integer", "description": "The anchor's line number from the latest read. Needed only when identical lines share a hash."}, "offset": num(), "delete": num(), "insert": map[string]any{"type": "array", "items": str()}, "allow_structural_change": boolean()}, "required": []string{"anchor", "delete", "insert"}, "additionalProperties": false}}}, "path", "hunks"), r.edit)
	r.add("exec", "Run a shell command in the workspace. Use background=true for long jobs.", schema(map[string]any{"command": str(), "background": boolean(), "timeout_seconds": num()}, "command"), r.run)
	r.add("job", "Wait for, cancel, or read logs from an exec command, or wait for (or check on) a worker job started by spawn. Accepts this session's cmd-… IDs, including the basename without .log from foreground exec log paths.", schema(map[string]any{"id": str(), "action": map[string]any{"type": "string", "enum": []string{"wait", "cancel", "logs"}}}, "id", "action"), r.job)
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
	if state.jobs == nil {
		state.jobs = map[string]*commandJob{}
	}
	state.mu.Unlock()
	r := &Registry{root: root, handlers: map[string]Handler{}, schemes: map[string]Handler{}, state: state, dialect: anchorDialect(dialect)}
	r.add("read", readDescription(r.dialect), schema(map[string]any{"path": str(), "start": num(), "limit": num(), "around_line": num()}, "path"), r.read)
	r.add("search", searchDescription, searchSchema(false), r.search)
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
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (value any, err error) {
	ctx, span := otel.Tracer("orrery/tools").Start(ctx, "tool.call")
	defer span.End()
	span.SetAttributes(attribute.String("tool", name))
	h, ok := r.handlers[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	// A bug in one tool must not take down the session, or the process
	// hosting it (the TUI and serve run sessions in-process): report it to
	// the model as a failed call and log the stack.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("tool panicked", "tool", name, "panic", rec, "stack", string(debug.Stack()))
			value, err = nil, fmt.Errorf("tool %s failed with an internal error (%v); this is a harness bug, not a problem with the call", name, rec)
		}
	}()
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
	snapshot := r.state.snapshots[p]
	snapshot.version = currentVersion
	snapshot.content = lineText(lines)
	snapshot.history = appendAnchorVersion(snapshot.history, currentVersion, false, lines)
	r.state.snapshots[p] = snapshot
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
	var translated []string
	if translated, err = r.translateStaleAnchors(&p, current); err != nil {
		return nil, err
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
		var amb *hashline.AmbiguousError
		if errors.As(err, &amb) {
			matches := make([]map[string]any, 0, len(amb.Windows))
			for i, w := range amb.Windows {
				matches = append(matches, map[string]any{"line": amb.Lines[i], "window": w})
			}
			return map[string]any{"error": err.Error(), "matching_lines": amb.Lines, "matches": matches, "directive": `retry with "line" set to the intended match, or anchor to a unique neighbouring line`}, err
		}
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
	snapshot := r.state.snapshots[p.Path]
	snapshot.version = currentVersion
	snapshot.content = lineText(result.Lines)
	snapshot.history = appendAnchorVersion(snapshot.history, currentVersion, true, result.Lines)
	r.state.snapshots[p.Path] = snapshot
	windows := computeFreshWindows(result.Lines, result.Affected)
	out := map[string]any{"applied": len(p.Hunks), "fresh_anchors": windows}
	if len(translated) > 0 {
		out["translated_anchors"] = translated
	}
	return out, nil
}

// appendAnchorVersion records the anchors this session handed out for one
// version of a file, keeping the last anchorHistoryVersions versions.
func appendAnchorVersion(history []anchorVersion, version string, byEdit bool, lines []hashline.Line) []anchorVersion {
	anchors := make(map[string]rememberedAnchor, len(lines))
	for _, line := range lines {
		if line.Hash == "" {
			continue
		}
		// A repeated hash matched several lines and cannot be translated.
		// Mark it empty so lookup refuses it.
		if _, ok := anchors[line.Hash]; !ok {
			anchors[line.Hash] = rememberedAnchor{text: line.Text, number: line.Number}
		} else {
			anchors[line.Hash] = rememberedAnchor{}
		}
	}
	history = append(history, anchorVersion{version: version, byEdit: byEdit, anchors: anchors, content: lineText(lines)})
	if len(history) > anchorHistoryVersions {
		history = history[len(history)-anchorHistoryVersions:]
	}
	return history
}

// translateStaleAnchors rewrites hunk anchors that are stale in the current
// file but were handed out for an earlier version this session produced or
// read. Translation only runs when the current version is one this session's
// own edit produced; anything else is left for the caller, which reports
// E_FILE_CHANGED. An anchor whose remembered line is gone or ambiguous stays
// stale, and ApplyWithMode reports it as today.
func (r *Registry) translateStaleAnchors(p *hashline.Patch, current []hashline.Line) ([]string, error) {
	if len(current) == 0 {
		return nil, nil
	}
	snapshot := r.state.snapshots[p.Path]
	if len(snapshot.history) == 0 || !snapshot.history[len(snapshot.history)-1].byEdit {
		return nil, nil
	}
	var translated []string
	for i := range p.Hunks {
		if r.anchorPresent(current, p.Hunks[i].Anchor) {
			continue
		}
		remembered, version, ok := lookupRemembered(snapshot.history, p.Hunks[i].Anchor)
		if !ok {
			continue
		}
		at, ok := retainedLine(version.content, lineText(current), remembered.number-1)
		if !ok || current[at].Text != remembered.text {
			continue
		}
		line := current[at]
		old := p.Hunks[i].Anchor
		p.Hunks[i].Anchor = line.Hash
		p.Hunks[i].Line = line.Number
		translated = append(translated, old)
	}
	return translated, nil
}

func (r *Registry) anchorPresent(lines []hashline.Line, anchor string) bool {
	for _, line := range lines {
		if r.anchorMatches(line.Hash, anchor) {
			return true
		}
	}
	return false
}

// lookupRemembered finds the oldest remembered version that handed out this
// anchor unambiguously. A contextual hash is valid only for the neighbours of
// the version that produced it, so a later version must not shadow it. An
// empty remembered anchor marks one that matched several lines.
func lookupRemembered(history []anchorVersion, anchor string) (rememberedAnchor, anchorVersion, bool) {
	for i := 0; i < len(history); i++ {
		remembered, ok := history[i].anchors[anchor]
		if !ok {
			continue
		}
		if remembered.number == 0 {
			return rememberedAnchor{}, anchorVersion{}, false
		}
		return remembered, history[i], true
	}
	return rememberedAnchor{}, anchorVersion{}, false
}

// maxAlignCells bounds the alignment table retainedLine builds for the
// region the session's edits changed; past it, anchors are not translated.
const maxAlignCells = 4_000_000

// retainedLine maps line old (0-based) of an earlier version to its index in
// the current file, when the session's own edits left that line in place. The
// versions are aligned by their common prefix and suffix and a longest common
// subsequence of the rest, so a line whose text merely recurs elsewhere (a
// closing brace after its block was deleted) is not mistaken for it.
func retainedLine(before, after []string, old int) (int, bool) {
	if old < 0 || old >= len(before) {
		return 0, false
	}
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	if old < prefix {
		return old, true
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix && before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	if old >= len(before)-suffix {
		return old - len(before) + len(after), true
	}
	b, a := before[prefix:len(before)-suffix], after[prefix:len(after)-suffix]
	if len(b)*len(a) > maxAlignCells {
		return 0, false
	}
	// lcs[i][j] is the common subsequence length of b[i:] and a[j:].
	lcs := make([][]int, len(b)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(a)+1)
	}
	for i := len(b) - 1; i >= 0; i-- {
		for j := len(a) - 1; j >= 0; j-- {
			if b[i] == a[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	for i, j := 0, 0; i < len(b) && j < len(a); {
		switch {
		case b[i] == a[j] && lcs[i][j] == lcs[i+1][j+1]+1:
			if prefix+i == old {
				return prefix + j, true
			}
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			if prefix+i == old {
				return 0, false
			}
			i++
		default:
			j++
		}
	}
	return 0, false
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
		lo := min(len(lines), max(0, r.Start-2))
		hi := max(lo, min(len(lines), r.End+2))
		out = append(out, lines[lo:hi])
	}
	return out
}
func (r *Registry) run(ctx context.Context, a map[string]any) (any, error) {
	cmdText := asString(a["command"])
	if cmdText == "" {
		return nil, errors.New("command required")
	}
	if reason := destructiveCommand(cmdText, r.root); reason != "" {
		return nil, fmt.Errorf("exec rejected: %s would discard work that cannot be recovered. Uncommitted changes may belong to the person; leave them alone, or ask.", reason)
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
	j := &commandJob{cmd: cmd, path: path, done: make(chan struct{})}
	r.state.mu.Lock()
	r.state.jobs[id] = j
	r.state.mu.Unlock()
	go func() { j.err = cmd.Wait(); f.Close(); close(j.done) }()
	if bg, _ := a["background"].(bool); bg {
		return map[string]any{"id": id, "log": path}, nil
	}
	timeout := time.Duration(asInt(a["timeout_seconds"], 120)) * time.Second
	select {
	case <-j.done:
		return commandSummary(path, j.err)
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("command timed out; log: %s", path)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Registry) job(ctx context.Context, a map[string]any) (any, error) {
	id := asString(a["id"])
	r.state.mu.Lock()
	j := r.state.jobs[id]
	r.state.mu.Unlock()
	if j == nil {
		if r.jobFallback != nil {
			return r.jobFallback(ctx, id, asString(a["action"]))
		}
		return nil, errors.New("job not found")
	}
	switch asString(a["action"]) {
	case "logs":
		return commandSummary(j.path, nil)
	case "cancel":
		return map[string]any{"cancelled": j.cmd.Process.Kill() == nil}, nil
	case "wait":
		select {
		case <-j.done:
			return commandSummary(j.path, j.err)
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
