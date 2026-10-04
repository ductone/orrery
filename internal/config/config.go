package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/model"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string                    `yaml:"listen"`
	WorkspaceRoot string                    `yaml:"workspace_root"`
	Database      string                    `yaml:"database"`
	Providers     map[string]ProviderConfig `yaml:"providers"`
	Models        []ModelConfig             `yaml:"models"`
	MCP           map[string]MCPConfig      `yaml:"mcp"`
	Router        RouterConfig              `yaml:"router"`
	Budget        BudgetConfig              `yaml:"budget"`
	Telemetry     TelemetryConfig           `yaml:"telemetry"`
	WebSearch     WebSearchConfig           `yaml:"web_search"`
	Instructions  []string                  `yaml:"instructions"`
	LSP           map[string]LSPConfig      `yaml:"lsp"`
	Interventions InterventionConfig        `yaml:"interventions"`
	Memory        MemoryConfig              `yaml:"memory"`
	Jev           JevConfig                 `yaml:"jev"`
}

// JevConfig enables shadow observations from TypeSafe's Jev classifier. Shadow
// answers are recorded next to the harness's own decision and never change
// behaviour. Every listed site sends session content (task text, tool calls,
// diffs) to TypeSafe, so nothing runs unless sites are named explicitly.
type JevConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	// Shadow lists the decision sites to observe; see JevShadowSites.
	Shadow []string `yaml:"shadow"`
	// SearchRanking adds an intent parameter to search. When the model passes
	// one, matching files are ranked by Jev, which receives each file's path
	// and first matching lines. Unlike shadow sites this changes behaviour.
	SearchRanking bool `yaml:"search_ranking"`
	// Review lets Jev triage review diffs, size reviewers, downgrade findings
	// that are not correctness bugs, decide when read-only workers have
	// converged, and accept low-risk parts of an inconclusive review. It sends
	// changed files' patches and reviewer findings to TypeSafe.
	Review bool `yaml:"review"`
	// Routing lets Jev choose the phase of a turn that starts with a new user
	// message, instead of always routing it as planning. It sends the message,
	// the task, and the todo plan to TypeSafe once per user message.
	Routing bool `yaml:"routing"`
	// TimeoutSeconds bounds each asynchronous call. Optional: zero means
	// defaultJevTimeout.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// JevShadowSites are the decision sites that can be shadowed. "difficulty"
// is still accepted so existing configs load, but it is no longer asked: it
// showed no signal.
var JevShadowSites = []string{"stall_judge", "phase", "difficulty", "review"}

const defaultJevTimeout = 5 * time.Second

// Shadows reports whether a site is shadowed.
func (j JevConfig) Shadows(site string) bool {
	return j.APIKey != "" && slices.Contains(j.Shadow, site)
}

// Timeout returns the configured per-call timeout, or the default.
func (j JevConfig) Timeout() time.Duration {
	if j.TimeoutSeconds <= 0 {
		return defaultJevTimeout
	}
	return time.Duration(j.TimeoutSeconds) * time.Second
}

// MemoryConfig governs the durable, workspace-scoped memory layer described
// in docs/proposals/memory.md. Enabled, shadow, inject, and auto-commit are
// independently gated: memory can be derived and measured in shadow mode
// without ever being injected into a prompt or committed automatically.
// Numeric limits are clamped in code to safe ceilings/floors; RetainDays of 0
// means records never expire automatically.
type MemoryConfig struct {
	// Enabled is the master opt-in. False (the default) means no memory
	// derivation, retrieval, or storage happens at all.
	Enabled bool `yaml:"enabled"`
	// Shadow retrieves/derives candidate records and records observations
	// without injecting them into a prompt or changing behaviour.
	Shadow bool `yaml:"shadow"`
	// Inject allows selected memory to be added to the assembled prompt. It is
	// independent of Shadow so injection can be opted into separately after
	// shadow evaluation.
	Inject bool `yaml:"inject"`
	// AutoCommit allows model-derived candidates to become active records
	// without explicit user confirmation. Suggestions stay pending otherwise.
	AutoCommit bool `yaml:"auto_commit"`
	// MaxRecords caps how many memory records may be selected/pinned at once.
	// Optional: zero/negative means defaultMemoryMaxRecords.
	MaxRecords int `yaml:"max_records"`
	// MaxTokens caps the estimated token cost of the pinned memory set.
	// Optional: zero/negative means defaultMemoryMaxTokens.
	MaxTokens int `yaml:"max_tokens"`
	// MaxRecordBytes caps the size of a single memory record's text.
	// Optional: zero/negative means defaultMemoryMaxRecordBytes.
	MaxRecordBytes int `yaml:"max_record_bytes"`
	// RetainDays expires records older than this many days. Zero means no
	// automatic expiry; this is a meaningful value, not an "unset" sentinel.
	RetainDays int `yaml:"retain_days"`
	// Jev configures the bounded Jev policy signals used for memory selection
	// and compaction-benefit estimation. It reuses JevConfig's credentials,
	// base URL, and model; it does not add a separate credential path.
	Jev MemoryJevConfig `yaml:"jev"`
}

