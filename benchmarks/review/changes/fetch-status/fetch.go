// Package fetcher downloads small text resources.
package fetcher

import (
	"fmt"
	"io"
	"net/http"
)

// Fetch returns the body of url fetched with c. A response whose status is
// not 200 OK is returned as an error naming the status code.
func Fetch(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: unexpected status %d", url, resp.StatusCode)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
