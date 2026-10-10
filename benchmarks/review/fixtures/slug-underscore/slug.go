// Package slug turns titles into URL slugs.
package slug

import "strings"

// Slug lowercases s and replaces each run of characters other than a-z and
// 0-9 with a single hyphen. Leading and trailing separators are dropped.
func Slug(s string) string {
	var b strings.Builder
	pending := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pending && b.Len() > 0 {
				b.WriteByte('-')
			}
			pending = false
			b.WriteRune(r)
		} else {
			pending = true
		}
	}
	return b.String()
}
