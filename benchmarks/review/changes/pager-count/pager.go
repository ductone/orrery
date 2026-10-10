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

// PageCount returns how many pages are needed to hold total items at size
// items per page. It returns 0 when total or size is not positive.
func PageCount(total, size int) int {
	if total <= 0 || size < 1 {
		return 0
	}
	return total/size + 1
}
