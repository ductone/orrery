# Orrery

Orrery is an opinionated Go agent harness that chooses models inside the agent loop. Routing accounts for task phase, progress and failure signals, compatibility constraints, and the real cost of abandoning a warm prompt cache.

Its contract is one binary, one strict YAML config, a checked-in model catalog, durable SQLite state, built-in coding tools, isolated in-process worker jobs, MCP clients, a local SSE web UI, and routing telemetry suitable for training a later learned policy.

The project's goals, invariants, and intentional boundaries are recorded in the [design charter](docs/design.md). Architectural details are in [architecture](docs/architecture.md).

## Build

```sh
go build ./cmd/orrery
cp orrery.example.yaml orrery.yaml
./orrery
```

Provider keys may be literal strings or `!cmd <command>` values. Secret commands are executed at startup and only their trimmed stdout is retained.

## Commands

```sh
# Browser UI and SSE API on the configured localhost address
./orrery --config orrery.yaml serve

# CI-friendly headless task; TaskResult is JSON and status controls the exit code
./orrery --config orrery.yaml run -p "Fix the failing tests" --workspace "$PWD"

# Embedding transports: newline-delimited JSON-RPC 2.0 or ACP v1 over stdio
./orrery --config orrery.yaml rpc
./orrery --config orrery.yaml acp

# Canonical learning dataset, with source content excluded
./orrery --config orrery.yaml export --since 24h > routing.jsonl

# Turn a completed session into a replay case, then compare policies
./orrery --config orrery.yaml eval --build-session SESSION_ID --acceptance "go test ./..." >> replay.jsonl
./orrery --config orrery.yaml eval --set replay.jsonl --policy frontier-pinned
./orrery --config orrery.yaml eval --set replay.jsonl --policy v1

# Run the public-safe engineering suite and compare a candidate to a baseline
./orrery --config orrery.yaml benchmark --set benchmarks/engineering/cases.jsonl \
  --policy v1 --output .orrery/benchmarks/baseline.json
./orrery --config orrery.yaml benchmark --set benchmarks/engineering/cases.jsonl \
  --policy candidate --baseline .orrery/benchmarks/baseline.json
```

Benchmark cases run in disposable fixture copies. Reports include pass rate, cost per successful case, tokens, latency percentiles, tool errors, first-attempt edit land rate, verification, and independent review. A baseline comparison enforces the 97% pass-rate guardrail before cost improvements count. Keep private replay sets and reports under `.orrery/`; only synthetic, public-safe fixtures belong in the repository.

Root and worker agents share the typed contract in [`proto/agent.proto`](proto/agent.proto). Workers default to detached Git worktrees and fall back to a copied workspace outside Git repositories. Their specs, status, and schema-validated results remain under `.orrery/jobs/` as well as in SQLite.

The built-in tool set is `read`, `search`, hashline `edit`, `exec`, background `job`, `todo`, `spawn`, `ask`, `skill`, `web_search`, and `fetch`. Configuring a language server adds the read-only `lsp` tool for definitions, references, hover, symbols, and diagnostics. MCP tools are namespaced by server. Public fetches reject private, loopback, link-local, credential-bearing, and non-HTTP URLs.

The `ask` tool transitions only the current turn to `input_required`; the session remains resumable through the next message. The typed state is available through HTTP/SSE, native JSON-RPC, ACP `_meta`, and the web composer. The web UI also supports explicit checkpoints, semantic compaction, conversational forks, and restore. Restore never rewrites workspace files.

## Language servers

Language servers are configured explicitly and started lazily per workspace:

```yaml
lsp:
  gopls:
    command: ["gopls"]
    extensions: [".go"]
    language_id: "go"
```

Orrery implements LSP framing and lifecycle directly. Its initial surface is deliberately read-only so semantic navigation cannot bypass hashline staleness checks or edit metrics.

## Context recovery

Automatic compaction runs at phase changes and at 75% of the selected model's context window. Orrery creates a restorable checkpoint before trimming, asks the active model for a structured durable state, retains four complete assistant turns, and invalidates cache warmth. The durable state preserves requirements, decisions, completed work, files, verification, open work, blockers, instructions, and worker results. If semantic summarization fails, a structured deterministic digest is used instead of risking the original history.

## Workspace instructions and skills

Orrery snapshots root `AGENTS.md`, `CLAUDE.md`, and `.github/copilot-instructions.md` files into the stable session prefix. Compatibility files are ordered with `AGENTS.md` last. When a read, search result, or edit enters a deeper directory, Orrery discovers applicable nested `AGENTS.md` files from broadest to most specific and returns each file once through tool history. An edit that first discovers a nested instruction file is paused before mutation so the agent can apply the new rules and retry safely.

Workspace `SKILL.md` files are cataloged by name, description, and path without mounting their bodies into context. The agent uses the `skill` tool to list or load a relevant skill; a skill explicitly named as `$name`, `skill:name`, or “use name skill” in the initial task is mounted immediately. Referenced skill resources remain progressive and are read only when needed. Dependency trees, runtime state, symlinks, non-UTF-8 files, and oversized instruction files are excluded or rejected.

## License

Apache License 2.0. See [LICENSE](LICENSE).
