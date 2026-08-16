# Design charter

This document records Orrery's product goals, architectural commitments, and
intentional boundaries. It is the reference for evaluating feature proposals:
a feature should advance the goals without quietly turning the harness into a
gateway, IDE, plugin host, terminal manager, or hosted control plane.

## Mission

Orrery is an opinionated, local-first agent harness for software engineering.
It aims to approach frontier-model task quality at substantially lower total
cost by making model and effort choices inside the agent loop, where task
phase, progress, cache state, tools, and outcomes are visible.

Cost reduction is subordinate to successful work. Staying on a frontier model
for an entire difficult session can be the correct routing result. Evaluation
must compare cost per successful run and enforce a quality floor; it must not
reward cheap failures or switching for its own sake.

## Design goals

### Harness-native routing

Model choice happens before request assembly. The chosen model determines the
prompt layout, tool definitions, edit dialect, reasoning effort, compatibility
shaping, and cache behavior. There is no post-hoc model substitution below the
router.

Routing occurs at meaningful boundaries: turn start, worker creation, review
creation, and escalation. Remaining on a warm model is an explicit routing
decision. A switch must justify its cache miss and fresh-context reread.

### Cache-aware context economics

Prompt caching is managed deliberately. Stable instructions, tool schemas,
durable task state, and plans precede volatile history. A cache ledger estimates
the actual next-call cost for each candidate, including warm reads, fresh
prefixes, family switches, and threshold repricing.

Compaction is a visible cache event, not background housekeeping. It occurs at
phase boundaries or a hard context threshold, creates a recovery checkpoint,
writes structured durable state, retains a short complete history tail, and
invalidates the affected cache estimate.

### Quality that improves from evidence

Every routing decision records its input state, candidates, choice,
explanation, cache estimate, and eventual outcome. Worker completion conditions
and independent reviews provide bounded credit-assignment signals.

The initial policy is deterministic and inspectable. A later learned policy may
be trained offline from the same records and shipped behind the same interface,
without changing the core loop or persistence schema.

### Resilience that preserves routing intent

Provider errors use tier-preserving fallback chains rather than arbitrary model
replacement. Rate limits and transient server failures move work to another
compatible candidate, while credentials receive independent cooldown and
round-robin handling. Cross-family fallback is still a real routing event: it
must pay the cache and compatibility costs instead of pretending only the model
identifier changed.

### Opinionated maintainability

The distribution remains one Go binary with one strict configuration file.
The model catalog and compatibility rules are checked-in code. Built-in tools
are ordinary Go packages. Unknown configuration keys fail rather than being
silently ignored.

Adding a compatible model should require a catalog entry and, optionally, a
routing weight. It should not require provider-specific orchestration logic.
This is the project's future-proofing test: better models should improve task
performance without increasing harness complexity.

### A small, safe coding surface

The built-in tool set should remain compact and legible. Reads and searches
shape large results to protect context. Shell output is summarized with durable
logs. Source edits pass through content-anchored hashline validation so stale
context fails before it corrupts a file.

Language-server integration is initially read-only. Semantic navigation and
diagnostics may guide the model, but mutations continue through the same
observable edit boundary.

### Durable recursive agents

Root and worker sessions use the same typed task, event, result, budget, and
workspace contracts. The root is a worker with an interactive transport
attached, not a separate orchestration architecture.

Worker specifications, completion conditions, logs, results, artifacts, and
budgets are durable state. They do not exist only in a parent transcript.
Budgets propagate downward and recursion depth is enforced.

### Environment-native workspace sharing

One root session represents one human-level task inside a workspace and its
stateful development environment. Workers normally need the same checkout,
services, databases, ports, and caches as their parent. Orrery therefore
models workspace authority directly instead of equating a detached source tree
with environment isolation.

Read workers share the checkout with mutation tools removed and may run in the
background. Shared-write workers run synchronously while the parent is paused.
A per-workspace writer lease prevents simultaneous mutable root turns. Creating
checkouts, snapshots, containers, or complete development environments remains
the embedding system's responsibility.

### Progressive instruction discovery

Workspace instructions and skill summaries enter the stable prefix when their
scope is known. Nested instructions and full skill bodies are loaded only when
a path or task requires them. Crossing a newly discovered instruction boundary
can pause an edit before mutation.

This keeps cache prefixes stable while respecting repository-local guidance.
Instructions are data with explicit provenance and scope; discovering a skill
does not install executable code.

### Thin, typed transports

HTTP/SSE, headless execution, native JSON-RPC, ACP, and future transports are
adapters over the same engine contract. Transport lifecycle operations must not
rewrite an already successful terminal result. A request for missing user input
ends only the current turn and leaves the session resumable.

### Observable behavior without private reasoning

Users and evaluators should be able to inspect plans, routing explanations,
tool calls, command results, edits, costs, failures, reviews, summaries, and
outcomes. These artifacts are the evidence used to diagnose performance.

Private model reasoning is not a product surface and is not required for
accountability. The harness should improve the quality of observable evidence
rather than depend on exposing hidden reasoning traces.

### Benchmarkable engineering performance

Evaluation tracks successful completion, cost per success, tokens, latency,
tool errors, edit land rate, verification, independent review, and recovery
after context transitions. Candidate policies must satisfy a pass-rate
guardrail relative to a frontier-pinned baseline before cost wins count.

