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
	"fmt"
	"log/slog"
	"slices"

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
	// Profile is the domain part of the system prompt.
	Profile = core.Profile
	// ProfileSection is a titled list of rules in a Profile.
	ProfileSection = core.ProfileSection
	// Providers routes model calls to the configured providers.
	Providers = provider.Registry
	// MCPManager runs the configured MCP servers.
	MCPManager = mcp.Manager
	// Classifier answers the engine's classifier-backed decisions; see
	// Engine.UseClassifier.
	Classifier = classify.Classifier
	// Tool is a host-provided tool offered to the model; see Options.Tools.
	Tool = core.Tool
	// ToolCall is one invocation of a host Tool.
	ToolCall = core.ToolCall
	// InputHandler answers a session's request for input without a person; see
	// Options.InputHandler and AutoAnswer.
	InputHandler = core.InputHandler
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
	// Providers defaults to NewProviders(Config). Safe to share between
	// engines in one process, and cheaper than building one per engine:
	// it holds only credentials, the HTTP clients and the rate-limit and
	// refusal state of those credentials, all guarded for concurrent use, and
	// nothing per session or per engine. Sharing means that cooling of a key
	// or a refused model applies to every engine using it, which is right
	// when they share credentials; give engines that use different accounts
	// different registries. Do not call SetRefusalHook on a registry already
	// in use. The engine never closes it.
	Providers *Providers
	// MCP is optional; without it the engine has no MCP tools. Safe to share
	// between engines: the manager keeps no per-session state (a session ID
	// is only forwarded to servers configured as Squire bridges, and session
	// IDs are UUIDs), and its servers' clients serialize their own I/O. Two
	// caveats: tool definitions refresh at phase boundaries, so one engine's
	// boundary can change the tool list another engine's next request sees
	// (a prompt-cache concern, not a correctness one); and an MCP server that
	// is itself stateful per connection sees all engines as one client.
	// Engines that need isolated MCP servers, such as per-agent credentials,
	// need their own manager. The engine never closes it, and ReplaceRuntime
	// hands back the manager it replaced for the host to close.
	MCP *MCPManager
	// ScratchRoot is the directory holding session scratch directories
	// (ScratchRoot/<session id>). Empty means the process-wide default under
	// the OS temp directory, shared with every other engine that leaves it
	// empty; the engine then removes only the old scratch directories of
	// sessions its own Store shows as finished. A non-empty ScratchRoot is
	// owned by this engine alone, and it also removes unknown directories
	// there that are older than a day. Hosts running many engines in one
	// process should give each its own.
	ScratchRoot string
	// Logger receives the engine's and its tools' log output; nil means
	// slog.Default(). Give each engine its own, tagged with the agent, when
	// running many in one process.
	Logger *slog.Logger
	// Profile sets the domain part of the system prompt: who the agent is and
	// its guidance for the work. Nil means CodingProfile(); a host running a
	// non-coding agent passes GeneralProfile() or its own. The engine's
	// harness rules are always included.
	Profile *Profile
	// Tools are host-provided tools offered to the model alongside the
	// built-in ones. New fails on a tool whose name is empty, repeated, or a
	// built-in tool's. A host tool takes precedence over an MCP tool of the
	// same name.
	Tools []Tool
	// EnabledTools, when non-empty, limits the model to the named tools:
	// built-in, engine and MCP tools not listed are not offered. Host Tools
	// are always offered unless listed in DisabledTools.
	EnabledTools []string
	// DisabledTools names tools never offered to the model. Together with
	// EnabledTools and ToolFilter, a tool must pass all of them. Tools the
	// engine's own flow does not depend on can be removed freely; see the
	// README for the few that change behavior.
	DisabledTools []string
	// ToolFilter, when set, is called with each tool's name and reports
	// whether to offer it. It applies to all tools, host tools included.
	ToolFilter func(name string) bool
	// InputHandler, if set, is asked before a session pauses for a person: for
	// the model's questions, the dollar budget and the limits (see
	// agentproto.InputRequest.Kind). When it returns an answer and true, the
	// answer is applied as if a person had given it through Continue, and the
	// run goes on: Engine.Run returns the final outcome rather than
	// InputRequired. When it returns false, or an error (which is logged), the
	// session pauses as usual. Persistent agents with no person watching use
	// AutoAnswer. Nil means every pause waits for a person.
	InputHandler InputHandler
	// MaxAutoAnswers bounds how many answers InputHandler gives in one run
	// before the session pauses for a person regardless, so a handler that
	// agrees to everything cannot loop forever. Zero means 10.
	MaxAutoAnswers int
}

// CodingProfile is the default system prompt profile: an autonomous coding
// agent working in a checkout.
func CodingProfile() Profile { return core.CodingProfile() }

// GeneralProfile is a domain-neutral profile for a persistent assistant.
func GeneralProfile() Profile { return core.GeneralProfile() }

// New constructs an Engine. Install the model catalog first.
//
// The model catalog is process-global (InstallModelCatalog calls
// model.Install): install it once, before creating engines, not per engine;
// every engine in the process sees the same catalog, and installing it again
// replaces it for all of them. The classifier (Engine.UseClassifier) is per
// engine, so engines may share one implementation if it is safe for
// concurrent use. An Engine's Close releases only what the engine created,
// never Providers or MCP.
func New(opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("orrery: a store is required")
	}
	p := opts.Providers
	if p == nil {
		p = NewProviders(opts.Config)
	}
	var copts []core.Option
	if opts.ScratchRoot != "" {
		copts = append(copts, core.WithScratchRoot(opts.ScratchRoot))
	}
	if opts.Logger != nil {
		copts = append(copts, core.WithLogger(opts.Logger))
	}
	if opts.Profile != nil {
		copts = append(copts, core.WithProfile(*opts.Profile))
	}
	if err := core.ValidateTools(opts.Tools); err != nil {
		return nil, fmt.Errorf("orrery: %w", err)
	}
	if len(opts.Tools) > 0 {
		copts = append(copts, core.WithTools(opts.Tools...))
	}
	if len(opts.EnabledTools) > 0 || len(opts.DisabledTools) > 0 || opts.ToolFilter != nil {
		host := map[string]bool{}
		for _, t := range opts.Tools {
			host[t.Name] = true
		}
		copts = append(copts, core.WithToolFilter(func(name string) bool {
			if slices.Contains(opts.DisabledTools, name) {
				return false
			}
			if len(opts.EnabledTools) > 0 && !host[name] && !slices.Contains(opts.EnabledTools, name) {
				return false
			}
			return opts.ToolFilter == nil || opts.ToolFilter(name)
		}))
	}
	if opts.InputHandler != nil {
		copts = append(copts, core.WithInputHandler(opts.InputHandler))
	}
	if opts.MaxAutoAnswers > 0 {
		copts = append(copts, core.WithMaxAutoAnswers(opts.MaxAutoAnswers))
	}
	return core.New(opts.Config, opts.Store, p, opts.MCP, copts...), nil
}
