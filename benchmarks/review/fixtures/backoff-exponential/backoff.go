// Package backoff computes retry delays.
package backoff

import "time"

// Constant returns a schedule that waits d before every attempt.
func Constant(d time.Duration) func(attempt int) time.Duration {
	return func(int) time.Duration { return d }
}
