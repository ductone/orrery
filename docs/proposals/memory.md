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

## Open questions

- How should a workspace-scoped memory root be added to the existing session-rooted SQLite schema, including migration, deletion, and workspace identity?
- Which checkpoint fields add recovery value without duplicating the authoritative continuation ledger and todo state?
- Should inferred memory ever be committed automatically, or should first release only persist user-confirmed facts and evidence-backed lessons?
- Which Jev question and confidence threshold improves retrieval enough to justify its remote call and privacy implications?
- What exact cache/token instrumentation is available at each compaction boundary to predict useful compaction timing?

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
