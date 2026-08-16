package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/ductone/orrey/internal/agentproto"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/core"
	orreval "github.com/ductone/orrey/internal/eval"
	"github.com/ductone/orrey/internal/mcp"
	"github.com/ductone/orrey/internal/provider"
	rpcserver "github.com/ductone/orrey/internal/rpc"
	"github.com/ductone/orrey/internal/store"
	"github.com/ductone/orrey/internal/telemetry"
	"github.com/ductone/orrey/internal/web"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var version = "dev"

type runtime struct {
	mu           sync.Mutex
	cfg          config.Config
	configPath   string
	store        *store.Store
	mcp          *mcp.Manager
	engine       *core.Engine
	shutdownOTel func(context.Context) error
	pending      atomic.Bool
	pendingEnv   map[string]string
}

func main() { os.Exit(realMain()) }
func realMain() int {
	global := flag.NewFlagSet("orrery", flag.ContinueOnError)
	configPath := global.String("config", "orrery.yaml", "configuration file")
	showVersion := global.Bool("version", false, "print version")
	if err := global.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	args := global.Args()
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rt, err := openRuntime(ctx, *configPath)
	if err != nil {
		slog.Error("startup", "error", err)
		return 2
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rt.close(c)
	}()
	switch cmd {
	case "serve":
		return serve(ctx, rt, args)
	case "run":
		return run(ctx, rt, args)
	case "export":
		return export(ctx, rt, args)
	case "eval", "benchmark":
		return evaluate(ctx, rt, args)
	case "rpc":
		return serveRPC(ctx, rt, rpcserver.Native)
	case "acp":
		return serveRPC(ctx, rt, rpcserver.ACP)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		return 2
	}
}

func serveRPC(ctx context.Context, rt *runtime, mode rpcserver.Mode) int {
	server := &rpcserver.Server{Engine: rt.engine, Mode: mode, Version: version}
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		slog.Error("stdio transport", "mode", mode, "error", err)
		return 1
	}
	return 0
}
func openRuntime(ctx context.Context, path string) (*runtime, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(cfg.Database)
	if err != nil {
		return nil, err
	}
	if err = s.MarkRunningInterrupted(ctx); err != nil {
		s.Close()
		return nil, err
	}
	shutdownOTel, err := telemetry.Setup(ctx, cfg.Telemetry.OTLPEndpoint)
	if err != nil {
		s.Close()
		return nil, err
	}
	mc, err := mcp.New(ctx, cfg.MCP, filepath.Join(".orrery", "logs"))
	if err != nil {
		s.Close()
		return nil, err
	}
	p := provider.New(cfg)
	e := core.New(cfg, s, p, mc)
	rt := &runtime{cfg: cfg, configPath: path, store: s, mcp: mc, engine: e, shutdownOTel: shutdownOTel}
	e.SetBoundaryHook(rt.phaseBoundary)
	return rt, nil
}
func serve(ctx context.Context, rt *runtime, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", rt.cfg.Listen, "listen address")
	viewListen := fs.String("view-listen", "", "optional read-only web listener")
	if fs.Parse(args) != nil {
		return 2
	}
	srv := web.New(*listen, rt.engine, version)
	srv.SetRuntimeReload(rt.queueReload)
	var view *web.Server
	if *viewListen != "" {
		view = web.NewView(*viewListen, rt.engine, version)
		go func() {
			if err := view.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("read-only server", "error", err)
			}
		}()
		slog.Info("Orrery read-only UI listening", "address", *viewListen)
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		if view != nil {
			_ = view.Shutdown(c)
		}
	}()
	slog.Info("Orrery listening", "address", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "error", err)
		return 1
	}
	return 0
}

func (rt *runtime) queueReload(env map[string]string) {
	rt.mu.Lock()
	rt.pendingEnv = make(map[string]string, len(env))
	for name, value := range env {
		rt.pendingEnv[name] = value
	}
	rt.pending.Store(true)
	rt.mu.Unlock()
}

func (rt *runtime) phaseBoundary(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.pending.Swap(false) {
		if rt.mcp != nil {
			return rt.mcp.PhaseBoundary(ctx)
		}
		return nil
	}
	nextCfg, err := config.LoadWithEnv(rt.configPath, rt.pendingEnv)
	if err != nil {
		rt.pending.Store(true)
		return fmt.Errorf("reload config: %w", err)
	}
	nextMCP, err := mcp.New(ctx, nextCfg.MCP, filepath.Join(".orrery", "logs"))
	if err != nil {
		rt.pending.Store(true)
		return fmt.Errorf("reload MCP: %w", err)
	}
	nextShutdownOTel, err := telemetry.Setup(ctx, nextCfg.Telemetry.OTLPEndpoint)
	if err != nil {
		_ = nextMCP.Close()
		rt.pending.Store(true)
		return fmt.Errorf("reload telemetry: %w", err)
	}
	oldMCP := rt.engine.ReplaceRuntime(nextCfg, provider.New(nextCfg), nextMCP)
	oldShutdownOTel := rt.shutdownOTel
	rt.cfg = nextCfg
	rt.pendingEnv = nil
	rt.mcp = nextMCP
	rt.shutdownOTel = nextShutdownOTel
	if oldMCP != nil {
		_ = oldMCP.Close()
	}
	if oldShutdownOTel != nil {
		_ = oldShutdownOTel(ctx)
	}
	slog.Info("Orrery runtime config reloaded at phase boundary")
	return nextMCP.PhaseBoundary(ctx)
}

