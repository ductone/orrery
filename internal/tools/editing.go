package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"unicode/utf8"

	"github.com/ductone/orrery/internal/hashline"
)

const writeDescription = "Write file content. Creates the file if it does not exist, or replaces its entire contents. Use this tool to create a new file instead of edit. The file becomes part of the session's file state, so a later edit must re-read it first."

const replaceDescription = "Exact string replacement across workspace files, directories or glob patterns. With replace_all=false (default) an old_string that matches more than once in a file fails the call and lists the matching line numbers; with replace_all=true every occurrence is replaced. Exact text only, no regex. Files matched by a directory or glob that do not contain the string are skipped. Returns per-file counts and changed lines with fresh hashline anchors."

const astEditDescription = "Structural code rewrite across workspace files with the ast-grep rule engine. Runs the given pattern/rewrite ops over the matched files in one call and returns a compact per-file list of changed lines with fresh anchors (no preview step). Fails cleanly on overlapping matches or files that change between match and apply."

// astGrepAvailable reports whether the ast-grep CLI is on PATH. The ast_edit
// tool is only registered when it is, so running without it merely omits the
// tool rather than failing calls.
func astGrepAvailable() bool {
	_, err := exec.LookPath("ast-grep")
	return err == nil
}

// resolvedPath is one file an editing tool will touch, with its provenance.
type resolvedPath struct {
	path     string
	explicit bool // named literally, not expanded from a directory or glob
}

