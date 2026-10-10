// Package chunker splits slices into fixed-size chunks.
package chunker

// Chunk splits items into consecutive chunks of at most size elements.
func Chunk(items []int, size int) [][]int {
	var out [][]int
	for i := 0; i < len(items); i += size {
		end := i + size
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[i:end])
	}
	return out
}