// MemoryJevConfig gates the memory-specific Jev decision sites independently
// of JevConfig.Shadow and of each other. Selection and CompactionBenefit are
// separate shadow/live policy switches so each can be promoted on its own
// evidence. Credentials come from JevConfig; this type adds none.
type MemoryJevConfig struct {
	// Selection gates the memory_select shadow/live policy.
	Selection bool `yaml:"selection"`
	// CompactionBenefit gates the compaction_benefit shadow/live policy.
	CompactionBenefit bool `yaml:"compaction_benefit"`
	// Timeout bounds each bounded Jev batch call. Optional: zero means
	// defaultMemoryJevTimeout.
	Timeout *time.Duration `yaml:"timeout,omitempty"`
	// MaxCandidates caps how many candidates one Jev batch may rank.
	// Optional: zero/negative means defaultMemoryMaxCandidates.
	MaxCandidates int `yaml:"max_candidates"`
}

// Memory defaults and clamp ceilings/floors. Ceilings bound a batch's cost and
// a single record's size; the record/candidate floor of 1 keeps a positive
// explicit setting meaningful instead of silently disabling selection.
const (
	defaultMemoryMaxRecords     = 8
	defaultMemoryMaxTokens      = 1200
	defaultMemoryMaxRecordBytes = 2048
	defaultMemoryMaxCandidates  = 12
	defaultMemoryJevTimeout     = 250 * time.Millisecond
	maxMemoryMaxRecords         = 64
	maxMemoryMaxTokens          = 20_000
	maxMemoryMaxRecordBytes     = 32_768
	maxMemoryMaxCandidates      = 64
	maxMemoryJevTimeout         = 10 * time.Second
	minMemoryClampedLimit       = 1
	// maxMemoryRetainDays bounds positive retention windows to keep expiry math
	// (converting to a future timestamp) safe from overflow/degenerate dates.
	// Zero is handled separately and always means no expiry; it is never
	// clamped to this ceiling.
	maxMemoryRetainDays = 3650
)

// Records returns the configured max memory record count, clamped to
// [1, maxMemoryMaxRecords]; zero/negative selects the default.
func (m MemoryConfig) Records() int {
	return clampInt(m.MaxRecords, defaultMemoryMaxRecords, minMemoryClampedLimit, maxMemoryMaxRecords)
}

// Tokens returns the configured max memory token budget, clamped to
// [1, maxMemoryMaxTokens]; zero/negative selects the default.
func (m MemoryConfig) Tokens() int {
	return clampInt(m.MaxTokens, defaultMemoryMaxTokens, minMemoryClampedLimit, maxMemoryMaxTokens)
}

// RecordBytes returns the configured max record size in bytes, clamped to
// [1, maxMemoryMaxRecordBytes]; zero/negative selects the default.
func (m MemoryConfig) RecordBytes() int {
	return clampInt(m.MaxRecordBytes, defaultMemoryMaxRecordBytes, minMemoryClampedLimit, maxMemoryMaxRecordBytes)
}

// ExpiresAfterDays reports the configured retention window. Zero means
// records never expire automatically; this is not clamped away. Positive
// values are clamped to [1, maxMemoryRetainDays] to keep expiry math (adding
// the window to a timestamp) safe from overflow/degenerate dates.
func (m MemoryConfig) ExpiresAfterDays() int {
	if m.RetainDays <= 0 {
		return 0
	}
	if m.RetainDays > maxMemoryRetainDays {
		return maxMemoryRetainDays
	}
	return m.RetainDays
}

