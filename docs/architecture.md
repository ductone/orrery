# Architecture

Orrery's root session and worker sessions execute through the same `TaskRequest → AgentEvent → TaskResult` contract. The web server, terminal UI, headless CLI, native JSON-RPC stdio server, and ACP v1 stdio server are adapters around that contract. The terminal UI is scoped to one session: it replays that session's event log from the store or over SSE, so attaching mid-turn or after a restart shows the same transcript, and it submits through the same queue-aware continuation path as the HTTP API. The proto definition remains the typed recursive boundary for a later gRPC deployment.

Model selection happens before request assembly. The v1 policy filters incompatible candidates, then scores `(model, effort)` pairs using phase quality, ledger-priced next-call cost, and switch penalties. The chosen model determines system layout, strict tool behavior, reasoning fields, and hashline dialect. Retryable provider failures produce a fresh recorded decision with the failed model excluded.

The stable request prefix is ordered as system instructions, tool definitions, durable task/summary, and todo plan. History follows. Cache ledger entries use provider-specific TTLs. Phase-boundary and hard-ceiling compaction checkpoints state, produces a structured semantic recovery summary, retains four complete assistant turns, and invalidates ledger warmth. A deterministic structured fallback and the pre-compaction checkpoint make summary failure recoverable.

An `ask` tool persists a typed pending-input record and ends the current turn with `input_required`, without terminalizing the session. Checkpoints snapshot session, messages, and todos. Restore and fork affect conversational state only; workspace ownership remains external and files are never silently reverted.

Configured LSP servers are long-lived, lazy subprocesses scoped by workspace and file extension. Orrery exposes navigation, symbols, hover, and diagnostics but not LSP edits, preserving hashline as the single mutation boundary.

Workspace instruction discovery preserves that cache layout. Root compatibility instructions and skill summaries are snapshotted into the stable system region for the session. Nested `AGENTS.md` files and full selected `SKILL.md` bodies are disclosed through tool history only when a path or task requires them. The session tracks disclosed paths so instructions are not repeatedly injected; subtree instructions are ordered broad-to-specific, and the first edit crossing a new instruction boundary is paused before mutation.

Built-ins are compiled Go handlers. Worker specs and results are persisted in SQLite and mirrored under `.orrery/jobs/<id>/`. MCP tools are snapshotted at startup; change notifications can only refresh their stable-prefix definitions at a phase boundary.

Workspace modes express authority rather than an isolation backend. `read` workers use the root checkout with mutation tools removed and may execute asynchronously. `shared-write` workers use that same checkout and execute synchronously, so their parent cannot mutate files concurrently. Root turns take a per-workspace writer lease, preventing two mutable sessions from running against one checkout at the same time. Orrery does not create worktrees or copy repositories for workers; the embedding environment owns stateful task isolation.

Optional Jev shadow observations are a separate record type. They are asked asynchronously at the stall judge, turn start, worker spawn, and review, stored with their question version, the harness's own decision, and later ground truth, and never read by routing, interventions, or budgets. They are the evidence for deciding whether a classifier should later take over any of those decisions.

Search ranking is the one Jev integration that changes behaviour, and only when the model asks for it by passing an intent. The regex still finds every candidate; Jev only orders files and moves low-relevance ones below a cutoff, and a ranking failure falls back to the plain result.

Each run snapshots the workspace's uncommitted files so review and verification cover only the run's own changes. Independent review is planned before any reviewer starts (`internal/review`). Deterministic rules decide what is always reviewed (code) and what never is (assets, lockfiles, generated files); Jev, when `jev.review` is set, triages the remaining files, scores the change's risk to size reviewers, downgrades findings that are not correctness bugs, and decides whether an inconclusive part can be accepted. Large reviews run as parallel shards whose verdicts are merged. Forced-synthesis and tool-restricted turns never change the stable prefix; they append a directive and constrain calls with `tool_choice`.

SQLite routing records are the learning boundary. `orrery export` emits them without source snapshots, and `orrery eval` compares replay policies using pass rate, cost, latency, and edit-retry outcomes.
