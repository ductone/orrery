package wordset

// Unique returns the distinct words in the order each was first seen.
// Comparison is exact (case-sensitive). It returns nil for empty input and
// does not modify words.
func Unique(words []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(words))
	for _, w := range words {
		if _, ok := seen[w]; ok {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}
