// Package wordset has helpers for lists of words.
package wordset

import "strings"

// Count returns how many times each word occurs, ignoring case.
func Count(words []string) map[string]int {
	out := map[string]int{}
	for _, w := range words {
		out[strings.ToLower(w)]++
	}
	return out
}
