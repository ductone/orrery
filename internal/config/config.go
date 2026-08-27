package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string                    `yaml:"listen"`
	WorkspaceRoot string                    `yaml:"workspace_root"`
	Database      string                    `yaml:"database"`
	Providers     map[string]ProviderConfig `yaml:"providers"`
	MCP           map[string]MCPConfig      `yaml:"mcp"`
	Router        RouterConfig              `yaml:"router"`
	Budget        BudgetConfig              `yaml:"budget"`
	Telemetry     TelemetryConfig           `yaml:"telemetry"`
	WebSearch     WebSearchConfig           `yaml:"web_search"`
	Instructions  []string                  `yaml:"instructions"`
	LSP           map[string]LSPConfig      `yaml:"lsp"`
	Interventions InterventionConfig        `yaml:"interventions"`
}

// InterventionConfig governs the LLM judge that gates expensive progress
// interventions. The counters that trigger an intervention are deliberately
// loose; the judge decides whether the trigger reflects a real stall.
type InterventionConfig struct {
	// JudgeEnabled turns the cascade off entirely. Disabled reproduces the
	// pre-judge behaviour: a tripped counter acts immediately.
	JudgeEnabled *bool `yaml:"judge_enabled"`
	// JudgeBackoff multiplies a signal's observed value to set its new floor
	// when the judge declines to intervene, so the same question is not asked
	// again every turn. Optional: zero means defaultJudgeBackoff.
	JudgeBackoff float64 `yaml:"judge_backoff"`
	// JudgeTimeoutSeconds bounds the synchronous judge call. Optional: zero
	// means defaultJudgeTimeout.
	JudgeTimeoutSeconds int `yaml:"judge_timeout_seconds"`
	// JudgeModel pins the judge to a catalog model id. Empty selects the
	// cheapest configured model.
	JudgeModel string `yaml:"judge_model"`
}

type LSPConfig struct {
	Command    []string `yaml:"command"`
	Extensions []string `yaml:"extensions"`
	LanguageID string   `yaml:"language_id"`
}

type ProviderConfig struct {
	APIKey  string   `yaml:"api_key"`
	BaseURL string   `yaml:"base_url"`
	Keys    []string `yaml:"api_keys"`
}

type MCPConfig struct {
	Transport  string            `yaml:"transport"`
	URL        string            `yaml:"url"`
	Command    []string          `yaml:"command"`
	AuthHeader string            `yaml:"auth_header"`
	Headers    map[string]string `yaml:"headers"`
	Env        map[string]string `yaml:"env"`
	Squire     bool              `yaml:"squire"`
}

type RouterConfig struct {
	LambdaCost          float64  `yaml:"lambda_cost"`
	FrontierFloorPhases []string `yaml:"frontier_floor_phases"`
	DisableSwitch       bool     `yaml:"disable_switch"`
	DefaultModel        string   `yaml:"default_model"`
}

type BudgetConfig struct {
	SessionUSD         float64 `yaml:"session_usd"`
	JobDefaultFraction float64 `yaml:"job_default_fraction"`
	// MinReviewUSD is the floor budget an independent review worker receives
	// when a fraction of the parent budget would be smaller. A reviewer has to
	// read the whole diff before it can say anything, so a floor below the cost
	// of one turn guarantees it exhausts without returning a verdict.
	// Optional: zero means the built-in default (defaultMinReviewUSD).
	MinReviewUSD float64 `yaml:"min_review_usd"`
	// SessionTokens caps the novel (non-cached) tokens a session may consume.
	// Optional: zero means the built-in default (defaultSessionTokens).
	SessionTokens int `yaml:"session_tokens"`
}

// defaultSessionTokens is the fallback cap on novel (non-cached) tokens per
// session when session_tokens is not set. It is deliberately generous: the
// dollar budget is the real cost guardrail, and cache re-reads are excluded
// from this count.
const defaultSessionTokens = 4_000_000

// defaultMinReviewUSD is the fallback review-worker budget floor. It is sized
// at roughly three turns of a frontier model reading a large diff; a smaller
// floor makes budget exhaustion the reviewer's normal outcome.
const defaultMinReviewUSD = 2.0

// Judge defaults. The backoff is multiplicative rather than additive because
// the right threshold varies by task and is not known in advance: 1.5x
// converges on a session's real exploration depth in a logarithmic number of
// judge calls instead of a linear one.
const (
	defaultJudgeBackoff = 1.5
	defaultJudgeTimeout = 20 * time.Second
)

// SessionTokenLimit returns the configured per-session token cap, or the
// default when unset/zero.
func (b BudgetConfig) SessionTokenLimit() int {
	if b.SessionTokens <= 0 {
		return defaultSessionTokens
	}
	return b.SessionTokens
}

// ReviewFloorUSD returns the configured review-worker budget floor, or the
// default when unset/zero.
func (b BudgetConfig) ReviewFloorUSD() float64 {
	if b.MinReviewUSD <= 0 {
		return defaultMinReviewUSD
	}
	return b.MinReviewUSD
}

// Enabled reports whether the intervention judge runs. It defaults to true, so
// an omitted interventions block still gets the cascade.
func (i InterventionConfig) Enabled() bool {
	return i.JudgeEnabled == nil || *i.JudgeEnabled
}

// Backoff returns the configured judge backoff multiplier, or the default when
// unset/zero.
func (i InterventionConfig) Backoff() float64 {
	if i.JudgeBackoff <= 0 {
		return defaultJudgeBackoff
	}
	return i.JudgeBackoff
}