// MaxCandidates returns the configured max Jev batch candidate count, clamped
// to [1, maxMemoryMaxCandidates]; zero/negative selects the default.
func (m MemoryJevConfig) MaxCandidatesLimit() int {
	return clampInt(m.MaxCandidates, defaultMemoryMaxCandidates, minMemoryClampedLimit, maxMemoryMaxCandidates)
}

// CallTimeout returns the configured per-batch Jev timeout, clamped to
// (0, maxMemoryJevTimeout]; unset or non-positive selects the default.
func (m MemoryJevConfig) CallTimeout() time.Duration {
	if m.Timeout == nil || *m.Timeout <= 0 {
		return defaultMemoryJevTimeout
	}
	if *m.Timeout > maxMemoryJevTimeout {
		return maxMemoryJevTimeout
	}
	return *m.Timeout
}

// clampInt returns value clamped to [lo, hi], or def when value is <= 0.
func clampInt(value, def, lo, hi int) int {
	if value <= 0 {
		value = def
	}
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
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

// ModelConfig is an optional field-level override for a catalog model, built
// in or discovered. Pointer fields distinguish omission from an explicit zero
// or false value. The id names one route ("ramp/grok-4.7"), or, without a
// provider prefix, a model ("grok-4.7") and so every route serving it. An
// entry for an unknown route id adds a model when it supplies family, tier,
// context_window, max_output, and input and output pricing.
type ModelConfig struct {
	ID string `yaml:"id"`
	// Disabled removes the model from the catalog.
	Disabled      *bool               `yaml:"disabled,omitempty"`
	Family        *model.Family       `yaml:"family,omitempty"`
	Tier          *model.Tier         `yaml:"tier,omitempty"`
	Inputs        *[]model.Modality   `yaml:"inputs,omitempty"`
	ContextWindow *int                `yaml:"context_window,omitempty"`
	MaxOutput     *int                `yaml:"max_output,omitempty"`
	Pricing       *ModelPricingConfig `yaml:"pricing,omitempty"`
	Effort        *[]model.Effort     `yaml:"effort,omitempty"`
	Compat        *ModelCompatConfig  `yaml:"compat,omitempty"`
	EditDialect   *model.EditDialect  `yaml:"edit_dialect,omitempty"`
}

type ModelPricingConfig struct {
	Input      *float64                    `yaml:"input,omitempty"`
	Output     *float64                    `yaml:"output,omitempty"`
	CacheRead  *float64                    `yaml:"cache_read,omitempty"`
	CacheWrite *float64                    `yaml:"cache_write,omitempty"`
	Thresholds *[]ModelThresholdRateConfig `yaml:"thresholds,omitempty"`
}

type ModelThresholdRateConfig struct {
	AboveTokens int     `yaml:"above_tokens"`
	Input       float64 `yaml:"input"`
	Output      float64 `yaml:"output"`
	CacheRead   float64 `yaml:"cache_read"`
	CacheWrite  float64 `yaml:"cache_write"`
}

type ModelCompatConfig struct {
	MaxTokensField          *string                  `yaml:"max_tokens_field,omitempty"`
	SupportsToolChoice      *bool                    `yaml:"supports_tool_choice,omitempty"`
	SupportsReasoningEffort *bool                    `yaml:"supports_reasoning_effort,omitempty"`
	EffortWireMap           *map[model.Effort]string `yaml:"effort_wire_map,omitempty"`
	RequiresReasoningEcho   *bool                    `yaml:"requires_reasoning_echo,omitempty"`
	RequiresAssistantText   *bool                    `yaml:"requires_assistant_text,omitempty"`
	SupportsStrictTools     *bool                    `yaml:"supports_strict_tools,omitempty"`
	StreamIdleTimeout       *time.Duration           `yaml:"stream_idle_timeout,omitempty"`
	SystemPromptStyle       *model.SystemStyle       `yaml:"system_prompt_style,omitempty"`
	CacheControl            *bool                    `yaml:"cache_control,omitempty"`
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
	// ProviderKeys names upstream providers for which the account has its own
	// key on a router (Ramp Router's "Provider keys"), so models served only
	// through them are usable. Without one, such models are left out.
	ProviderKeys []string `yaml:"provider_keys"`
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

// Home is Orrery's user-level directory: the default config, database, and
// logs live here. $ORRERY_HOME overrides ~/.orrery.
func Home() string {
	if dir := strings.TrimSpace(os.Getenv("ORRERY_HOME")); dir != "" {
		return expandHome(dir)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".orrery")
}

// LocalConfig is the per-directory override consulted before the user config.
const LocalConfig = "orrery.yaml"

// Resolve finds the configuration file. An explicit path (from --config) or
// $ORRERY_CONFIG must exist. Otherwise ./orrery.yaml overrides
// <Home>/orrery.yaml; found is false when neither exists, and the caller runs
// on defaults. searched lists the candidates for error messages.
func Resolve(explicit string) (path string, found bool, searched []string, err error) {
	for _, pinned := range []struct{ path, source string }{{explicit, "--config"}, {os.Getenv("ORRERY_CONFIG"), "$ORRERY_CONFIG"}} {
		if pinned.path == "" {
			continue
		}
		if _, err := os.Stat(pinned.path); err != nil {
			return "", false, []string{pinned.path}, fmt.Errorf("config %s (from %s): %w", pinned.path, pinned.source, err)
		}
		return pinned.path, true, []string{pinned.path}, nil
	}
	for _, candidate := range []string{LocalConfig, filepath.Join(Home(), "orrery.yaml")} {
		searched = append(searched, candidate)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, searched, nil
		}
	}
	return "", false, searched, nil
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen: "127.0.0.1:7433", WorkspaceRoot: filepath.Join(home, "src"), Database: filepath.Join(Home(), "orrery.db"),
		Providers: map[string]ProviderConfig{}, MCP: map[string]MCPConfig{},
		LSP:    map[string]LSPConfig{},
		Router: RouterConfig{LambdaCost: .35, FrontierFloorPhases: []string{"plan", "diagnose"}},
		Budget: BudgetConfig{SessionUSD: 25, JobDefaultFraction: .2, MinReviewUSD: defaultMinReviewUSD},
		Memory: MemoryConfig{Shadow: true},
	}
}

