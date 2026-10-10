// Package tagindex maps tags to the IDs that carry them.
package tagindex

// Index maps tags to IDs. The zero value is ready to use.
type Index struct {
	m map[string][]string
}

// New returns an empty Index.
func New() *Index { return &Index{m: map[string][]string{}} }

// Add records that id carries tag.
func (ix *Index) Add(tag, id string) {
	if ix.m == nil {
		ix.m = map[string][]string{}
	}
	ix.m[tag] = append(ix.m[tag], id)
}

// IDs returns the IDs recorded under tag, in insertion order.
func (ix *Index) IDs(tag string) []string { return ix.m[tag] }