// JudgeTimeout returns the configured judge call timeout, or the default when
// unset/zero.
func (i InterventionConfig) JudgeTimeout() time.Duration {
	if i.JudgeTimeoutSeconds <= 0 {
		return defaultJudgeTimeout
	}
	return time.Duration(i.JudgeTimeoutSeconds) * time.Second
}

type TelemetryConfig struct {
	OTLPEndpoint string `yaml:"otlp_endpoint"`
}
type WebSearchConfig struct {
	Provider string `yaml:"provider"`
	APIKey   string `yaml:"api_key"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen: "127.0.0.1:7433", WorkspaceRoot: filepath.Join(home, "src"), Database: ".orrery/orrery.db",
		Providers: map[string]ProviderConfig{}, MCP: map[string]MCPConfig{},
		LSP:    map[string]LSPConfig{},
		Router: RouterConfig{LambdaCost: .35, FrontierFloorPhases: []string{"plan", "diagnose"}},
		Budget: BudgetConfig{SessionUSD: 25, JobDefaultFraction: .2, MinReviewUSD: defaultMinReviewUSD},
	}
}

func Load(path string) (Config, error) {
	return LoadWithEnv(path, nil)
}

// LoadWithEnv resolves !env references from overrides before the process
// environment. It lets a trusted local supervisor rotate credentials without
// persisting secret values or mutating process-global environment state.
func LoadWithEnv(path string, overrides map[string]string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if cfg.Listen == "" || cfg.Database == "" {
		return cfg, errors.New("config: listen and database must not be empty")
	}
	cfg.WorkspaceRoot = expandHome(cfg.WorkspaceRoot)
	cfg.Database = expandHome(cfg.Database)
	if cfg.Router.LambdaCost < 0 {
		return cfg, errors.New("config: router.lambda_cost must be non-negative")
	}
	if cfg.Budget.SessionUSD <= 0 || cfg.Budget.JobDefaultFraction <= 0 || cfg.Budget.JobDefaultFraction > 1 {
		return cfg, errors.New("config: invalid budget")
	}
	if cfg.Budget.SessionTokens < 0 {
		return cfg, errors.New("config: budget.session_tokens must be non-negative")
	}
	if cfg.Budget.MinReviewUSD < 0 {
		return cfg, errors.New("config: budget.min_review_usd must be non-negative")
	}
	// A backoff at or below 1 would set a floor no higher than the value that
	// just tripped, so the judge would be re-asked every turn forever.
	if cfg.Interventions.JudgeBackoff != 0 && cfg.Interventions.JudgeBackoff <= 1 {
		return cfg, errors.New("config: interventions.judge_backoff must be greater than 1")
	}
	if cfg.Interventions.JudgeTimeoutSeconds < 0 {
		return cfg, errors.New("config: interventions.judge_timeout_seconds must be non-negative")
	}
	for name, server := range cfg.LSP {
		if strings.TrimSpace(name) == "" || len(server.Command) == 0 || strings.TrimSpace(server.Command[0]) == "" {
			return cfg, fmt.Errorf("config: lsp %q requires a command", name)
		}
		if len(server.Extensions) == 0 {
			return cfg, fmt.Errorf("config: lsp %q requires extensions", name)
		}
		for i, ext := range server.Extensions {
			if !strings.HasPrefix(ext, ".") {
				return cfg, fmt.Errorf("config: lsp %q extension %q must start with a dot", name, ext)
			}
			server.Extensions[i] = strings.ToLower(ext)
		}
		cfg.LSP[name] = server
	}
	if err := resolveSecrets(&cfg, overrides); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func resolveSecrets(cfg *Config, overrides map[string]string) error {
	resolve := func(v *string) error {
		if strings.HasPrefix(*v, "!env ") {
			name := strings.TrimSpace(strings.TrimPrefix(*v, "!env "))
			if name == "" {
				return errors.New("secret environment variable name is empty")
			}
			value, ok := overrides[name]
			if !ok {
				value, ok = os.LookupEnv(name)
			}
			if !ok || strings.TrimSpace(value) == "" {
				return fmt.Errorf("secret environment variable %s is not set", name)
			}
			*v = strings.TrimSpace(value)
			return nil
		}
		if !strings.HasPrefix(*v, "!cmd ") {
			return nil
		}
		ctx, cancel := timeContext()
		defer cancel()
		out, err := exec.CommandContext(ctx, "sh", "-c", strings.TrimPrefix(*v, "!cmd ")).Output()
		if err != nil {
			return fmt.Errorf("secret command: %w", err)
		}
		*v = strings.TrimSpace(string(out))
		return nil
	}
	for name, p := range cfg.Providers {
		if err := resolve(&p.APIKey); err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
		for i := range p.Keys {
			if err := resolve(&p.Keys[i]); err != nil {
				return fmt.Errorf("provider %s key: %w", name, err)
			}
		}
		cfg.Providers[name] = p
	}
	for name, m := range cfg.MCP {
		if err := resolve(&m.AuthHeader); err != nil {
			return fmt.Errorf("mcp %s: %w", name, err)
		}
		for k, v := range m.Headers {
			if err := resolve(&v); err != nil {
				return err
			}
			m.Headers[k] = v
		}
		for k, v := range m.Env {
			if err := resolve(&v); err != nil {
				return fmt.Errorf("mcp %s env %s: %w", name, k, err)
			}
			m.Env[k] = v
		}
		cfg.MCP[name] = m
	}
	if err := resolve(&cfg.WebSearch.APIKey); err != nil {
		return fmt.Errorf("web_search: %w", err)
	}
	return nil
}

func timeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
