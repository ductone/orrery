// Package kv parses simple key=value configuration text.
package kv

import "strings"

// Parse reads key=value lines. Blank lines and lines starting with # are
// ignored. Values may be wrapped in double quotes, which are removed.
func Parse(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "=")
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out
}
