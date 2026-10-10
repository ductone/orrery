# Review benchmark set

Measures whether Orrery's review cascade (Jev gate, light reviewer, full
review) rejects buggy changes and approves clean ones. There are 14 cases:
8 with a real correctness bug and 6 correct changes (docs-only, small feature,
behaviour-preserving refactor, legitimate test rewrite, correct concurrency,
tricky-but-correct boundary handling).

## Layout

- `fixtures/<case>/` - a self-contained Go module (standard library only) whose
  `go test ./...` passes.
- `changes/<case>/` - only the files that differ after the change, at the same
  relative paths with full contents. Copy them over the fixture (overwrite
  same-path files, add new ones) to form the proposed change.
- `cases.jsonl` - one case per line: `name`, `task` (the request the change
  claims to fulfil), `fixture`, `change`, `bug` (expected verdict is reject
  when true), `why`, and `commands`.

Bug cases have bugs that the visible tests do not catch: `go test ./...` passes
on the changed code, so a reviewer cannot rely on the tests alone.

## Run

```
orrery benchmark --review --set benchmarks/review/cases.jsonl --output .orrery/benchmarks/review.json
```

## Public-safe rule

Everything here is synthetic. Do not add proprietary code, credentials, or
internal URLs.
