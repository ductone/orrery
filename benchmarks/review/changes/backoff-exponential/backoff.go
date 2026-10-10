// Package backoff computes retry delays.
package backoff

import "time"

// Constant returns a schedule that waits d before every attempt.
func Constant(d time.Duration) func(attempt int) time.Duration {
	return func(int) time.Duration { return d }
}

// Exponential returns a schedule that waits base, 2*base, 4*base, ... and
// never more than max. Attempts are zero-based.
func Exponential(base, max time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		d := base << uint(attempt)
		if d > max {
			return max
		}
		return d
	}
}
