# Architecture

Orrery's root session and worker sessions execute through the same `TaskRequest → AgentEvent → TaskResult` contract. The web server and headless CLI are adapters around that contract; the proto definition reserves the later out-of-process transport without making the core depend on gRPC.

Model selection happens before request assembly. The v1 policy filters incompatible candidates, then scores `(model, effort)` pairs using phase quality, ledger-priced next-call cost, and switch penalties. The chosen model determines system layout, strict tool behavior, reasoning fields, and hashline dialect. Retryable provider failures produce a fresh recorded decision with the failed model excluded.

The stable request prefix is ordered as system instructions, tool definitions, durable task/summary, and todo plan. History follows. Cache ledger entries use provider-specific TTLs. Phase-boundary and hard-ceiling compaction moves old activity into the durable summary, retains a short live tail, and invalidates ledger warmth.

Workspace instruction discovery preserves that cache layout. Root compatibility instructions and skill summaries are snapshotted into the stable system region for the session. Nested `AGENTS.md` files and full selected `SKILL.md` bodies are disclosed through tool history only when a path or task requires them. The session tracks disclosed paths so instructions are not repeatedly injected; subtree instructions are ordered broad-to-specific, and the first edit crossing a new instruction boundary is paused before mutation.

Built-ins are compiled Go handlers. Worker specs and results are persisted in SQLite and mirrored under `.orrery/jobs/<id>/`. MCP tools are snapshotted at startup; change notifications can only refresh their stable-prefix definitions at a phase boundary.

SQLite routing records are the learning boundary. `orrery export` emits them without source snapshots, and `orrery eval` compares replay policies using pass rate, cost, latency, and edit-retry outcomes.
