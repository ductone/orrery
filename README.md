# Orrery

Orrery is an opinionated Go agent harness that chooses models inside the agent loop. Routing accounts for task phase, progress and failure signals, compatibility constraints, and the real cost of abandoning a warm prompt cache.

Its contract is one binary, one strict YAML config, a checked-in model catalog, durable SQLite state, built-in coding tools, context-isolated in-process worker jobs, MCP clients, a local SSE web UI, a session-scoped terminal UI, and routing telemetry suitable for training a later learned policy.

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

# Terminal UI bound to one session; attaches to `serve` with --server
./orrery --config orrery.yaml tui "Fix the failing tests"
./orrery tui --server http://127.0.0.1:7433 --session SESSION_ID

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

Root and worker agents share the typed contract in [`proto/agent.proto`](proto/agent.proto). A worker receives either `read` access to the existing checkout or synchronous `shared-write` access. Exploration, planning, and review default to `read`; implementation defaults to `shared-write`. Orrery does not create worker worktrees or pretend that a source snapshot isolates stateful development services. Worker specs, status, and schema-validated results remain under `.orrery/jobs/` as well as in SQLite.

Only one root turn may hold write access to a workspace at a time. Read workers may run asynchronously, while shared-write workers block their parent until completion. The surrounding environment owns checkout selection, services, databases, ports, and stronger task-level isolation.

The built-in tool set is `read`, `search`, hashline `edit`, `exec`, background `job`, `todo`, `spawn`, `ask`, `skill`, `web_search`, and `fetch`. Configuring a language server adds the read-only `lsp` tool for definitions, references, hover, symbols, and diagnostics. MCP tools are namespaced by server. Public fetches reject private, loopback, link-local, credential-bearing, and non-HTTP URLs.

The `ask` tool transitions only the current turn to `input_required`; the session remains resumable through the next message. The typed state is available through HTTP/SSE, native JSON-RPC, ACP `_meta`, the web composer, and the terminal UI. The web UI also supports explicit checkpoints, semantic compaction, conversational forks, and restore. Restore never rewrites workspace files.

The terminal UI renders one session's event log into scrollback and keeps a live region for the running turn, plan, queue, and composer. It embeds the engine or attaches to `serve`, and implements the harness side of Squire's agent contract: session binding, a prompt control socket, and an event journal. See [terminal UI](docs/tui.md).

## Routing

Most harnesses pick one model per session. Orrery re-decides at four points: the start of every turn (`turn`), when a worker job is spawned (`spawn`), when an independent reviewer is created (`review`), and when the loop escalates after a stall (`escalation`). Each decision is scored, recorded, and explained in one line, for example `stayed on <model>: phase implement, warm prefix 82K, estimated next-call cost $0.0141`.

A decision runs in two stages: hard filters, then scoring.

**Filters** remove models that cannot or must not run the call. A candidate is rejected when the provider is not configured, the model was excluded after a provider failure, the request carries an image the model cannot read, input plus expected output exceeds the context window, its family is excluded, a tier pin does not match, switching is disabled, or the phase sits under the frontier floor (`plan`, `diagnose`, and `review` by default). Reviewers additionally reject the implementer's own family, but only after confirming some other family is actually usable, so single-provider deployments still get a review. If nothing survives, routing fails loudly rather than silently downgrading.

**Scoring** ranks whatever remains by `score = quality − lambda_cost × cost − switch_penalty`.

- *Quality* starts from the tier (frontier, efficient, tiny) and is then adjusted by phase. Judgement-heavy phases (`plan`, `diagnose`, `review`) reward frontier models and penalize the rest; throughput phases (`explore`, `implement`, `wrap-up`) give efficient models a bonus, since most agent turns are mechanical.
- *Cost* is the estimated price of the actual next call, computed from live token counts and cached-prefix pricing, not a list price. `lambda_cost` is the single dial that says how much quality a dollar is worth.
- *Switch penalty* prices the cache you would throw away. Leaving a warm model mid-tool-chain costs more, and the penalty grows with conversation size. Critically, it only applies when the prefix is warm: right after compaction there is no cache to protect, so cost and quality decide freely.

**Stall handling** is where routing earns its keep. Repeated failed commands, a test-failure streak, repeated edits, turns without progress, or a phase running long all mark the turn as stalled. Orrery then distinguishes two kinds of stuck. Hard failures look like a capability ceiling and push toward frontier models. But repeated reads or searches are a discipline problem, not a hard problem, so the largest bonus goes to *efficient* models: redundant exploration escalates to a cheaper, better-behaved model instead of burning frontier tokens re-reading the same files.

Reasoning effort follows the same phase logic — high for planning, diagnosis, review, and repeated test failures; low for wrap-up; medium otherwise — clamped to what each model supports. The chosen model also fixes its edit dialect and whether the strict or portable toolset is used.

Ties break deterministically: keep the current model, then prefer the configured default, then sort by ID. Identical state produces an identical decision, which is what makes replay evaluation meaningful.

Every decision — full input state, all candidates including rejected ones with reasons, chosen model, effort, cache estimate, and explanation — is written to SQLite. `export` turns that into a training dataset, `eval` replays recorded sessions against alternative policies, and `benchmark` compares a candidate policy to a baseline. Routing is tuned by measurement, and the schema is deliberately shaped for a learned policy to replace the hand-written `v1` weights later.

```yaml
router:
  lambda_cost: 0.35                            # higher = more cost-sensitive
  frontier_floor_phases: [plan, diagnose, review]
  disable_switch: false                        # true pins the session to one model
```

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
