// Package counter tracks hit counts shared between goroutines.
package counter

// Counter counts events.
type Counter struct {
	n int
}

// Inc records one event.
func (c *Counter) Inc() {
	c.n++
}

// Value returns the number of recorded events.
func (c *Counter) Value() int {
	return c.n
}