Only synthetic, redistributable fixtures belong in the public repository.
Private replay data remains local unless explicitly and safely exported.

## Architectural invariants

- Choice precedes request assembly.
- Provider fallback re-enters compatibility and routing logic.
- Stable-prefix changes are explicit cache events.
- Durable state, not transcript recollection, is the system of record.
- Hashline is the source-file mutation boundary.
- Results are schema-validated when a result schema is supplied.
- Child budgets and recursion depth are hard limits.
- Review should use a different model family when a compatible alternative is
  available.
- Workspace boundaries are explicit; tools do not discover broader authority.
- Session restore never silently rewrites workspace files.
- Ordinary tool and MCP results are untrusted task data, not instructions.
- Public telemetry and fixtures exclude source content, credentials, private
  issue text, customer data, and captured production transcripts.

## Intentional non-goals

### Not a terminal or environment manager

Orrery executes commands as an agent tool, but it does not aim to provide a
general terminal emulator, PTY product, environment lifecycle manager, package
installer, or remote workspace service. An embedding host may own those
facilities.

Orrery also does not create detached worktrees or repository copies for worker
jobs. Source-only isolation is misleading when commands still share services,
databases, ports, caches, and process-global configuration. A future isolated
write mode would require an explicit, complete patch handoff contract and is
not part of the current worker model.

### Not an approval system

The harness assumes it runs inside an appropriately isolated workspace. It does
not implement per-tool permission prompts or policy approval modes.
`input_required` is for information genuinely needed to continue, not for
security authorization.

### Not an authentication or multi-tenant control plane

The built-in web interface targets localhost or a trusted network. Orrery does
not currently provide user accounts, tenant isolation, public-service
hardening, billing, or an OAuth broker. The deployment perimeter remains the
security boundary.

### Not a model gateway

Orrery does not transparently swap model identifiers after a request has been
built. It does not aim to be an organization-wide inference proxy. Its routing
advantage comes from agent state, job design, cache economics, and outcome
feedback that a generic gateway cannot see.

### Not a provider-count competition

The project favors a small, tested catalog with explicit compatibility and
pricing behavior over broad but shallow provider coverage. Supporting every
host, deployment target, or model-discovery API is not a goal.

### Not a plugin marketplace

There is no dynamic executable plugin API or marketplace. Extensibility means
editing well-factored Go packages under code review. Skills are progressively
loaded instructions, not executable plugins. A small fixed hook surface may be
considered, but it should not become an alternate extension runtime.

### Not an MCP aggregation layer

Orrery supports a gateway or a small number of direct MCP servers. It does not
aggregate hundreds of tools, rank tools across servers, or reproduce a
gateway's discovery and authentication responsibilities. Tool-list changes are
deferred to cache-safe boundaries.

### Not a work-tracking or collaboration suite

The harness may consume issue details through configured tools and emit links,
artifacts, events, and results. It does not aim to replace issue trackers, code
hosting, notifications, human collaboration, or higher-level job scheduling.
Embedding systems should consume the typed contract instead of being recreated
inside the harness.

### Not a complete IDE

Read-only LSP navigation is in scope because it improves agent performance.
Editor buffers, arbitrary LSP edits, graphical debugging, a DAP frontend,
browser automation, and a general project UI are not core goals. AST editing
remains a possible later optimization only if hashline miss-rate evidence
justifies the complexity.

### No silent filesystem rewind

Checkpoints, forks, and restore operate on conversational state. Source-control
or workspace rollback must be an explicit, reviewable operation. Restoring a
conversation must never unexpectedly discard a user's files.

### No transcript-only state

The transcript is model context, not durable orchestration state. Jobs,
questions, plans, checkpoints, outcomes, and artifacts belong in the store and
workspace metadata.

### No routing churn for appearance's sake

Orrery does not route on a timer, per token, or merely to demonstrate savings.
Warm-prefix continuity and steady mechanical progress should usually defeat a
marginal theoretical model advantage.

### No automatic authority expansion

Agents and workers stay inside assigned workspaces and configured services.
They do not search neighboring checkouts, clone unrelated repositories,
discover credentials, or widen network access unless the task and deployment
explicitly provide that scope.

### No online self-training

The running harness records learning-ready outcomes but does not train or
promote policies online. Policy training, evaluation, and rollout are offline,
versioned, and reviewable.

### No proprietary public corpus

The public repository must not contain private source, issue references,
customer identifiers, credentials, internal URLs, or production transcripts.
Public benchmarks use synthetic or otherwise redistributable material.

### No universal platform support

Windows support is not currently a goal. Browser automation, graphical
debugging, and native AST editing are named future possibilities rather than
implicit requirements.

## Evolving the charter

These boundaries are defaults, not a ban on evolution. A proposal that changes
one should state the new user value, maintenance and security cost, cache and
routing implications, telemetry needed to judge it, and the old boundary it
supersedes.

Two earlier boundaries have already evolved deliberately: Orrery now supports
read-only LSP queries and stdio transports. Out-of-process recursive execution
over the typed agent contract remains separate future work. When an exception
lands, this charter and the architecture document should change in the same
commit so obsolete non-goals do not survive as folklore.
