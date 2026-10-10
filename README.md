# Orrery

Orrery is an opinionated Go agent harness that chooses models inside the agent loop. Routing accounts for task phase, progress and failure signals, compatibility constraints, and the real cost of abandoning a warm prompt cache.

Its contract is one binary, one strict YAML config, a checked-in model catalog extended by provider discovery, durable SQLite state, built-in coding tools, context-isolated in-process worker jobs, MCP clients, a local SSE web UI, a session-scoped terminal UI, and routing telemetry suitable for training a later learned policy.

The project's goals, invariants, and intentional boundaries are recorded in the [design charter](docs/design.md). Architectural details are in [architecture](docs/architecture.md).

## Build

```sh
go build ./cmd/orrery
mkdir -p ~/.orrery && cp orrery.example.yaml ~/.orrery/orrery.yaml
cd ~/code/some-repo && orrery        # terminal UI session in this directory
orrery -p "Fix the failing tests"    # same, sending a first message
orrery --session SESSION_ID          # resume a session; printed on exit
```

Bare `orrery` starts a terminal UI session in the current directory; it needs a terminal, and scripts should name a command. Configuration is `--config`, else `$ORRERY_CONFIG`, else `./orrery.yaml` (a per-directory override), else `~/.orrery/orrery.yaml`. Relative paths inside a config file resolve against that file's directory. The database defaults to `~/.orrery/orrery.db` and logs go to `~/.orrery/logs/`; `$ORRERY_HOME` moves both. Commands that call models fail at startup when no providers are configured.

Provider keys may be literal strings or `!cmd <command>` values. Secret commands are executed at startup and only their trimmed stdout is retained.

## Commands

```sh
# Browser UI and SSE API on the configured localhost address
./orrery serve

# Terminal UI bound to one session; attaches to `serve` with --server
./orrery tui "Fix the failing tests"
./orrery tui --server http://127.0.0.1:7433 --session SESSION_ID

# CI-friendly headless task; TaskResult is JSON and status controls the exit code
./orrery run -p "Fix the failing tests" --workspace "$PWD"

# Embedding transports: newline-delimited JSON-RPC 2.0 or ACP v1 over stdio
./orrery rpc
./orrery acp

# Canonical learning dataset, with source content excluded
./orrery export --since 24h > routing.jsonl

# Turn a completed session into a replay case, then compare policies
./orrery eval --build-session SESSION_ID --acceptance "go test ./..." >> replay.jsonl
./orrery eval --set replay.jsonl --policy frontier-pinned
./orrery eval --set replay.jsonl --policy v1

# Run the public-safe engineering suite and compare a candidate to a baseline
./orrery benchmark --set benchmarks/engineering/cases.jsonl \
  --policy v1 --output .orrery/benchmarks/baseline.json
./orrery benchmark --set benchmarks/engineering/cases.jsonl \
  --policy candidate --baseline .orrery/benchmarks/baseline.json

# List the catalog startup would build (no provider keys needed)
./orrery models
./orrery models --stats            # add per-model call statistics
```

### Model catalog and statistics

`orrery models` lists the startup catalog: discovered routes (or their cache), built-in routes, and config overrides. The aligned table shows route and canonical model IDs, tier, family, input/output prices per million tokens, context window, and discovered/overridden flags. Disabled config entries and override warnings appear below it.

The command does not resolve secrets or require provider keys. `--stats` adds call counts, EWMA latency and output tokens/s (alpha 0.1), truncated percentage, failure counts, and the age of the last slow call (over 120s). Existing main-loop usage and failure events are backfilled when the stats table is empty. These statistics do not yet affect routing.

Benchmark cases run in disposable fixture copies. Reports include pass rate, cost per successful case, tokens, latency percentiles, tool errors, first-attempt edit land rate, verification, and independent review. A baseline comparison enforces the 97% pass-rate guardrail before cost improvements count. Keep private replay sets and reports under `.orrery/`; only synthetic, public-safe fixtures belong in the repository.

Root and worker agents share the typed contract in [`proto/agent.proto`](proto/agent.proto). A worker receives either `read` access to the existing checkout or synchronous `shared-write` access. Exploration, planning, and review default to `read`; implementation defaults to `shared-write`. Orrery does not create worker worktrees or pretend that a source snapshot isolates stateful development services. Worker specs, status, and schema-validated results remain under `.orrery/jobs/` as well as in SQLite.