// expandEditPaths expands files, directories and glob patterns inside the
// workspace into a sorted, de-duplicated list of regular files. Directories
// are walked recursively, ignoring the same directories as search. Globs are
// matched against workspace-relative paths with the search tool's globMatch,
// so ** and {a,b} work as they do there. Files named literally on the command
// line are marked explicit.
func (r *Registry) expandEditPaths(raw []string) ([]resolvedPath, error) {
	var out []resolvedPath
	seen := map[string]bool{}
	add := func(p string, explicit bool) {
		key := filepath.Clean(p)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, resolvedPath{path: p, explicit: explicit})
	}
	for _, rawPath := range raw {
		if rawPath == "" {
			continue
		}
		if strings.ContainsAny(rawPath, "*?[{") {
			pattern := filepath.ToSlash(rawPath)
			err := filepath.WalkDir(r.root, func(p string, d fs.DirEntry, e error) error {
				if e != nil {
					return nil
				}
				if d.IsDir() {
					if p != r.root && ignoredSearchDir(d.Name()) {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(r.root, p)
				if err != nil {
					return nil
				}
				if globMatch(pattern, filepath.ToSlash(rel)) {
					add(p, false)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		safe, err := r.safe(rawPath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(safe)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", rawPath, err)
		}
		if info.IsDir() {
			err := filepath.WalkDir(safe, func(p string, d fs.DirEntry, e error) error {
				if e != nil {
					return nil
				}
				if d.IsDir() {
					if p != safe && ignoredSearchDir(d.Name()) {
						return filepath.SkipDir
					}
					return nil
				}
				if d.Type().IsRegular() {
					add(p, false)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else if info.Mode().IsRegular() {
			add(safe, true)
		} else {
			return nil, fmt.Errorf("path %q is not a regular file or directory", rawPath)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// asStringSlice extracts a []string from an argument the model provides either
// as a slice of strings or as a single string.
func asStringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	default:
		return nil
	}
}

// refreshSnapshot records the file's current state as the version this session
// last produced, so a following hashline edit sees fresh anchors. It must be
// called with the file already written and the file lock released.
func (r *Registry) refreshSnapshot(path string) error {
	lines, err := hashline.ReadWithMode(path, r.dialect.mode())
	if err != nil {
		return err
	}
	currentVersion, err := fileVersion(path)
	if err != nil {
		return err
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	snapshot := r.state.snapshots[path]
	snapshot.version = currentVersion
	snapshot.content = lineText(lines)
	snapshot.history = appendAnchorVersion(snapshot.history, currentVersion, true, lines)
	snapshot.execChanged = false
	r.state.snapshots[path] = snapshot
	return nil
}

// spannedLines returns the 1-based line numbers a needled occurrence at byte
// offset spans in content.
func spannedLines(content, needle string, offset int) []int {
	start := strings.Count(content[:offset], "\n") + 1
	end := start + strings.Count(needle, "\n")
	out := make([]int, 0, end-start+1)
	for l := start; l <= end; l++ {
		out = append(out, l)
	}
	return out
}

// changedLineEntries returns the 1-based lines of after that differ from
// before, annotated with the fresh hashline anchor of the new line. When the
// line count is unchanged lines are compared one to one; otherwise the common
// prefix and suffix are trimmed so an inserted or removed line does not mark
// every following line as changed.
func changedLineEntries(before, after []string, fresh []hashline.Line) []map[string]any {
	var idx []int
	if len(before) == len(after) {
		for i := range after {
			if before[i] != after[i] {
				idx = append(idx, i)
			}
		}
	} else {
		lo, hi := changedSpan(before, after)
		for i := lo; i < hi; i++ {
			idx = append(idx, i)
		}
	}
	out := make([]map[string]any, 0, len(idx))
	for _, i := range idx {
		// strings.Split leaves an empty element after a trailing newline; it
		// is not a line of the file.
		if i >= len(fresh) {
			continue
		}
		out = append(out, map[string]any{"line": i + 1, "text": after[i], "hash": fresh[i].Hash})
	}
	return out
}

// changedSpan returns the half-open range [lo, hi) of after's indexes left once
// the lines after shares with before at the start and end are removed.
func changedSpan(before, after []string) (int, int) {
	p := 0
	for p < len(before) && p < len(after) && before[p] == after[p] {
		p++
	}
	s := 0
	for s < len(before)-p && s < len(after)-p && before[len(before)-1-s] == after[len(after)-1-s] {
		s++
	}
	return p, len(after) - s
}

// write writes file content, creating the file or replacing its entire
// contents. It goes through the same file-state path as edit: workspace
// boundary check, changed-file snapshot, and fresh hashline anchors for later
// edits.
func (r *Registry) write(_ context.Context, a map[string]any) (any, error) {
	rawPath := asString(a["path"])
	if rawPath == "" {
		return nil, errors.New("path required")
	}
	safe, err := r.safe(rawPath)
	if err != nil {
		return nil, err
	}
	content := asString(a["content"])
	appendMode := false
	if v, ok := a["append"].(bool); ok {
		appendMode = v
	}

	lock := fileLock(safe)
	lock.Lock()
	defer lock.Unlock()

	created := false
	mode := os.FileMode(0o644)
	if info, err := os.Stat(safe); err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("path %q is a directory", rawPath)
		}
		mode = info.Mode()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		created = true
	}

	data := []byte(content)
	if appendMode && !created {
		b, err := os.ReadFile(safe)
		if err != nil {
			return nil, err
		}
		joined := make([]byte, 0, len(b)+1+len(content))
		joined = append(joined, b...)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			joined = append(joined, '\n')
		}
		joined = append(joined, content...)
		data = joined
	}
	// Ensure a trailing newline for line-based tools.
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	if err := os.WriteFile(safe, data, mode); err != nil {
		return nil, err
	}
	if err := r.refreshSnapshot(safe); err != nil {
		return nil, err
	}
	lines, err := hashline.ReadWithMode(safe, r.dialect.mode())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":       safe,
		"bytes":      len(data),
		"created":    created,
		"lines":      lineText(lines),
		"anchors":    lines,
		"line_count": len(lines),
	}, nil
}

// replace replaces an exact string in one or more files. The whole call is
// validated before anything is written, so a file that violates the
// constraints (multiple matches without replace_all, or an explicitly named
// file with no match) fails the call without leaving earlier files modified.
func (r *Registry) replace(_ context.Context, a map[string]any) (any, error) {
	oldString := asString(a["old_string"])
	if oldString == "" {
		return nil, errors.New("old_string must not be empty")
	}
	newString := asString(a["new_string"])
	replaceAll, _ := a["replace_all"].(bool)
	rawPaths := asStringSlice(a["paths"])
	if len(rawPaths) == 0 {
		return nil, errors.New("paths required")
	}
	resolved, err := r.expandEditPaths(rawPaths)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, errors.New("no files matched the given paths")
	}

	type filePlan struct {
		path   string
		count  int
		before string
		after  string
	}
	var plans []filePlan
	var violations []string
	for _, rp := range resolved {
		lock := fileLock(rp.path)
		lock.Lock()
		raw, err := os.ReadFile(rp.path)
		if err != nil {
			lock.Unlock()
			return nil, err
		}
		if !utf8.Valid(raw) {
			lock.Unlock()
			return nil, fmt.Errorf("replace: %s is not a UTF-8 text file", rp.path)
		}
		content := string(raw)
		count := strings.Count(content, oldString)
		if count == 0 {
			lock.Unlock()
			if rp.explicit {
				violations = append(violations, fmt.Sprintf("%s: no matches of %q", rp.path, oldString))
			}
			continue
		}
		if !replaceAll && count > 1 {
			var lines []int
			searchFrom := 0
			for i := 0; i < count; i++ {
				idx := strings.Index(content[searchFrom:], oldString)
				abs := searchFrom + idx
				lines = append(lines, spannedLines(content, oldString, abs)...)
				searchFrom = abs + len(oldString)
			}
			violations = append(violations, fmt.Sprintf("%s: %q matches %d times at lines %v (set replace_all=true or narrow the paths)", rp.path, oldString, count, lines))
			lock.Unlock()
			continue
		}
		lock.Unlock()
		after := strings.ReplaceAll(content, oldString, newString)
		if after == content {
			continue
		}
		plans = append(plans, filePlan{path: rp.path, count: count, before: content, after: after})
	}
	if len(violations) > 0 {
		return nil, fmt.Errorf("replace: %s", strings.Join(violations, "; "))
	}
	if len(plans) == 0 {
		return nil, errors.New("no files contained the replacement string")
	}

	// Write all files or none: each file is re-checked under its lock right
	// before it is written, and a failure restores the files already written.
	var written []filePlan
	rollback := func() {
		for _, w := range written {
			lock := fileLock(w.path)
			lock.Lock()
			_ = os.WriteFile(w.path, []byte(w.before), 0o644)
			lock.Unlock()
		}
	}
	for _, pl := range plans {
		lock := fileLock(pl.path)
		lock.Lock()
		current, err := os.ReadFile(pl.path)
		if err == nil && string(current) != pl.before {
			err = errors.New("file changed since the match was verified")
		}
		if err == nil {
			err = os.WriteFile(pl.path, []byte(pl.after), 0o644)
		}
		lock.Unlock()
		if err != nil {
			rollback()
			return nil, fmt.Errorf("replace: %s: %w; no files were modified, re-run replace", pl.path, err)
		}
		written = append(written, pl)
	}

	var results []map[string]any
	total := 0
	for _, pl := range plans {
		if err := r.refreshSnapshot(pl.path); err != nil {
			return nil, fmt.Errorf("replace wrote every file but could not refresh %s: %w", pl.path, err)
		}
		fresh, err := hashline.ReadWithMode(pl.path, r.dialect.mode())
		if err != nil {
			return nil, fmt.Errorf("replace wrote every file but could not read %s: %w", pl.path, err)
		}
		results = append(results, map[string]any{
			"path":          pl.path,
			"count":         pl.count,
			"changed_lines": changedLineEntries(strings.Split(pl.before, "\n"), strings.Split(pl.after, "\n"), fresh),
		})
		total += pl.count
	}
	return map[string]any{"replaced": results, "total": total}, nil
}

// astEditResult mirrors the subset of ast-grep's --json=compact output we need.
type astEditResult struct {
	Range struct {
		ByteOffset struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"byteOffset"`
	} `json:"range"`
	Replacement string `json:"replacement"`
}

// astEdit applies ast-grep pattern/rewrite ops over workspace files. It runs
// ast-grep once per (file, op), collects the matches against the original
// content, and applies the replacements itself by byte offset so the writes go
// through orrery's file-state path. It fails cleanly on overlapping matches
// and on files that change between match and apply.
func (r *Registry) astEdit(ctx context.Context, a map[string]any) (any, error) {
	opsRaw, ok := a["ops"].([]any)
	if !ok || len(opsRaw) == 0 {
		return nil, errors.New("ops required")
	}
	type op struct {
		pattern string
		rewrite string
	}
	ops := make([]op, 0, len(opsRaw))
	for _, item := range opsRaw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("each op must be an object with pattern and rewrite")
		}
		pattern := asString(m["pattern"])
		rewrite := asString(m["rewrite"])
		if pattern == "" {
			return nil, errors.New("op pattern must not be empty")
		}
		ops = append(ops, op{pattern: pattern, rewrite: rewrite})
	}
	rawPaths := asStringSlice(a["paths"])
	if len(rawPaths) == 0 {
		return nil, errors.New("paths required")
	}
	lang := asString(a["lang"])
	resolved, err := r.expandEditPaths(rawPaths)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, errors.New("no files matched the given paths")
	}

	var perFile []map[string]any
	total := 0
	for _, rp := range resolved {
		p := rp.path
		lock := fileLock(p)
		lock.Lock()
		original, err := os.ReadFile(p)
		if err != nil {
			lock.Unlock()
			return nil, err
		}
		type edit struct {
			start, end  int
			replacement string
		}
		var edits []edit
		for _, o := range ops {
			cmd := exec.CommandContext(ctx, "ast-grep", "run", "-p", o.pattern, "-r", o.rewrite, "--json=compact")
			if lang != "" {
				cmd.Args = append(cmd.Args, "-l", lang)
			}
			cmd.Args = append(cmd.Args, p)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()
			var matches []astEditResult
			if jerr := json.Unmarshal(stdout.Bytes(), &matches); jerr != nil {
				lock.Unlock()
				return nil, fmt.Errorf("ast-grep produced invalid JSON for %s: %v; stderr: %s", p, jerr, stderr.String())
			}
			if runErr != nil && len(matches) == 0 && strings.TrimSpace(stderr.String()) != "" {
				// ast-grep exits 1 with an empty [] on a clean no-match, but
				// reports real pattern errors on stderr.
				lock.Unlock()
				return nil, fmt.Errorf("ast-grep on %s failed: %v; stderr: %s", p, runErr, stderr.String())
			}
			for _, m := range matches {
				start, end := m.Range.ByteOffset.Start, m.Range.ByteOffset.End
				if start < 0 || end < start || end > len(original) {
					lock.Unlock()
					return nil, fmt.Errorf("ast-grep returned an out-of-range match for %s (%d-%d)", p, start, end)
				}
				edits = append(edits, edit{start: start, end: end, replacement: m.Replacement})
			}
			if err := ctx.Err(); err != nil {
				lock.Unlock()
				return nil, err
			}
		}
		if len(edits) == 0 {
			lock.Unlock()
			continue
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		for i := 1; i < len(edits); i++ {
			if edits[i].start < edits[i-1].end {
				lock.Unlock()
				return nil, fmt.Errorf("ast_edit: overlapping matches in %s at offsets %d-%d and %d-%d; narrow the patterns", p, edits[i-1].start, edits[i-1].end, edits[i].start, edits[i].end)
			}
		}
		// Apply bottom-up by byte offset so earlier offsets stay valid. The
		// replacements were computed by ast-grep against the original bytes;
		// because all edits are disjoint, splicing them bottom-up is exact.
		newContent := append([]byte(nil), original...)
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			newContent = append(newContent[:e.start], append([]byte(e.replacement), newContent[e.end:]...)...)
		}
		if bytes.Equal(newContent, original) {
			lock.Unlock()
			continue
		}
		// Confirm the file is exactly what we matched before writing: the file
		// lock serializes our own tools, but an exec-driven change cannot be
		// observed until after we release the lock, so compare the bytes.
		current, err := os.ReadFile(p)
		if err != nil {
			lock.Unlock()
			return nil, err
		}
		if !bytes.Equal(current, original) {
			lock.Unlock()
			return nil, fmt.Errorf("file %s changed between match and apply; re-run the ast_edit call", p)
		}
		if err := os.WriteFile(p, newContent, 0o644); err != nil {
			lock.Unlock()
			return nil, err
		}
		lock.Unlock()
		if err := r.refreshSnapshot(p); err != nil {
			return nil, err
		}
		fresh, err := hashline.ReadWithMode(p, r.dialect.mode())
		if err != nil {
			return nil, err
		}
		perFile = append(perFile, map[string]any{
			"path":          p,
			"changed_lines": changedLineEntries(strings.Split(string(original), "\n"), strings.Split(string(newContent), "\n"), fresh),
		})
		total++
	}
	return map[string]any{"changed_files": total, "files": perFile}, nil
}
