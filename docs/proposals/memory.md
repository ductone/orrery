# Proposal: durable memory, better compaction, and Jev

**Status:** design proposal; this document does not describe implemented behavior.  
**Scope:** local-first coding sessions and workspace-scoped knowledge in Orrery.

## Summary

Treat three things as related but distinct:

1. **History** is the append-only evidence of what happened in a session.
2. **Compaction** is a context-window operation that creates a concise, recoverable working checkpoint from history.
3. **Memory** is a curated set of durable, scoped facts and lessons that may be useful in later turns or sessions.

Keep durable memory out of the transcript as the system of record. Before each model call, assemble only the useful context for that task. Improve compaction by preserving typed task state, provenance, and a recent exact tail while dropping bulky or replaceable tool output. Use Jev for inexpensive, typed judgments—such as candidate relevance, retention priority, contradiction, or compaction readiness—behind conservative thresholds and observable fallbacks. Keep summarization and memory writing with the existing language-model/tool loop; Jev does not generate prose.

## What current harnesses are doing

The public approaches converge on a few complementary mechanisms, rather than one universal “memory” feature:

- **Persistent instructions and project notes.** Claude Code loads `CLAUDE.md`/`AGENTS.md` guidance and offers auto-memory notes across sessions. Codex documents hierarchical `AGENTS.md` discovery and precedence. These are explicit, human-readable, scoped knowledge; they are not a substitute for the current task transcript. [Claude Code memory](https://docs.anthropic.com/en/docs/claude-code/memory), [Codex `AGENTS.md`](https://developers.openai.com/codex/guides/agents-md).
- **Thread history and longer-term memory as separate state.** LangGraph distinguishes thread/checkpoint state (short-term, keyed to a conversation) from cross-thread long-term memory, often represented as namespaced records and retrieved as needed. The OpenAI Agents SDK similarly offers session history backends and a compaction wrapper; its docs call out limiting fetched history, choosing one continuation mechanism, and the latency trade-off of automatic compaction. [LangGraph memory](https://docs.langchain.com/oss/python/langgraph/add-memory), [Agents SDK sessions](https://openai.github.io/openai-agents-python/sessions/).
- **Agent-managed external notes and files.** Letta describes persistent agent state and core memory that can be edited while the complete interaction history remains persisted outside the active context. MemGPT frames this as virtual context management across memory tiers. [Letta memory](https://docs.letta.com/guides/agents/memory), [MemGPT paper](https://arxiv.org/abs/2310.08560).
- **Context as a budget, with progressive disclosure.** Anthropic recommends compacting long traces while retaining important decisions and recent working files, using structured notes for long-horizon work, and loading large information sources just in time rather than stuffing them into every prompt. It also identifies tool-result clearing as a low-risk way to reclaim space. [Effective context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents).
- **Typed model judgments are a different primitive from generated text.** Jev/TypeSafe exposes Noul (a 0–1 truth-like value), Choice, and Score; Choice and Score also return probabilities/confidence. These primitives are intended for small, decomposable decisions. That makes Jev a plausible policy input for retrieval and compaction—not a replacement for an LLM summarizer. [TypeSafe / Jev](https://docs.typesafe.ai/introduction).

These systems are not directly comparable products: some persist conversation state, some expose project guidance, and some provide agent-editable memory. The common design lesson is to separate full history from a bounded active context and from a smaller, explicitly retrieved knowledge layer. More context is not automatically better; retrieval and compact summaries need to preserve evidence and remain inspectable.

## Orrery-specific starting point

Orrery already has several pieces to build on:

- Root and worker sessions share a typed task/event/result contract; jobs, questions, plans, checkpoints, and artifacts are durable state rather than transcript recollections ([architecture](../architecture.md), [design charter](../design.md)).
- Compaction is already an explicit cache event with a recovery checkpoint, validated continuation anchor, recent exact tail, and synchronous commit. Phase boundaries are gated; estimated input beyond three quarters of the context window takes the token-pressure path without that gate. The main opportunities are earlier tool-result clearing, a richer checkpoint schema, and replacing fixed heuristics with an evaluated benefit signal ([compaction gate](../../internal/core/compaction_gate.go), [compaction implementation](../../internal/core/compaction.go)).
- The Jev client already supports batched typed questions and deliberately does not retry. Orrery uses Jev in both live policy paths and shadow evaluation; memory retrieval and compaction would be new policy surfaces and should earn promotion independently ([Jev client](../../internal/jev/jev.go), [Jev review integration](../../internal/review/jev.go)).

The proposal should extend these mechanics rather than create an independent agent runtime, a vector database requirement, or a second source of task truth.

## Proposed model

### 1. Keep history, checkpoint, and memory distinct

- **Session history:** preserve the existing durable event/transcript record. Compaction must not delete the audit/recovery source merely because old messages leave model context.
- **Task checkpoint:** a versioned, replaceable working summary for continuing one task. Keep goal and acceptance criteria, completed work and evidence, current plan/next action, unresolved questions and risks, important decisions with rationale, and paths/identifiers needed to reopen evidence. Include a short exact recent tail where wording/tool sequence matters. The existing harness-owned continuation ledger and todo state remain authoritative over best-effort summary prose.
- **Memory records:** small reusable facts or lessons, each scoped to a workspace/project or user preference as appropriate. Keep evidence references, creation/update time, provenance (user, checked-in instruction, observed event, or model suggestion), confidence/status, and optional expiry. Prefer a path/query/link to large evidence over copying it into the record.

Never promote private chain-of-thought, credentials, raw customer data, or unverified model speculation into reusable memory. Treat retrieved memory as untrusted data, not instructions. Explicit user corrections should supersede conflicting inferred notes; retain the superseded record or provenance for audit rather than silently rewriting history.

### 2. Make compaction checkpoint-first and recoverable

Orrery already compacts synchronously at phase boundaries and a fixed token-pressure threshold, validates its continuation anchor, commits before replacement, and retains a recent tail. Improve this by estimating expected benefit (tokens reclaimed and evidence preserved) instead of relying only on fixed thresholds. Keep the context-pressure path unconditional. At a safe boundary, create a checkpoint from typed task state and recent history; validate required fields and references; then commit it durably before replacing active prompt history. If summary generation or validation fails, retain the old active context and continue safely or report that compaction could not run.

Use a loss-minimizing order:

1. Remove or truncate stale, reproducible tool output while retaining a pointer/hash and the meaningful result (tool-result clearing is safer than paraphrasing everything).
2. Fold completed turns into the structured task checkpoint; keep recent messages and active tool cycles intact.
3. Preserve evidence references, decisions, constraints, open questions, and failure/recovery details. Summarize claims, not away their source.
4. Rebuild the next context in cache-aware order: stable instructions and schemas, authoritative durable task state, session-pinned memory, compact checkpoint, then a bounded recent tail. Refresh retrieved memory only at a declared cache-safe boundary (session start, phase transition, or compaction), record it as a cache event, and price the cold suffix rather than assuming stable ordering makes volatile content free.

Keep compaction synchronous, idempotent, and versioned. Record input range/checkpoint version, output, trigger/reason, token estimates, and validation outcome. The continuation ledger remains authoritative if summary prose conflicts with it. A checkpoint is not a workspace rollback and must never change source files.

### 3. Add a small retrieval-first memory layer

Extend Orrery’s existing local SQLite store with a workspace-scoped root that can outlive any one session; current state is session-rooted, so ownership, migration, and deletion semantics need an explicit schema design. Do not require a separate embedding service. Initial memory categories:

- workspace facts and conventions that are not already reliably discoverable from checked-in instructions;
- decisions and their evidence pointers;
- recurring user preferences only when explicitly expressed or confirmed;
- verified lessons from outcomes, such as a test command that works or a known failure mode.

At session start, select a small, token-capped set of scoped, high-confidence records and pin that set until an explicit cache-safe refresh boundary. Retrieve candidates using scope, recency, lexical/path match, and status; offer progressive disclosure via references instead of injecting whole notes. Put pinned memory after the stable prompt prefix so a refresh invalidates only the volatile suffix, and record the estimated cache cost. Give users a way to inspect, correct, forget, and disable memory. A missing, corrupt, or unavailable memory store must not block a session.

Start without automatic cross-workspace/user personalization. Scope and authorize records explicitly; avoid leaking one repository’s or user’s information into another. Do not save secrets. Record where each injected fact came from, and favor current repository evidence over stale memory when they conflict.

### 4. Use Jev as a bounded policy signal

Ask atomic, independently answerable questions in one bounded Jev batch against compact states containing candidate notes, task/query, provenance, recency, and alternatives—not the entire transcript. Cap candidate count, bytes/tokens, one batch per refresh boundary, and timeout; if any answer or the batch fails, discard the Jev result and use deterministic ranking. Example questions:

- Choice: is a candidate relevant to this task (`include`, `maybe`, `exclude`)?
- Score: how well supported and still current is this candidate, using named levels?
- Choice: does this candidate conflict with current evidence (`conflicts`, `unclear`, `consistent`)?
- Choice/Score: is predicted context pressure high enough to compact now, given estimated remaining tokens and the value of retaining the recent tail?

Use confidence/probabilities as one signal, never as truth. Deterministic checks enforce scope, expiry, token caps, and safety. Low confidence, timeout, malformed/missing answers, or missing credentials discard the entire optional batch and fall back to lexical/recency ranking and the current compaction policy. Run Jev asynchronously before the next cache-safe boundary or under a strict deadline; never extend the coding turn to wait for it. Jev must not write/delete memory or directly trigger compaction.

Although Orrery already ships other live Jev policies, memory selection and compaction timing have different information-loss, privacy, cache, and continuity risks. Start these new decisions in shadow mode and promote each independently only after evaluation. Keep decisions explainable: candidates selected, reason, confidence, fallback, and token/cost/latency effect. Ask one question per meaningful dimension and combine dimensions in code rather than posing one opaque “what should I remember?” prompt.

## End-to-end flow

1. **Start/resume:** load stable project instructions and authoritative durable task state; select and pin a small, token-capped memory set after the stable prefix. Optionally have one bounded Jev batch rank candidates in shadow mode. Record the retrieval and cache estimate.
2. **Work:** append events normally. Keep plans, worker results, review outcomes, and artifacts in their durable typed stores. Read workers may consume the parent’s pinned memory but cannot write memory. Shared-write workers may return evidence-backed candidates in their typed result; only the owning parent/session commits them at a safe boundary.
3. **At pressure/boundary:** estimate tokens, evidence loss, and cache impact; decide whether to clear tool output or checkpoint/compact. Jev may supply a typed benefit signal, but safety and context-limit logic remain deterministic.
4. **Checkpoint:** summarize relevant events into versioned structured state, validate, persist, then rebuild active context with the recent tail. Preserve original event history, and preserve continuation/todo state as the authority over summary prose.
5. **At completion or idle:** identify candidate reusable lessons from verified outcomes. Keep candidates pending/low-confidence unless grounded in user-provided preference or observable evidence; allow review/correction. Update or expire scoped memory atomically.

## Rollout and evaluation

1. **Baseline:** record current compaction inputs/outputs, trigger, tokens, latency, and subsequent recovery failures on synthetic traces.
2. **Compaction delta:** add early tool-result clearing, evidence pointers, explicit checkpoint fields not already represented by the continuation ledger, and an evaluated benefit trigger. Compare against the shipped validated, synchronous compaction path without changing model routing.
3. **Memory in read-only/shadow mode:** derive and retrieve candidate records without injecting them or changing behavior. Inspect false positives, stale/conflicting notes, and privacy leakage.
4. **Jev shadow evaluation:** compare Jev’s typed choices with deterministic ranking and human-reviewed synthetic fixtures. Capture confidence, latency, errors, and provider cost; ensure failure has no user-visible delay.
5. **Opt-in injection:** expose inspect/forget controls and enable scoped memory behind an explicit setting. Promote Jev into ranking only if it measurably improves retrieval/continuity.

Measure task success and recovery after compaction, information-loss errors, stale/contradictory retrieval, tokens and cache reuse, added latency/cost, compaction frequency, and memory corrections/deletions. Compare on long-horizon coding traces with a frontier-pinned quality baseline; a token reduction without preserved success is not a win. Use synthetic, redistributable fixtures in the public repository.

## Code-anchored implementation plan

This section turns the proposal into an implementation sequence; all schema, settings, and events below are proposed, not existing behavior. File references point to the current extension points. Keep the work inside the existing store/session and core orchestration rather than introducing a new runtime or service.

### Store schema and migration

Extend `(*Store).migrate` in `internal/store/store.go`, following the package's existing additive `CREATE TABLE IF NOT EXISTS` / `ensureColumn` SQLite migration style. The current durable records (`Messages`, `Session`, `Todos`, `Continuation`, `WorkItems`, checkpoints, and compaction state) are session-oriented; add an explicit workspace root that can be shared by sessions without making a session the owner of durable memory:

```sql
CREATE TABLE workspaces (
  workspace_id TEXT PRIMARY KEY,
  identity_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
 );
CREATE TABLE memory_records (
  memory_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(workspace_id),
  scope TEXT NOT NULL CHECK (scope IN ('workspace', 'project', 'user')),
  kind TEXT NOT NULL,
  text TEXT NOT NULL,
  provenance TEXT NOT NULL,             -- user, checked-in instruction, observed event, or model suggestion
  confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'superseded', 'expired', 'deleted')),
  evidence_refs TEXT NOT NULL DEFAULT '[]', -- bounded JSON references, not copied source material
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  expires_at TEXT,
  superseded_by TEXT REFERENCES memory_records(memory_id)
 );
CREATE INDEX memory_records_lookup ON memory_records(workspace_id, scope, status, updated_at);
```

Use a stable workspace identity resolved by the application, not a session ID; keep canonical-path or equivalent identity handling in one place and do not emit it in telemetry. `scope='user'` is reserved but disabled until an explicit cross-workspace identity/consent design exists. Enforce same-workspace supersession in store methods. Treat `deleted` as a tombstone (with content removal according to the forget contract), not a retrievable record. Evidence refs should identify a session/event or repository path and optional range/hash, never contain credentials or raw transcript dumps. Migration tests should cover empty and existing databases, reopening, workspace isolation, expiration, supersession, and forget/delete behavior. SQLite/store errors are best-effort: callers continue with no memory rather than failing session startup or a turn.

Keep task checkpoint storage separate. Extend the existing compaction checkpoint representation in `internal/core/compaction.go` only for typed recovery fields not already authoritative in `Continuation`/`Todos`; add a checkpoint schema version and input-range/evidence references. Preserve its synchronous validate-then-commit behavior (`ApplyCompaction`) and current fallback path. Do not migrate transcript history into `memory_records`.

### Configuration surface

Add a `MemoryConfig` alongside `JevConfig` in `internal/config/config.go` and document defaults in `orrery.example.yaml`. Suggested shape:

```yaml
memory:
  enabled: false                   # master opt-in; disabled until controls/retention ship
  shadow: true                      # retrieve/derive and measure without prompt injection
  max_records: 8
  max_tokens: 1200
  max_record_bytes: 2048
  retain_days: 0                    # 0 means no automatic expiry
  auto_commit: false                # suggestions stay pending absent explicit confirmation
  inject: false                    # independently opt in after shadow evaluation
  jev:
    selection: false                # memory_select shadow/live policy switch
    compaction_benefit: false       # separate shadow/live policy switch
    timeout: 250ms
    max_candidates: 12
```

Names and exact defaults can be aligned with existing config conventions, but keep `enabled`, injection, automatic commit, and each Jev surface independently gated. Reuse existing Jev credentials/base URL/model (`JevConfig` and current `answer_check.go` pattern); do not create another credential path. Clamp all limits in code. Missing credentials, disabled Jev, timeout, malformed response, or store failure selects deterministic lexical/recency ranking and current compaction behavior. Shadow observations should use the existing `internal/core/shadow.go` / `shadow_observations` pattern and existing Jev site naming conventions; do not log memory text or workspace identity in observations.

### Events and prompt/cache boundaries

Emit additive, versioned events through existing store/event mechanisms; events describe decisions and references, not duplicate the durable record body:

- `memory.retrieved`: workspace/session reference, selected memory IDs, deterministic/JeV decision source, bounded reason codes, count and estimated token cost, refresh boundary, and outcome (including empty/error fallback). Store only when memory is enabled; omit note text and secrets.
- `memory.committed`: memory ID, kind/scope, provenance, status transition, evidence-ref identifiers, and whether user-confirmed. Commit/update/supersede atomically with the event or use an outbox/transaction mechanism supported by the store so event and record cannot disagree.
- `memory.updated` / `memory.forgotten`: IDs and status transition for inspectable corrections/deletion; ensure forgetting removes the content from future retrieval and prompt caches.
- Extend `context.compacted` metadata in `internal/core/compaction.go` with checkpoint schema/version, input range, trigger/reason, token estimates, tool-output bytes cleared, evidence-ref count, validation result, and memory/cache boundary identifier. Keep existing `context.compaction_failed` behavior and ensure no event claims a commit before the checkpoint transaction succeeds.
- Record each memory refresh as a cache event at session start, phase transition, or compaction. Pin its selected IDs for that context epoch; never silently re-retrieve every turn. In `internal/core/engine.go`, add a distinct volatile request segment adjacent to `DurableSpec`: after stable `System` instructions/bootstrap and before `DurableSpec`, `Plan`, and conversation `Messages`. A refresh should invalidate only the volatile suffix rather than changing stable prompt assembly.

Jev is called only on bounded compact candidate descriptions through the existing batched `jev.Noul` / `Choice` / `Score` client. Add independent site identifiers such as `memory_select` and `compaction_benefit` to shadow evaluation. Jev results are observations in shadow mode: they cannot write records, replace deterministic eligibility checks, or call compaction. Live promotion remains independently feature-gated and timeout-bounded.

### Phased implementation slices

1. **Instrument baseline and protect invariants.** Add synthetic compaction fixtures and measurements around the existing gate (`internal/core/compaction_gate.go`), `Compact`/`ApplyCompaction` path, and `context.compacted` events. Document/assert continuation and todo authority, idempotency, synchronous validated commit, and no session failure on optional persistence errors. No behavior change.
2. **Checkpoint/tool-output delta.** Implement bounded stale tool-result clearing with evidence pointers, versioned typed checkpoint fields, and cache-aware context rebuild in `internal/core/compaction.go`; retain the current pressure path and gate in `compaction_gate.go`. Add recovery/idempotency/fallback tests in `internal/core/compaction_test.go`; compare against the baseline before adjusting thresholds.
3. **Workspace store and user controls, dark.** Add the migration and store APIs/tests in `internal/store/store.go` for scoped records, pending/active lifecycle, provenance, evidence, expiry, supersession, inspect, correct, forget, and workspace isolation. Keep APIs unavailable to prompt construction initially; failures are non-fatal. Never automatically persist model speculation.
4. **Deterministic memory shadow.** Add bounded candidate extraction/ranking and retrieval/cache-event recording in core orchestration, guarded by config. Compare candidate sets to fixtures and inspect privacy, stale/conflicting notes, cost, and errors; do not inject notes or alter the response.
5. **Jev shadow, then independent opt-in.** Add batched typed ranking and benefit questions using current Jev client/config and existing shadow observation mechanism. Enforce candidate/payload caps, one batch per boundary, deadline, and whole-batch deterministic fallback. Evaluate each surface independently; only later expose separate live gates, after fixture and long-horizon trace evidence supports promotion.
6. **Opt-in injection and verified learning.** Add cache-safe prompt assembly, explicit user inspect/correct/forget/disable controls, and optional injection behind separate settings. Begin with user-confirmed facts and evidence-backed lessons as pending proposals; parent session is the only committer, and worker candidates require evidence. Add end-to-end tests for cross-session continuity, workspace isolation, correction precedence, forget, store failure, and unchanged session behavior when disabled.

Do not advance a phase based on token savings alone: require preserved task success/recovery and no privacy leakage or added user-visible Jev wait. Suggested unresolved decisions are limited to measured thresholds, precise checkpoint fields after a field-by-field comparison with `Continuation`/`Todos`, and the eventual consent model for user-wide memory; schema ownership, initial safety defaults, event boundaries, and rollout order are specified above.

## Open questions

- Which checkpoint fields remain genuinely absent after the phase-1 comparison with authoritative `Continuation` and `Todos`? See the checkpoint schema/migration and first two slices above; do not duplicate existing task state.
- What retrieval/compaction thresholds meet quality, latency, cache, and cost targets on synthetic and long-horizon traces? Decide only after baseline and independent Jev shadow evaluation; see rollout slices 1, 2, and 5.
- Should/when should user-wide memory be supported? The first implementation is workspace-scoped; `user` scope stays disabled until consent, identity, and cross-workspace isolation are designed and tested.
- Which records can be committed automatically? Initial defaults keep model-derived candidates pending and `auto_commit` off; evaluate user-confirmed facts and evidence-backed lessons before relaxing this rule.

## Sources and limits

Primary sources consulted for this synthesis (accessed October 2025):

- [Claude Code memory](https://docs.anthropic.com/en/docs/claude-code/memory)
- [Codex project instructions](https://developers.openai.com/codex/guides/agents-md)
- [LangGraph memory](https://docs.langchain.com/oss/python/langgraph/add-memory)
- [OpenAI Agents SDK sessions and compaction](https://openai.github.io/openai-agents-python/sessions/)
- [Letta memory](https://docs.letta.com/guides/agents/memory)
- [MemGPT: Towards LLMs as Operating Systems](https://arxiv.org/abs/2310.08560)
- [Anthropic: Effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
- [TypeSafe / Jev introduction](https://docs.typesafe.ai/introduction)

Vendor documentation changes quickly and describes each vendor’s own product, not independent comparative evaluations. The synthesis uses it to identify design patterns, not to claim that one implementation is superior. Jev’s applicability here is a proposal to evaluate; public Jev documentation establishes typed decisions, not that it has already improved Orrery’s memory or compaction quality.
