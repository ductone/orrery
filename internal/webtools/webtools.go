package webtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	key  string
	http *http.Client
}

func New(key string) *Client {
	c := &Client{key: key}
	c.http = &http.Client{Transport: &http.Transport{DialContext: safeDial}, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		return validateURL(req.Context(), req.URL)
	}}
	return c
}
func safeDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if publicIP(ip) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
	}
	return nil, fmt.Errorf("address %s has no public IP", host)
}

// SearchConfigured reports whether web search has a provider key.
func (c *Client) SearchConfigured() bool { return c != nil && c.key != "" }

func (c *Client) Search(ctx context.Context, query string, count int) (any, error) {
	if c.key == "" {
		return nil, errors.New("web search is not configured")
	}
	if count <= 0 || count > 20 {
		count = 8
	}
	u := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query) + "&count=" + strconv.Itoa(count)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("X-Subscription-Token", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Brave HTTP %d: %s", resp.StatusCode, truncate(raw, 1000))
	}
	var wire struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	return wire.Web.Results, nil
}

// FetchChars is how much text one fetch returns; later windows are read with
// a start offset.
const FetchChars = 60_000

// Fetch returns a page's readable text, a window at a time. HTML is reduced
// to its text; other text types are returned as they are.
func (c *Client) Fetch(ctx context.Context, rawURL string, start int) (any, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err = validateURL(ctx, u); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", "Orrery/0.3")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 1000))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
		return nil, fmt.Errorf("unsupported content type %q", ct)
	}
	out := map[string]any{"url": resp.Request.URL.String(), "content_type": ct}
	text := string(b)
	if strings.Contains(ct, "html") {
		title, body := htmlText(b)
		text = body
		if title != "" {
			out["title"] = title
		}
		out["extracted_from_html"] = true
	}
	for k, v := range textWindow(text, start) {
		out[k] = v
	}
	return out, nil
}

// textWindow returns up to FetchChars of text from start, ending on a line
// boundary when one is near, and where to continue.
func textWindow(text string, start int) map[string]any {
	start = max(0, min(start, len(text)))
	end := min(len(text), start+FetchChars)
	if end < len(text) {
		if nl := strings.LastIndexByte(text[start:end], '\n'); nl > FetchChars/2 {
			end = start + nl + 1
		}
	}
	out := map[string]any{"content": text[start:end], "total_chars": len(text)}
	if end < len(text) {
		out["next_start"] = end
		out["hint"] = fmt.Sprintf("Showing characters %d-%d of %d. Fetch again with start=%d for more.", start, end, len(text), end)
	}
	return out
}
func validateURL(ctx context.Context, u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http and https URLs are allowed")
	}
	if u.User != nil {
		return errors.New("URL credentials are forbidden")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return fmt.Errorf("address %s is not public", ip)
		}
	}
	return nil
}

var nonPublicPrefixes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"), netip.MustParsePrefix("2001:db8::/32")}

func publicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}
func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