Only one root turn may hold write access to a workspace at a time. Read workers may run asynchronously, while shared-write workers block their parent until completion. The surrounding environment owns checkout selection, services, databases, ports, and stronger task-level isolation.

The built-in tool set is `read`, `search`, hashline `edit`, `exec`, background `job`, `todo`, `spawn`, `ask`, `skill`, `web_search`, and `fetch`. Configuring a language server adds the read-only `lsp` tool for definitions, references, hover, symbols, and diagnostics. MCP tools are namespaced by server. Public fetches reject private, loopback, link-local, credential-bearing, and non-HTTP URLs. `fetch` returns a page's readable text (HTML is reduced to headings, paragraphs, lists, and code, without scripts, styles, or navigation) 60,000 characters at a time, with a `start` offset for the rest; `web_search` is offered only when a search key is configured. Any single tool result over 100,000 characters is stored as its head and tail with a note on how to narrow the call, and a history that no available model can hold has its oversized tool results shrunk and is compacted before routing is retried.

The `ask` tool transitions only the current turn to `input_required`; the session remains resumable through the next message. The typed state is available through HTTP/SSE, native JSON-RPC, ACP `_meta`, the web composer, and the terminal UI. The web UI also supports explicit checkpoints, semantic compaction, conversational forks, and restore. Restore never rewrites workspace files.

The terminal UI renders one session's event log into scrollback and keeps a live region for the running turn, plan, queue, and composer. It embeds the engine or attaches to `serve`, and implements the harness side of Squire's agent contract: session binding, a prompt control socket, and an event journal. See [terminal UI](docs/tui.md).

## Workspace memory

Orrery stores durable memory in local SQLite, scoped to the workspace rather than a session. Active-record injection defaults on, bounded to **8 records / 1200 estimated tokens**; set `memory.inject: false` to omit active memory from prompts without disabling extraction or storage. Retrieved memory is untrusted data, never instructions.

The parent extracts evidence-backed proposals from root-session evidence after successful requests (with startup catch-up) and during compaction. Workers cannot propose typed memory candidates, and worker results are not a direct extraction source. Extracted records start pending: Jev triage drops candidates scoring below 0.5; missing credentials, failed calls, or missing answers leave them pending. Successful triage plus sightings in **two distinct sessions** promotes a proposal, even with `auto_commit: false` (the default). Opting into `auto_commit` allows immediate activation after successful triage.

Pending proposals are surfaced in the next request context as **untrusted proposals, not established facts**, so the agent can ask for confirmation. Proposal event notices carry IDs, not memory text. The `memory` tool supports inspect/propose/confirm/correct/forget; confirmation, corrections, and forgetting require the person's explicit instruction. Corrections supersede old records; forgetting removes their content and invalidates pinned memory.

Supported kinds are `fact`, `command`, `decision`, `preference`, and `lesson`; provenance is `user`, `instruction`, `observed`, or `extracted`. Migration normalizes unknown legacy kinds/provenance to `fact`/`observed`. `retain_days: 0` means no automatic expiry; memory listing sweeps elapsed expiry timestamps into the persisted `expired` status, excluding those records from active injection. See [memory design and current behavior](docs/proposals/memory.md) and [example config](orrery.example.yaml).

## Routing

Most harnesses pick one model per session. Orrery re-decides at four points: the start of every turn (`turn`), when a worker job is spawned (`spawn`), when an independent reviewer is created (`review`), and when the loop escalates after a stall (`escalation`). Each decision is scored, recorded, and explained in one line, for example `stayed on <model>: phase implement, warm prefix 82K, estimated next-call cost $0.0141`.

A decision runs in two stages: hard filters, then scoring.

**Filters** remove models that cannot or must not run the call. A candidate is rejected when the provider is not configured, the model was excluded after a provider failure, the request carries an image the model cannot read, input plus expected output exceeds the context window, its family is excluded, a tier pin does not match, switching is disabled, or the phase sits under the frontier floor (`plan`, `diagnose`, and `review` by default). Reviewers additionally reject the implementer's own family, but only after confirming some other family is actually usable, so single-provider deployments still get a review. If nothing survives, routing fails loudly rather than silently downgrading.

**Scoring** ranks whatever remains by `score = quality − lambda_cost × (work_cost + time_cost) − switch_penalty − performance_penalty`.

