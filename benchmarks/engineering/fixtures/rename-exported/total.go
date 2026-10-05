// Package shop computes order totals.
package shop

// Item is a priced line item.
type Item struct {
	Name  string
	Cents int
	Qty   int
}

// CalcTotal returns the total price of items in cents.
func CalcTotal(items []Item) int {
	total := 0
	for _, it := range items {
		total += it.Cents * it.Qty
	}
	return total
}
