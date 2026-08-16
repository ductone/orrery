# Architecture

Orrery's root session and worker sessions execute through the same `TaskRequest → AgentEvent → TaskResult` contract. The web server, headless CLI, native JSON-RPC stdio server, and ACP v1 stdio server are adapters around that contract. The proto definition remains the typed recursive boundary for a later gRPC deployment.

Model selection happens before request assembly. The v1 policy filters incompatible candidates, then scores `(model, effort)` pairs using phase quality, ledger-priced next-call cost, and switch penalties. The chosen model determines system layout, strict tool behavior, reasoning fields, and hashline dialect. Retryable provider failures produce a fresh recorded decision with the failed model excluded.

The stable request prefix is ordered as system instructions, tool definitions, durable task/summary, and todo plan. History follows. Cache ledger entries use provider-specific TTLs. Phase-boundary and hard-ceiling compaction checkpoints state, produces a structured semantic recovery summary, retains four complete assistant turns, and invalidates ledger warmth. A deterministic structured fallback and the pre-compaction checkpoint make summary failure recoverable.

An `ask` tool persists a typed pending-input record and ends the current turn with `input_required`, without terminalizing the session. Checkpoints snapshot session, messages, and todos. Restore and fork affect conversational state only; workspace ownership remains external and files are never silently reverted.

Configured LSP servers are long-lived, lazy subprocesses scoped by workspace and file extension. Orrery exposes navigation, symbols, hover, and diagnostics but not LSP edits, preserving hashline as the single mutation boundary.

Workspace instruction discovery preserves that cache layout. Root compatibility instructions and skill summaries are snapshotted into the stable system region for the session. Nested `AGENTS.md` files and full selected `SKILL.md` bodies are disclosed through tool history only when a path or task requires them. The session tracks disclosed paths so instructions are not repeatedly injected; subtree instructions are ordered broad-to-specific, and the first edit crossing a new instruction boundary is paused before mutation.

Built-ins are compiled Go handlers. Worker specs and results are persisted in SQLite and mirrored under `.orrery/jobs/<id>/`. MCP tools are snapshotted at startup; change notifications can only refresh their stable-prefix definitions at a phase boundary.

Workspace modes express authority rather than an isolation backend. `read` workers use the root checkout with mutation tools removed and may execute asynchronously. `shared-write` workers use that same checkout and execute synchronously, so their parent cannot mutate files concurrently. Root turns take a per-workspace writer lease, preventing two mutable sessions from running against one checkout at the same time. Orrery does not create worktrees or copy repositories for workers; the embedding environment owns stateful task isolation.

SQLite routing records are the learning boundary. `orrery export` emits them without source snapshots, and `orrery eval` compares replay policies using pass rate, cost, latency, and edit-retry outcomes.
