// Package shipping prices parcels.
package shipping

type tier struct {
	maxGrams int // inclusive upper bound
	standard int // cents
	express  int // cents
}

// tiers are checked in order; weights above the last bound use heavy.
var tiers = []tier{
	{maxGrams: 500, standard: 500, express: 1200},
	{maxGrams: 2000, standard: 900, express: 1800},
}

var heavy = tier{standard: 1500, express: 3000}

// Cost returns the shipping price in cents for a parcel of the given weight.
func Cost(weightGrams int, express bool) int {
	t := heavy
	for _, c := range tiers {
		if weightGrams <= c.maxGrams {
			t = c
			break
		}
	}
	if express {
		return t.express
	}
	return t.standard
}
