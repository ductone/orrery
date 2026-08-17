package store

import (
	"strings"
	"unicode"
)

// Title constants.
const (
	MaxFallbackTitle  = 80
	MaxGeneratedTitle = 60
)

// SessionTitle returns a deterministic, concise fallback title for a session spec.
// It normalizes whitespace and truncates at a word boundary so callers can safely
// display a title before an asynchronous model-generated title is available.
func SessionTitle(spec string) string {
	if spec = strings.TrimSpace(spec); spec == "" {
		return "Untitled task"
	}

	// Collapse runs of whitespace and trim line breaks.
	var b strings.Builder
	var prevSpace bool
	for _, r := range spec {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	normalized := strings.TrimSpace(b.String())

	if len(normalized) <= MaxFallbackTitle {
		return normalized
	}

	// Truncate at the last word boundary before MaxFallbackTitle.
	cut := strings.LastIndexAny(normalized[:MaxFallbackTitle], " \t\n-—")
	if cut <= 0 {
		cut = MaxFallbackTitle
	}
	return strings.TrimRight(normalized[:cut], " \t\n-—") + "…"
}

// NormalizeTitle validates and cleans a model-generated title. It returns an empty
// string if the candidate is not suitable to replace the deterministic fallback.
func NormalizeTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Strip surrounding quotes.
	if (strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"")) ||
		(strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'")) ||
		(strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`")) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	// Remove markdown headings and common prefixes.
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), "title: ")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Require a single line.
	if strings.ContainsAny(s, "\r\n") {
		return ""
	}
	// Length cap for generated titles.
	if len(s) > MaxGeneratedTitle {
		s = s[:MaxGeneratedTitle]
		if cut := strings.LastIndexAny(s, " \t\n-—"); cut > 0 {
			s = strings.TrimRight(s[:cut], " \t\n-—") + "…"
		}
	}
	// Avoid returning an empty or whitespace-only title.
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// DisplayTitle returns the session title to show in UIs. If the persisted Title is
// empty, it derives a deterministic fallback from the full spec without mutating state.
func (s Session) DisplayTitle() string {
	if strings.TrimSpace(s.Title) != "" {
		return s.Title
	}
	return SessionTitle(s.Spec)
}

// TitlePrompt is the system prompt used for lightweight model-generated session titles.
const TitlePrompt = `You are a title generator. Create a concise, human-readable session title (3–8 words) that summarizes the user's task. Output only the title text. No quotes, no markdown, no explanation, no numbering.`
