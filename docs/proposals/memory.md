# Proposal: durable memory, better compaction, and Jev

**Status:** implemented memory behavior with remaining design ideas identified separately.
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

## Implemented current behavior

The memory layer is implemented in [`internal/store/memory.go`](../../internal/store/memory.go), [`internal/store/memory_extraction.go`](../../internal/store/memory_extraction.go), [`internal/core/memory.go`](../../internal/core/memory.go), and [`internal/core/memory_extraction.go`](../../internal/core/memory_extraction.go). The future ideas below are not a claim that every part of the original proposal has shipped.

### Workspace records and lifecycle

Memory lives in local SQLite under a stable workspace identity, not under an owning session. It can therefore be reused across sessions in the same workspace without crossing workspace boundaries. User-wide personalization is not supported. Session history remains the evidence source, and memory remains separate from task checkpoints and authoritative continuation/todo state.

- Supported kinds: `fact`, `command`, `decision`, `preference`, `lesson`.
- Supported provenance: `user`, `instruction`, `observed`, `extracted`. Migration normalizes unknown legacy kinds to `fact` and unknown legacy provenance to `observed`.
- Lifecycle statuses: `pending`, `active`, `superseded`, `expired`, `deleted`. Evidence references are bounded pointers, not copied transcripts.
- Memory listing sweeps elapsed expiry timestamps on pending/active records into the persisted `expired` status. Expired records are excluded from active selection; expiry is not merely a read-time filter. `retain_days: 0` means no automatic expiry.
- The root session's `memory` tool supports `inspect`, `propose`, `confirm`, `correct`, and `forget`. Confirmation, correction, and forgetting require explicit user instruction; they are not authority for the model to silently approve its own suggestions. Corrections supersede old records, preserving the relationship for audit. Forgetting tombstones the record, excludes it from future retrieval, and marks pinned memory stale for the next refresh boundary.

Never promote private chain-of-thought, credentials, raw customer data, or unverified speculation into reusable memory. Retrieved memory is untrusted data, not instructions; current repository evidence and explicit user corrections take precedence.

### Extraction, triage, and promotion

The parent extracts bounded, evidence-backed candidates from **root-session evidence only** after successful requests, with startup catch-up from a persisted per-session watermark. Compaction can also produce candidates alongside its summary. Extraction is not a shutdown operation. Workers cannot propose typed memory candidates, and there is no direct worker-result extraction path; neither read nor shared-write worker results are a supported memory-candidate channel.

Extracted candidates start as pending proposals with `extracted` provenance. Jev supplies a bounded triage signal, not prose or storage mutations:

- A successful triage score **below 0.5** drops the candidate.
- Missing credentials, failed calls, or missing/invalid answers leave candidates pending; failure is not permission to promote.
- With successful triage, repeated observations in **two distinct sessions** can promote a matching proposal to active. Repetition within one session does not satisfy this rule.
- Explicit user confirmation can activate a pending proposal. `auto_commit: false` is the default: it disables immediate automatic activation, not the successfully triaged two-session promotion path. Opting into `auto_commit` allows immediate activation after successful triage.

Pending proposals are surfaced through the **next request context**, explicitly labeled as **untrusted proposals, not established facts**, so the agent can offer them for confirmation. They are separate from the active-memory injection set. Proposal event notices carry record IDs, not memory text; the notices alone are not the proposal presentation channel.

### Retrieval, injection, and configuration

Only active, unexpired records are eligible for the pinned active-memory set. Injection defaults on and is bounded by **8 records / 1200 estimated tokens**. The selected set is pinned across ordinary turns and refreshed at declared cache-safe boundaries, with invalidation for mutations. Memory/store failures are best-effort rather than a reason to fail a coding session.

Current defaults in [`internal/config/config.go`](../../internal/config/config.go) and [`orrery.example.yaml`](../../orrery.example.yaml):

```yaml
memory:
  inject: true                     # bounded active-memory injection
  auto_commit: false               # no immediate automatic activation
  max_records: 8
  max_tokens: 1200
  max_record_bytes: 2048
  retain_days: 0                   # no automatic expiry
  jev:
    selection: false               # optional memory_select shadow observations
    compaction_benefit: false      # independent compaction_benefit observations
    timeout: 250ms
    max_candidates: 12
```

Derivation, retrieval, and storage are always on. Set `memory.inject: false` to omit active memory from prompts; this does not disable extraction or storage. The legacy `memory.enabled` key is accepted but ignored. The unused `memory.shadow` setting has been removed and is rejected by strict config decoding. This does **not** remove `jev.shadow` or Jev's per-site shadow observations.

Memory-specific Jev selection and compaction-benefit observations are independently gated and reuse existing Jev credentials/base URL/model. Limits are clamped in code. These shadow observations do not change deterministic lexical/recency ranking or current compaction policy. Extraction triage is live and has a conservative fallback: candidates remain pending rather than being promoted.

### Events and context boundaries

Memory events record IDs, bounded decision metadata, status transitions, and evidence pointers, not copies of memory text. Retrieval records selected IDs and budget/cache information; mutation events accompany record changes. Proposal notices identify proposals without exposing their text. The next request's proposal context provides the bounded untrusted proposal bodies separately.

Keep the stable prompt prefix separate from volatile memory context. Jev cannot write/delete records or directly trigger compaction: orchestration enforces scope, expiry, budgets, and lifecycle rules. Root and worker sessions still share their existing typed task/event/result contract ([architecture](../architecture.md), [design charter](../design.md)); that contract does not add a worker memory-candidate field.

## Future ideas and evaluation

The following are design directions, not additional current defaults or promised features:

- **Compaction improvements:** evaluate earlier clearing of stale reproducible tool output, richer versioned checkpoint fields, and benefit thresholds while retaining synchronous validated commit, the recovery anchor, a recent exact tail, and the unconditional token-pressure safety path. Do not duplicate authoritative `Continuation`/`Todos` state or delete original evidence history.
- **Richer memory policy:** evaluate contradiction/currentness signals, progressive disclosure through evidence pointers, and measured retrieval thresholds. Typed Jev confidence is a signal, never truth; deterministic safety checks remain authoritative.
- **User-wide memory:** consider only after an explicit consent, identity, and cross-workspace isolation design. Current memory is workspace-scoped.
- **Worker candidate channels:** any future typed channel would need a separate contract and evidence validation. It is not implemented; the current parent extracts root evidence only.

Evaluate task success and recovery after compaction, stale/contradictory retrieval, privacy leakage, memory corrections/deletions, token/cache effects, latency/cost, and false promotions. Compare optional Jev policies independently against deterministic behavior using synthetic public-safe fixtures and long-horizon traces. Token savings without preserved success and recovery are not a win. No claim is made here that Jev has already improved Orrery's memory or compaction quality.

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
