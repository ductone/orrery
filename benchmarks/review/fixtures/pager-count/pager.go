// Package pager slices lists into pages.
package pager

// Page returns the 1-based page of items with the given page size.
// It returns nil when the page is out of range or size is not positive.
func Page(items []string, page, size int) []string {
	if page < 1 || size < 1 {
		return nil
	}
	start := (page - 1) * size
	if start >= len(items) {
		return nil
	}
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}
