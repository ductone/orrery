package retryboundary

import "errors"

var ErrNoAttempts = errors.New("no attempts configured")

// Retry invokes fn at most attempts times and stops after the first success.
func Retry(attempts int, fn func() error) error {
	var err error
	for attempt := 0; attempt <= attempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
	}
	return err
}
