// Package shipping prices parcels.
package shipping

// Cost returns the shipping price in cents for a parcel of the given weight.
func Cost(weightGrams int, express bool) int {
	if weightGrams <= 500 {
		if express {
			return 1200
		}
		return 500
	}
	if weightGrams <= 2000 {
		if express {
			return 1800
		}
		return 900
	}
	if express {
		return 3000
	}
	return 1500
}
