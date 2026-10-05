package shop

import "fmt"

// Invoice renders a one-line invoice.
func Invoice(items []Item) string {
	return fmt.Sprintf("invoice: %d cents", CalcTotal(items))
}
