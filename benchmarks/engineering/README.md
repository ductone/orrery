# Engineering benchmark

This replay set measures whether Orrery can diagnose, edit, verify, and finish small software-engineering tasks. Every case runs in a temporary copy of a synthetic fixture, so repeated runs are isolated and do not mutate the checked-in source.

The first ten cases cover retry boundaries, immutable label merging, a concurrent counter, a multi-file rename, rune-aware truncation, wrapped errors, quoted configuration values, deduplication performance, documentation correction, and a diagnose-only question. Five harder cases follow, for 15 in total: a cross-package cache-key bug, cursor pagination across three packages, a concurrency fix whose invariant spans two methods (graded with `-race`), an interface-extraction refactor that must preserve error and numbering behaviour, and a quadratic-to-linear speedup that must keep its ordering and tie-breaking rules. Each fixture is a separate Go module, excluded from root `go test ./...` by its own `go.mod`.

A case may set `hidden` to a directory (relative to `cases.jsonl`) of grading files. They are copied over the workspace after the agent finishes and before acceptance runs, so the agent never sees or edits them. The five harder cases use it to check spec edge cases the visible tests do not cover; their fixtures' visible tests are intentionally incomplete and some fail on the unmodified fixture where the task describes a bug.

The diagnose-only case passes unchanged: its checksums reject edits to the source and tests rather than requiring a code fix. Acceptance does not grade the written diagnosis or detect newly added files. Its checksum command requires `sha256sum`.

Run it with a configured model provider:

```sh
orrery benchmark --set benchmarks/engineering/cases.jsonl \
  --policy=v1 --output=.orrery/benchmarks/latest.json
```

Compare a candidate against a saved baseline:

```sh
orrery benchmark --set benchmarks/engineering/cases.jsonl \
  --policy=candidate \
  --baseline=.orrery/benchmarks/baseline.json \
  --output=.orrery/benchmarks/candidate.json
```

The report tracks pass rate, cost per successful case, token use, median and p95 latency, tool-error rate, edit retries, verification, and independent review. A comparison exits with code 4 when pass rate falls below 97% of the baseline. Benchmark reports live under `.orrery/` and are intentionally not committed: they may contain model output or local paths.

Fixtures in this directory are synthetic and public-safe. Do not add proprietary source, private issue text, customer data, credentials, internal URLs, or captured production transcripts.
