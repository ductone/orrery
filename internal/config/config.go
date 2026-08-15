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
		Router: RouterConfig{LambdaCost: .35, FrontierFloorPhases: []string{"plan", "diagnose", "review"}},
		Budget: BudgetConfig{SessionUSD: 25, JobDefaultFraction: .2},
	}
}

func Load(path string) (Config, error) {
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
	if err := resolveSecrets(&cfg); err != nil {
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

func resolveSecrets(cfg *Config) error {
	resolve := func(v *string) error {
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
