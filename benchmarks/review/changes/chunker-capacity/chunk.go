// Package chunker splits slices into fixed-size chunks.
package chunker

// Chunk splits items into consecutive chunks of at most size elements.
// It returns nil when size is not positive. Each chunk has its capacity
// limited to its length, so appending to one chunk never overwrites the
// next chunk's elements in items.
func Chunk(items []int, size int) [][]int {
	if size < 1 {
		return nil
	}
	var out [][]int
	for i := 0; i < len(items); {
		end := len(items)
		if size < end-i { // written this way so i+size cannot overflow
			end = i + size
		}
		out = append(out, items[i:end:end])
		i = end
	}
	return out
}
