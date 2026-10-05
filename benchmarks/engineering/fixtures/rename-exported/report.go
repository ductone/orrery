package shop

// Average returns the mean line-item total in cents, or 0 for no items.
func Average(items []Item) int {
	if len(items) == 0 {
		return 0
	}
	return CalcTotal(items) / len(items)
}