func (rt *runtime) close(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var mcpErr, otelErr error
	if rt.mcp != nil {
		mcpErr = rt.mcp.Close()
	}
	if rt.shutdownOTel != nil {
		otelErr = rt.shutdownOTel(ctx)
	}
	return errors.Join(mcpErr, otelErr, rt.engine.Close(), rt.store.Close())
}
func run(ctx context.Context, rt *runtime, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	prompt := fs.String("p", "", "task prompt")
	workspace := fs.String("workspace", rt.cfg.WorkspaceRoot, "workspace path")
	budget := fs.Float64("budget", rt.cfg.Budget.SessionUSD, "maximum USD")
	maxTokens := fs.Int("max-tokens", 4_000_000, "maximum total input and output tokens")
	tier := fs.String("tier", "", "optional tier pin")
	if fs.Parse(args) != nil {
		return 2
	}
	if *prompt == "" {
		b, _ := os.ReadFile("/dev/stdin")
		*prompt = strings.TrimSpace(string(b))
	}
	req := agentproto.TaskRequest{Spec: *prompt, Budget: agentproto.Budget{MaxUSD: *budget, MaxTokens: *maxTokens, MaxWallClock: 2 * time.Hour, MaxDepth: 4}, Workspace: agentproto.Workspace{Path: *workspace, Isolation: "shared"}, Hints: agentproto.RoutingHints{TierPin: *tier}, Depth: 4}
	result, err := rt.engine.Run(ctx, req, func(ev agentproto.AgentEvent) {
		if ev.Type == "routing.decision" || ev.Type == "tool.started" {
			b, _ := json.Marshal(ev)
			fmt.Fprintln(os.Stderr, string(b))
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	switch result.Status {
	case agentproto.Pass:
		return 0
	case agentproto.BudgetExhausted:
		return 3
	case agentproto.Cancelled:
		return 130
	default:
		return 1
	}
}
func export(ctx context.Context, rt *runtime, args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	sinceArg := fs.String("since", "0", "RFC3339 timestamp or duration such as 24h")
	if fs.Parse(args) != nil {
		return 2
	}
	since, err := parseSince(*sinceArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	err = rt.store.ExportRouting(ctx, since, func(line []byte) error { _, err := os.Stdout.Write(append(line, '\n')); return err })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
func evaluate(ctx context.Context, rt *runtime, args []string) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	set := fs.String("set", "", "replay set JSONL")
	policy := fs.String("policy", "v1", "frontier-pinned, v1, or candidate")
	buildSession := fs.String("build-session", "", "build one replay JSONL row from a completed session")
	acceptance := fs.String("acceptance", "", "acceptance command for --build-session")
	baseline := fs.String("baseline", "", "optional prior benchmark report for regression comparison")
	output := fs.String("output", "", "optional path for the formatted benchmark report")
	minPassRatio := fs.Float64("min-pass-ratio", .97, "minimum pass-rate ratio versus --baseline")
	if fs.Parse(args) != nil {
		return 2
	}
	if *buildSession != "" {
		c, err := orreval.BuildCase(ctx, rt.store, *buildSession, *acceptance)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(c)
		return 0
	}
	if *set == "" {
		fmt.Fprintln(os.Stderr, "--set is required")
		return 2
	}
	cases, err := orreval.Load(*set)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	report, err := orreval.Run(ctx, rt.engine, *policy, cases)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *baseline != "" {
		prior, loadErr := orreval.LoadReport(*baseline)
		if loadErr != nil {
			fmt.Fprintln(os.Stderr, loadErr)
			return 1
		}
		comparison := orreval.Compare(report, prior, *minPassRatio)
		report.Comparison = &comparison
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *output != "" {
		if err := os.WriteFile(*output, append(encoded, '\n'), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	fmt.Println(string(encoded))
	if report.Comparison != nil && !report.Comparison.Passed {
		return 4
	}
	return 0
}
func parseSince(v string) (time.Time, error) {
	if v == "0" || v == "" {
		return time.Unix(0, 0), nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Parse(time.RFC3339, v)
}
func usage() {
	fmt.Fprintln(os.Stderr, `usage: orrery [--config path] <command>
commands:
  serve [--listen address]       run the web UI and HTTP/SSE transport
  run -p "task" [--workspace]    run one task; emit JSON TaskResult
  export [--since 24h]           emit routing records as JSONL
  eval --set tasks.jsonl         run a replay set
  benchmark --set cases.jsonl    run isolated engineering cases and compare trends`)
}
