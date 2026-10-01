// Package catalog builds the model catalog Orrery routes over: models
// discovered from providers that list them, then the built-in catalog, then
// local configuration overrides, each layer winning over the one before.
//
// Discovery is best effort. A provider that cannot be reached falls back to
// its last good listing, cached on disk, and then to the built-in catalog
// alone; it never stops Orrery from starting. Configuration stays strict:
// overrides are validated when the config loads.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/model"
)

// DefaultRampBaseURL is Ramp Router's API base when the config sets none.
const DefaultRampBaseURL = "https://api.router.com"

// discoveryTimeout bounds startup's wait for a provider listing.
const discoveryTimeout = 5 * time.Second

// rampModel is the part of a Ramp Router /v1/models entry inference reads.
type rampModel struct {
	ID     string `json:"id"`
	Router struct {
		RequestName string   `json:"request_name"`
		Status      string   `json:"status"`
		Surfaces    []string `json:"surfaces"`
		Limits      struct {
			ContextWindow   int  `json:"context_window"`
			MaxOutputTokens *int `json:"max_output_tokens"`
		} `json:"limits"`
		Capabilities struct {
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
			Tools struct {
				Supported bool `json:"supported"`
			} `json:"tools"`
			PromptCaching bool `json:"prompt_caching"`
			Reasoning     struct {
				Supported bool `json:"supported"`
				Efforts   []struct {
					Value string `json:"value"`
				} `json:"efforts"`
			} `json:"reasoning"`
		} `json:"capabilities"`
		// Providers are the upstreams that serve the model.
		Providers []struct {
			Provider string `json:"provider"`
		} `json:"providers"`
		Pricing struct {
			Input     string `json:"input"`
			Output    string `json:"output"`
			CacheRead string `json:"cache_read_input"`
			// CacheWrite is the default write price; some entries price
			// only the TTL-specific variants.
			CacheWrite   string `json:"cache_write_input"`
			CacheWrite5m string `json:"cache_write_input_5m"`
		} `json:"pricing"`
	} `json:"router"`
}

// fetchRamp lists Ramp Router's models.
func fetchRamp(ctx context.Context, client *http.Client, baseURL, key string) ([]rampModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	var out struct {
		Data []rampModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("list models: decode: %w", err)
	}
	return out.Data, nil
}

// Discovery is the outcome of listing one provider's models.
type Discovery struct {
	Provider string `json:"provider"`
	// Models are the inferred specs, empty when discovery failed without a cache.
	Models []model.ModelSpec `json:"models"`
	// Listed and Skipped count raw entries, and why some were left out.
	Listed  int            `json:"listed"`
	Skipped map[string]int `json:"skipped,omitempty"`
	// Source is "live", "cache", or "none".
	Source  string    `json:"source"`
	Fetched time.Time `json:"fetched,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// DiscoverRamp lists Ramp Router's models and infers specs for them. On
// failure it falls back to the last good listing cached under cacheDir.
func DiscoverRamp(ctx context.Context, client *http.Client, baseURL, key, cacheDir string, providerKeys []string) Discovery {
	d := Discovery{Provider: "ramp", Source: "none"}
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	listed, err := fetchRamp(ctx, client, baseURL, key)
	if err == nil {
		d.Models, d.Skipped = inferRamp(listed, providerKeys)
		d.Listed, d.Source, d.Fetched = len(listed), "live", time.Now().UTC()
		writeCache(cacheDir, d)
		return d
	}
	d.Error = err.Error()
	if cached, ok := readCache(cacheDir, "ramp"); ok {
		cached.Source, cached.Error = "cache", d.Error
		return cached
	}
	return d
}

func cachePath(dir, provider string) string {
	return filepath.Join(dir, "catalog-"+provider+".json")
}

func writeCache(dir string, d Discovery) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	tmp := cachePath(dir, d.Provider) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, cachePath(dir, d.Provider))
	}
}

func readCache(dir, provider string) (Discovery, bool) {
	if dir == "" {
		return Discovery{}, false
	}
	b, err := os.ReadFile(cachePath(dir, provider))
	if err != nil {
		return Discovery{}, false
	}
	var d Discovery
	if json.Unmarshal(b, &d) != nil || len(d.Models) == 0 {
		return Discovery{}, false
	}
	return d, true
}

func price(s string) (float64, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil && v >= 0
}
