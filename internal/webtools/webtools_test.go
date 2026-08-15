package webtools

import (
	"context"
	"net/url"
	"testing"
)

func TestValidateURLRejectsPrivateAndCredentials(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "http://127.0.0.1/x", "http://user:pass@example.com"} {
		u, _ := url.Parse(raw)
		if err := validateURL(context.Background(), u); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
