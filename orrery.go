// Package orrery embeds the Orrery agent engine in a Go program.
//
// A host builds a config.Config (config.Default, config.Load, or by hand),
// installs the model catalog, and constructs an Engine with New:
//
//	cfg := config.Default()
//	orrery.InstallModelCatalog(ctx, cfg)
//	st, err := orrery.OpenSQLite(cfg.Database)
//	...
//	eng, err := orrery.New(orrery.Options{Config: cfg, Store: st})
//	...
//	defer eng.Close()
//	result, err := eng.Run(ctx, agentproto.TaskRequest{
//		Spec:      "...",
//		Workspace: agentproto.Workspace{Path: dir},
//	}, func(ev agentproto.AgentEvent) {})
//
// Engine methods start, continue, cancel, checkpoint and fork sessions; the
// agentproto package holds the request, result and event types they use.
package orrery

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ductone/orrery/config"
	"github.com/ductone/orrery/internal/catalog"
	"github.com/ductone/orrery/internal/classify"
	"github.com/ductone/orrery/internal/core"
	"github.com/ductone/orrery/internal/mcp"
	"github.com/ductone/orrery/internal/model"
	"github.com/ductone/orrery/internal/provider"
	"github.com/ductone/orrery/internal/store"
	"github.com/ductone/orrery/storage"
)

type (
	// Engine runs agent sessions.
	Engine = core.Engine
	// EmitFunc receives the events a session produces.
	EmitFunc = core.EmitFunc
	// StartInfo describes a session started or continued by an integration.
	StartInfo = core.StartInfo
	// SessionOptions carries integration identity for StartIntegrated.
	SessionOptions = core.SessionOptions
	// ReviewOutcome is the result of Engine.ReviewChange.
	ReviewOutcome = core.ReviewOutcome
	// WorkspaceFunc resolves a session's workspace; see Engine.UseWorkspaces.
	WorkspaceFunc = core.WorkspaceFunc
	// Providers routes model calls to the configured providers.
	Providers = provider.Registry
	// MCPManager runs the configured MCP servers.
	MCPManager = mcp.Manager
	// Classifier answers the engine's classifier-backed decisions; see
	// Engine.UseClassifier.
	Classifier = classify.Classifier
	// SQLiteStore is the SQLite-backed session store.
	SQLiteStore = store.Store
)

// InstallModelCatalog builds the model catalog for cfg (models discovered
// from providers that list them, the built-in catalog, then cfg.Models
// overrides) and installs it process-wide. Call it before NewProviders and
// New. Discovery caches listings under config.Home().
func InstallModelCatalog(ctx context.Context, cfg config.Config) {
	res := catalog.Build(ctx, cfg, catalog.Dir(), nil)
	model.Install(res.Models)
	for _, d := range res.Discoveries {
		attrs := []any{"provider", d.Provider, "source", d.Source, "listed", d.Listed, "usable", len(d.Models)}
		if d.Error != "" {
			attrs = append(attrs, "error", d.Error)
		}
		slog.Info("model discovery", attrs...)
	}
	for _, w := range res.Warnings {
		slog.Warn("model override not applied", "warning", w)
	}
	slog.Info("model catalog", "models", len(res.Models), "discovered", res.Discovered, "overrides", res.Overridden, "disabled", res.Disabled)
}

// NewProviders returns the model providers configured in cfg. A model a
// provider refuses for this account is recorded so later discovery leaves it
// out.
func NewProviders(cfg config.Config) *Providers {
	p := provider.New(cfg)
	p.SetRefusalHook(func(id, reason string) {
		if err := catalog.MarkUnavailable(catalog.Dir(), id, reason); err != nil {
			slog.Warn("record refused model", "model", id, "error", err)
		}
		slog.Warn("model refused by provider; leaving it out", "model", id)
	})
	return p
}

// NewMCP starts the MCP servers configured in cfg, writing their logs under
// logDir.
func NewMCP(ctx context.Context, cfg config.Config, logDir string) (*MCPManager, error) {
	return mcp.New(ctx, cfg.MCP, logDir)
}

// OpenSQLite opens (creating if needed) a SQLite session store.
func OpenSQLite(path string) (*SQLiteStore, error) {
	return store.Open(path)
}

// Options configures New.
type Options struct {
	Config config.Config
	// Store holds sessions, transcripts, events, jobs and memory: OpenSQLite,
	// or any storage.Store implementation. Required.
	Store storage.Store
	// Providers defaults to NewProviders(Config).
	Providers *Providers
	// MCP is optional; without it the engine has no MCP tools.
	MCP *MCPManager
}

// New constructs an Engine. Install the model catalog first.
func New(opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("orrery: a store is required")
	}
	p := opts.Providers
	if p == nil {
		p = NewProviders(opts.Config)
	}
	return core.New(opts.Config, opts.Store, p, opts.MCP), nil
}
