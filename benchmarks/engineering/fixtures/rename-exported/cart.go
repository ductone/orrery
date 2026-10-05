package shop

// CartIsFree reports whether the cart costs nothing.
func CartIsFree(items []Item) bool {
	return CalcTotal(items) == 0
}
