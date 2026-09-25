package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ductone/orrey/internal/tui"
)

// runTUI starts the terminal UI. Local mode embeds the engine; --server
// attaches to a running `orrery serve` and never opens the local store, so a
// TUI can share a session with the web UI without two processes owning one
// SQLite database.
func runTUI(ctx context.Context, configPath string, args []string) int {
	squireTask := os.Getenv("SQUIRE_TASK_ID")
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	sessionID := fs.String("session", "", "attach to this session id")
	server := fs.String("server", os.Getenv("ORRERY_SERVER"), "attach to a running `orrery serve` at this URL instead of embedding the engine")
	integration := fs.String("integration", "squire", "integration namespace for --external-id")
	externalID := fs.String("external-id", "", "bind to the session for this external identity, creating it on the first message (default $SQUIRE_TASK_ID)")
	incarnation := fs.String("incarnation", "", "external incarnation for --external-id")
	workspace := fs.String("workspace", "", "workspace for a new session (default: current directory)")
	budget := fs.Float64("budget", 0, "USD budget for a new session (default: config budget.session_usd)")
	tier := fs.String("tier", "", "optional tier pin for a new session")
	prompt := fs.String("p", "", "message to send on start; trailing arguments are joined as the message too")
	squire := fs.Bool("squire", squireTask != "", "publish the Squire pid binding, control socket, and event journal (default: on when $SQUIRE_TASK_ID is set)")
	squireDir := fs.String("squire-dir", filepath.Join(home, ".orrery", "squire"), "directory for Squire pids/, ipc/, and sessions/")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	message := strings.TrimSpace(strings.Join(append([]string{*prompt}, fs.Args()...), " "))
	if *externalID == "" && *sessionID == "" && squireTask != "" {
		*externalID = squireTask
	}
	if *workspace == "" {
		*workspace, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(*workspace); err == nil {
		*workspace = abs
	}
	if *squire && squireTask == "" {
		fmt.Fprintln(os.Stderr, "--squire requires $SQUIRE_TASK_ID")
		return 2
	}
	opts := tui.Options{
		SessionID: *sessionID,
		Create: tui.CreateRequest{
			Workspace: *workspace, BudgetUSD: *budget, TierPin: *tier,
		},
		Prompt:  message,
		Version: version,
	}
	if *externalID != "" {
		opts.Create.Integration, opts.Create.ExternalID, opts.Create.ExternalIncarnation = *integration, *externalID, *incarnation
	}
	if *squire {
		opts.SquireDir, opts.SquireTaskID = *squireDir, squireTask
	}

	if *server != "" {
		if opts.SessionID == "" && opts.Create.ExternalID == "" {
			fmt.Fprintln(os.Stderr, "--server needs --session or --external-id: remote sessions are keyed by an external identity")
			return 2
		}
		remote, err := tui.NewRemote(*server, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		opts.Backend = remote
		return finishTUI(tui.Run(ctx, opts))
	}

	// The embedded engine, MCP servers, and language servers log to stderr;
	// route that to a file so it cannot tear the terminal UI.
	logPath := filepath.Join(".orrery", "logs", "tui.log")
	restore, err := redirectStderr(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "redirect logs:", err)
		return 1
	}
	rt, err := openRuntime(ctx, configPath)
	if err != nil {
		restore()
		fmt.Fprintln(os.Stderr, "startup:", err)
		return 2
	}
	opts.Backend = tui.NewLocal(ctx, rt.engine)
	runErr := tui.Run(ctx, opts)
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = rt.close(c)
	cancel()
	restore()
	return finishTUI(runErr)
}

func finishTUI(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "orrery tui:", err)
		return 1
	}
	return 0
}