func Load(path string) (Config, error) {
	return LoadWithEnv(path, nil)
}

// LoadWithEnv resolves !env references from overrides before the process
// environment. It lets a trusted local supervisor rotate credentials without
// persisting secret values or mutating process-global environment state.
func LoadWithEnv(path string, overrides map[string]string) (Config, error) {
	return load(path, overrides, true)
}

// LoadUnresolved loads and validates a config without resolving secrets, for
// commands that only read the store and must not need provider keys (or run
// secret commands) to do so. Secret fields keep their !env/!cmd references.
func LoadUnresolved(path string) (Config, error) {
	return load(path, nil, false)
}

func load(path string, overrides map[string]string, secrets bool) (Config, error) {
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
	// Relative paths belong to the config file, not to wherever Orrery was
	// started, so a user-level config does not scatter databases.
	base := filepath.Dir(path)
	cfg.WorkspaceRoot = relativeTo(base, expandHome(cfg.WorkspaceRoot))
	cfg.Database = relativeTo(base, expandHome(cfg.Database))
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
	for _, site := range cfg.Jev.Shadow {
		if !slices.Contains(JevShadowSites, site) {
			return cfg, fmt.Errorf("config: jev.shadow site %q is unknown; valid sites are %s", site, strings.Join(JevShadowSites, ", "))
		}
	}
	if (len(cfg.Jev.Shadow) > 0 || cfg.Jev.SearchRanking || cfg.Jev.Review || cfg.Jev.Routing) && cfg.Jev.APIKey == "" {
		return cfg, errors.New("config: jev.shadow, jev.search_ranking, jev.review, and jev.routing require jev.api_key")
	}
	if cfg.Jev.TimeoutSeconds < 0 {
		return cfg, errors.New("config: jev.timeout_seconds must be non-negative")
	}
	if cfg.Memory.RetainDays < 0 {
		return cfg, errors.New("config: memory.retain_days must be non-negative")
	}
	if cfg.Memory.Jev.Timeout != nil && *cfg.Memory.Jev.Timeout < 0 {
		return cfg, errors.New("config: memory.jev.timeout must be non-negative")
	}
	// Note: memory.jev.selection/compaction_benefit without jev.api_key is not a
	// validation error. Per docs/proposals/memory.md, missing credentials (like
	// low confidence, timeout, or malformed answers) must fall back to
	// deterministic lexical/recency ranking and the current compaction policy
	// at runtime rather than block startup.
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
	if err := validateModels(cfg.Models); err != nil {
		return cfg, fmt.Errorf("config: models: %w", err)
	}
	if !secrets {
		return cfg, nil
	}
	if err := resolveSecrets(&cfg, overrides); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func validateModels(models []ModelConfig) error {
	seen := make(map[string]struct{}, len(models))
	for i, m := range models {
		if strings.TrimSpace(m.ID) != m.ID || m.ID == "" || strings.HasPrefix(m.ID, "/") || strings.HasSuffix(m.ID, "/") {
			return fmt.Errorf("entry %d has invalid id %q (want provider/model, or a model name)", i, m.ID)
		}
		if _, ok := seen[m.ID]; ok {
			return fmt.Errorf("duplicate id %q", m.ID)
		}
		seen[m.ID] = struct{}{}
		// Families are open-ended (discovered models bring new vendors), but
		// must be a simple lowercase name.
		if m.Family != nil && !validFamily(string(*m.Family)) {
			return fmt.Errorf("%s: invalid family %q (want a lowercase name such as anthropic or qwen)", m.ID, *m.Family)
		}
		if m.Tier != nil && !oneOf(*m.Tier, model.Frontier, model.Efficient, model.Tiny) {
			return fmt.Errorf("%s: unknown tier %q", m.ID, *m.Tier)
		}
		if (m.ContextWindow != nil && *m.ContextWindow <= 0) || (m.MaxOutput != nil && *m.MaxOutput <= 0) {
			return fmt.Errorf("%s: context_window and max_output must be positive", m.ID)
		}
		if m.Inputs != nil {
			for _, v := range *m.Inputs {
				if !oneOf(v, model.Text, model.Image) {
					return fmt.Errorf("%s: unknown input %q", m.ID, v)
				}
			}
		}
		if m.Effort != nil {
			for _, v := range *m.Effort {
				if !oneOf(v, model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortXHigh) {
					return fmt.Errorf("%s: unknown effort %q", m.ID, v)
				}
			}
		}
		if m.EditDialect != nil && !oneOf(*m.EditDialect, model.HashlineJSON, model.HashlineXML, model.HashlineContextual, model.TextAnchor) {
			return fmt.Errorf("%s: unknown edit_dialect %q", m.ID, *m.EditDialect)
		}
		if m.Compat != nil {
			if m.Compat.SystemPromptStyle != nil && !oneOf(*m.Compat.SystemPromptStyle, model.SystemTopLevel, model.SystemFirstTurn) {
				return fmt.Errorf("%s: unknown system_prompt_style %q", m.ID, *m.Compat.SystemPromptStyle)
			}
			if m.Compat.StreamIdleTimeout != nil && *m.Compat.StreamIdleTimeout < 0 {
				return fmt.Errorf("%s: stream_idle_timeout must be non-negative", m.ID)
			}
		}
		if m.Pricing != nil {
			for _, v := range []*float64{m.Pricing.Input, m.Pricing.Output, m.Pricing.CacheRead, m.Pricing.CacheWrite} {
				if v != nil && *v < 0 {
					return fmt.Errorf("%s: pricing must be non-negative", m.ID)
				}
			}
			if m.Pricing.Thresholds != nil {
				for _, t := range *m.Pricing.Thresholds {
					if t.AboveTokens <= 0 || t.Input < 0 || t.Output < 0 || t.CacheRead < 0 || t.CacheWrite < 0 {
						return fmt.Errorf("%s: invalid pricing threshold", m.ID)
					}
				}
			}
		}
	}
	return nil
}

func validFamily(f string) bool {
	if f == "" {
		return false
	}
	for _, r := range f {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func oneOf[T comparable](value T, allowed ...T) bool {
	return slices.Contains(allowed, value)
}

func relativeTo(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
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
	if err := resolve(&cfg.Jev.APIKey); err != nil {
		return fmt.Errorf("jev: %w", err)
	}
	return nil
}

func timeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
