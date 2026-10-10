// Package fetcher downloads small text resources.
package fetcher

import (
	"io"
	"net/http"
)

// Fetch returns the body of url fetched with c.
func Fetch(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
