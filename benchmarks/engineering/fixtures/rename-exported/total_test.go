package shop

import "testing"

func TestTotals(t *testing.T) {
	items := []Item{{"a", 150, 2}, {"b", 100, 1}}
	if got := CalcTotal(items); got != 400 {
		t.Fatalf("CalcTotal = %d, want 400", got)
	}
	if got := Invoice(items); got != "invoice: 400 cents" {
		t.Fatalf("Invoice = %q", got)
	}
	if CartIsFree(items) || !CartIsFree(nil) {
		t.Fatal("CartIsFree wrong")
	}
	if got := Average(items); got != 200 {
		t.Fatalf("Average = %d, want 200", got)
	}
}