- *Quality* starts from the tier (frontier, efficient, tiny) and is then adjusted by phase. Judgement-heavy phases (`plan`, `diagnose`, `review`) reward frontier models and penalize the rest; throughput phases (`explore`, `implement`, `wrap-up`) give efficient models a bonus, since most agent turns are mechanical.
- *Work cost* prices a unit of work, not one call: the next call at the session's actual cache warmth, then the further calls the model typically needs for the same work (`CallsPerTask` in the catalog, measured by pinned benchmark sweeps: a model that takes 2.7 steps where Claude takes one pays for re-sending the context each time), each at the route's recorded output per call and cache reuse from `model_stats`. Routes with few recorded calls lean on priors. `lambda_cost` is the dial that says how much quality a dollar is worth.
- *Time cost* values the expected seconds of that work (steps × recorded latency) at `time_value_usd_per_minute`: `interactive` when someone is waiting (the default), `background` for runs marked `orrery run --background` and the jobs they spawn. With time free, the cheapest model wins; with it valued, a faster model that needs fewer steps can win despite a higher token price.
- *Switch penalty* prices the cache you would throw away. Leaving a warm model mid-tool-chain costs more, and the penalty grows with conversation size. Critically, it only applies when the prefix is warm: right after compaction there is no cache to protect, so cost and quality decide freely.

While the agent fixes findings from a failed independent review, its turns are routed to frontier models: a reviewer finding real bugs is a hard-failure signal, not mechanical work. That remediation ends a run after eight turns without an edit, or after four rejected reviews; a completion whose diff is unchanged since a failed review is refused with those findings instead of being reviewed again.

**Stall handling** is where routing earns its keep. Repeated failed commands, a test-failure streak, repeated edits, turns without progress, or a phase running long all mark the turn as stalled. Orrery then distinguishes two kinds of stuck. Hard failures look like a capability ceiling and push toward frontier models. But repeated reads or searches are a discipline problem, not a hard problem, so the largest bonus goes to *efficient* models: redundant exploration escalates to a cheaper, better-behaved model instead of burning frontier tokens re-reading the same files.

Reasoning effort follows the same phase logic — high for planning, diagnosis, review, and repeated test failures; low for wrap-up; medium otherwise — clamped to what each model supports. The chosen model also fixes its edit dialect and whether the strict or portable toolset is used.

Ties break deterministically: keep the current model, then prefer the configured default, then sort by ID. Identical state produces an identical decision, which is what makes replay evaluation meaningful.

Every decision — full input state, all candidates including rejected ones with reasons, chosen model, effort, cache estimate, and explanation — is written to SQLite. `export` turns that into a training dataset, `eval` replays recorded sessions against alternative policies, and `benchmark` compares a candidate policy to a baseline. Routing is tuned by measurement, and the schema is deliberately shaped for a learned policy to replace the hand-written `v1` weights later.

```yaml
router:
  lambda_cost: 0.35                            # higher = more cost-sensitive
  time_value_usd_per_minute:                   # what waiting is worth
    interactive: 0.25                          # someone is waiting (default)
    background: 0                              # orrery run --background and its jobs
  frontier_floor_phases: [plan, diagnose, review]
  disable_switch: false                        # true pins the session to one model
```

## Model catalog

The built-in catalog defines each model once (`model.Models` in `internal/model/models.go`): its family (the lab, which cross-family review uses), its tier, which is Orrery's judgement of its quality, its input types, limits, reasoning levels, and edit dialect. Separately, `model.Routes` lists who serves each model, by provider, with the provider's id for it and its price. A provider's API quirks (token-limit field, strict tool schemas, reasoning echo, prompt caching) are set per provider, not per model. Each route becomes a routable model with the id `provider/wire-id`, such as `ramp/grok-4.7` or `fireworks/accounts/fireworks/models/kimi-k2p7-code`, and that id is what routing records and session state name. When a route fails with a retryable error, the turn tries another route to the same model before moving on to another model of the same tier.

