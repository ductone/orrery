// Package client talks to the handler over HTTP.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"benchmark/paged-items/store"
)

// ErrNotFound is returned when the server has no such item.
var ErrNotFound = errors.New("client: not found")

// Client calls the item API.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a Client for the server at base. A nil hc means http.DefaultClient.
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: base, hc: hc}
}

// Get fetches one item.
func (c *Client) Get(id string) (store.Item, error) {
	resp, err := c.hc.Get(c.base + "/items/" + url.PathEscape(id))
	if err != nil {
		return store.Item{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return store.Item{}, ErrNotFound
	default:
		return store.Item{}, fmt.Errorf("client: unexpected status %d", resp.StatusCode)
	}
	var it store.Item
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return store.Item{}, err
	}
	return it, nil
}
