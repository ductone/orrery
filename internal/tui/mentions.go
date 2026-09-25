package tui

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const maxIndexedFiles = 50_000

// indexFiles lists workspace files for @-mention completion, skipping the
// same dependency and state trees the search tool ignores.
func indexFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		if len(out) >= maxIndexedFiles {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".orrery", ".task-worktrees", "node_modules", "vendor", "local_vendor", "bazel-bin", "bazel-out", "bazel-testlogs", ".cache", "dist", "target", "__pycache__", ".venv":
		return true
	}
	return false
}

// fuzzyScore scores a subsequence match of pattern in candidate. Matches at
// path-segment starts, in the basename, and in contiguous runs rank higher;
// shorter candidates win ties.
func fuzzyScore(pattern, candidate string) (int, bool) {
	if pattern == "" {
		return -len(candidate), true
	}
	p := []rune(strings.ToLower(pattern))
	c := []rune(strings.ToLower(candidate))
	base := strings.LastIndexByte(candidate, '/') + 1
	score, pi, prev := 0, 0, -2
	for ci := 0; ci < len(c) && pi < len(p); ci++ {
		if c[ci] != p[pi] {
			continue
		}
		bonus := 1
		if ci == prev+1 {
			bonus += 6
		}
		if ci == 0 || !unicode.IsLetter(c[ci-1]) && !unicode.IsDigit(c[ci-1]) {
			bonus += 5
		}
		if ci >= base {
			bonus += 3
		}
		score += bonus
		prev = ci
		pi++
	}
	if pi < len(p) {
		return 0, false
	}
	if strings.Contains(strings.ToLower(candidate[base:]), string(p)) {
		score += 20
	}
	return score*4 - len(c), true
}

func fuzzyFilter(pattern string, candidates []string, limit int) []string {
	type scored struct {
		s     string
		score int
	}
	var hits []scored
	for _, c := range candidates {
		if score, ok := fuzzyScore(pattern, c); ok {
			hits = append(hits, scored{c, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.s
	}
	return out
}

// mentionToken returns the @-token being typed at the end of value.
func mentionToken(value string) (string, bool) {
	i := strings.LastIndexAny(value, " \t\n")
	token := value[i+1:]
	if !strings.HasPrefix(token, "@") {
		return "", false
	}
	return token[1:], true
}