At startup and on config reload, Orrery asks providers that list their models for their catalog (Ramp Router's `GET /v1/models` today) and merges three layers, each winning over the one before: discovered models, the built-in catalog, then `models:` overrides in the config. A built-in route the provider also lists takes the listing's price and limits, which the provider decides, and keeps everything else. Discovery has a five-second timeout and falls back to the last good listing cached in `~/.orrery/catalog/`, then to the built-in catalog alone, so it never stops Orrery from starting. The startup log reports how many models were listed, usable, and overridden.

A listed model that is in the curated set keeps its definition: a Ramp listing of `grok-4.6` is frontier-tier xAI, as it is served directly, with Ramp's API settings and the listed price. Other discovered models are used only when they are active, support the Responses API and tool calling, have at least a 64K context window, and list prices. A discovered model is never inferred to be frontier tier: reasoning models costing $0.50 or more per million output tokens become efficient tier, and cheaper or non-reasoning ones tiny. Discovered routes, including listed routes to curated models, also carry a quality penalty in routing, so price alone cannot make one outscore a built-in route; a `models:` override that sets `tier` vouches for the model and removes it. Its family comes from its name (so a new vendor diversifies reviews), and it gets portable compatibility settings and the contextual edit dialect.

```yaml
models:
  - id: ramp/qwen4-coder          # promote a discovered model once it has earned it
    tier: frontier
  - id: ramp/some-flaky-model     # remove a model from routing
    disabled: true
  - id: ramp/claude-opus-5-5      # field-level override of one route
    pricing: {input: 4.5}
  - id: grok-4.7                  # no provider prefix: every route to the model
    tier: efficient
```

Models Ramp serves only through upstreams that need the account's own provider key (Amazon Bedrock) are left out unless the provider config says the account has one: `ramp: {api_key: ..., provider_keys: [bedrock]}`. When a provider refuses a model at request time anyway (no access, a provider key the account lacks, an unknown model), the turn routes to another model instead of failing, and the refused model leaves routing for the rest of the process. Refusals whose error code names the model are also remembered in `~/.orrery/catalog/unavailable.json` for a week, so the next startup leaves the model out.

Overrides change only the fields they set. An entry whose id has no provider prefix names a model, and applies to every route serving it; one that names no served model is reported as a warning. An entry for a route that is not in the catalog adds it when it gives `family`, `tier`, `context_window`, `max_output`, and input and output pricing; otherwise it is reported as a warning and skipped, since the model may just not have been listed this time.

## Jev

Several decisions use TypeSafe's [Jev](https://docs.typesafe.ai/) classifier: search ranking, review triage, the instruction phase, the answer check, tool-result relevance, and memory triage. Each is enabled under `jev:`; nothing is sent unless a site is enabled.

Jev uses the resolved `providers.ramp` key and base URL, which default to `https://api.router.com`, so no separate key is needed. Setting `jev.api_key` instead uses TypeSafe directly by default, and `jev.base_url` overrides the endpoint either way.

### Search ranking

With `jev.search_ranking: true`, `search` gains an optional `intent` parameter. When the model passes one and the pattern matches more than 20 lines across several files, each matching file (up to 200) is scored by Jev for relevance to the intent, from its path and up to ten matching lines sampled across the file with surrounding context. Files come back in relevance order with their lines; files below 0.3 relevance are listed by path, score, and match count without their lines, so nothing disappears silently. The top three files always keep their lines. If ranking fails or times out, search returns its ordinary result with a `ranking_error`.

Independently of Jev, a search that hits `max_results` now reports the total match count, the number of matching files, and the files with the most matches, instead of silently truncating in walk order.

## Limits pause, they do not fail

A run ends when the work is done, when it needs the person, or on an error nothing can recover from. Limits change strategy instead of ending it:

- **Budgets.** When a session reaches its dollar budget, it pauses with a question: continue with another `budget.session_usd`, or stop. The token and wall-clock limits of a run ask whether to keep going. A worker's budget is a hard slice of its parent's; a worker that exhausts it returns its latest findings as a partial result.
- **Stall checks** (a phase running long, review findings left unfixed, an unchanged plan resubmitted) climb an escalation ladder: a nudge, then a different model, then compacting history and restating the latest request, and only then a question to the person.
- **A misbehaving model** (repeated empty replies, truncated tool calls, results that fail the schema) is set aside for the rest of the run and the turn reroutes. If no model is left, or a provider error cannot be retried, the run asks rather than fails, naming the models it set aside and why.
- **"Finish now" advice** after verification or a long review is advice: tools stay available, so findings can still be fixed.

Answering the question resumes the session: agreeing (or replying with guidance) continues, and "Stop here" ends it cleanly. A headless `orrery run` exits with status 4 at the question; resume it with `orrery --session ID`.

## Verification and review scope

At the start of every run Orrery records the workspace's uncommitted state. Independent review then looks only at what the run changed: untracked notes, edits in progress, and staged work already in the checkout are left out, so they are neither reviewed nor sent to a reviewer or classifier.

Orrery does not decide whether a change was verified, and keeps no list of check commands. Every command the agent runs after its last edit is recorded with its output, and the review stages above weigh it: the Jev gate and the light reviewer see each command and what it printed, so a repository's own `./scripts/check.sh` counts as much as `go test`, and a `go test ./pkg/a` that doesn't cover a change in `pkg/b` is visible as such. The outcome's `verified` means only that some command ran after the last edit.

Every prompt leads with the person's latest message, verbatim, as the current request; the session's first message follows as context, marked as answered unless restated. The latest request is stored with the session, so it survives compaction, and compaction never files it as resolved. With `jev.review`, each final result is also checked against the latest request before verification and review run: an answer to an earlier question is refused (at most twice) with the latest request quoted back, and a Jev outage never blocks completion.

A turn that starts with a message a person sent has been routed as planning, which floors it to a frontier model at high effort. Messages the harness writes itself (rejections, nudges, worker handoffs) are marked and never count as new instructions. With `jev.routing`, Jev reads a real message against the current plan and chooses the phase, so "keep going" or "also fix the typo" is routed as implementation rather than planning; below 0.8 confidence, or if Jev is unavailable, the turn is planned as before. Only the arriving turn is affected, and the choice is recorded in the routing record as `instruction_phase`.

When every configured credential is backed off after a rate limit or server error, routing waits for the earliest one to return (up to two minutes, honouring the provider's `Retry-After`) instead of failing the turn, worker, or review.

Phase changes compact history only at real boundaries: not when the history is small, not within six turns of the last compaction, and not when a session returns to a phase it left a few turns earlier.

## Independent review

When a worker changes the workspace, Orrery reviews the change in stages, cheapest first:

1. **Size.** A change over 400 reviewed lines or 10 files (assets, lockfiles, generated and binary files don't count) goes straight to the full review below.
2. **Jev gate, prose only.** A change that touches no code file (by name) and whose other files Jev judges prose rather than behaviour (docs and example text, not CI, schemas, or configuration a program reads) is offered to Jev, which sees the request, the diff, and every command run since the last edit, and approves it at a score of 0.7 or more. Code never reaches the gate: on a review benchmark Jev's scores barely separated buggy code changes from clean ones and it approved two of eight bugs, while the light reviewer caught every bug it saw. Changes that delete or weaken tests skip the gate too.
3. **Light review.** One reviewer from another model family, at low effort with the same evidence and a few turns, approves, rejects a concrete bug (file, line, failing case), or escalates when it suspects a problem it cannot confirm.
4. **Full review**, only for large changes and escalations, as follows.

Each stage is recorded (`review.gate`, `review.escalated`, `review.outcome` with its `stage`). For the full review, Orrery plans before any reviewer runs. The diff is split into files and classified: code (by extension and well-known names such as `Makefile` and `Dockerfile`) is always shown in full; assets, lockfiles, generated files, and binaries are listed without their patch; everything else (prose, data, configuration) is shown in full unless Jev judges it not to need a correctness review. A diff with nothing left to review is not reviewed.

The rest is packed by top-level directory into at most four parallel reviewers of about 60K characters each; oversized patches are truncated with a pointer to the file, and code is never dropped. Each reviewer's turn limit grows with its share of the diff. Every reviewer's spec lists the files it was not shown, so it can read one when a finding depends on it.

A part without a verdict is reviewed once more by another model family with more room. Findings Jev is fairly sure are not correctness bugs (style, naming, pre-existing problems) become notes, and a review whose findings are all notes passes. Read-only workers, reviewers included, synthesise their result at their turn limit. Without `jev.review`, prose is reviewed in full and findings stand. The plan, each decision, and every score are recorded as `review.*` events. Jev's risk score for the change is recorded for calibration (`orrery shadow --report`) but drives nothing: on real reviews it predicted outcomes worse than chance. A Jev difficulty score and a Jev worker-convergence check were removed for the same reason: neither changed a decision usefully.

Turns that force a result or restrict tools keep the tool definitions and system prompt unchanged, so the cached prefix survives: the directive travels as a trailing message and calls are forbidden with `tool_choice`. A response cut off at the output limit is retried with a larger limit, and every response's stop reason and output kinds are recorded.

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
