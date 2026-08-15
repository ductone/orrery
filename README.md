# Orrery

Orrery is an opinionated Go agent harness that chooses models inside the agent loop. Routing accounts for task phase, progress and failure signals, compatibility constraints, and the real cost of abandoning a warm prompt cache.

Its contract is one binary, one strict YAML config, a checked-in model catalog, durable SQLite state, built-in coding tools, isolated in-process worker jobs, MCP clients, a local SSE web UI, and routing telemetry suitable for training a later learned policy.

## Build

```sh
go build ./cmd/orrery
cp orrery.example.yaml orrery.yaml
./orrery
```

Provider keys may be literal strings or `!cmd <command>` values. Secret commands are executed at startup and only their trimmed stdout is retained.

## Commands

```sh
# Browser UI and SSE API on the configured localhost address
./orrery --config orrery.yaml serve

# CI-friendly headless task; TaskResult is JSON and status controls the exit code
./orrery --config orrery.yaml run -p "Fix the failing tests" --workspace "$PWD"

# Canonical learning dataset, with source content excluded
./orrery --config orrery.yaml export --since 24h > routing.jsonl

# Turn a completed session into a replay case, then compare policies
./orrery --config orrery.yaml eval --build-session SESSION_ID --acceptance "go test ./..." >> replay.jsonl
./orrery --config orrery.yaml eval --set replay.jsonl --policy frontier-pinned
./orrery --config orrery.yaml eval --set replay.jsonl --policy v1
```

Root and worker agents share the typed contract in [`proto/agent.proto`](proto/agent.proto). Workers default to detached Git worktrees and fall back to a copied workspace outside Git repositories. Their specs, status, and schema-validated results remain under `.orrery/jobs/` as well as in SQLite.

The built-in tool set is `read`, `search`, hashline `edit`, `exec`, background `job`, `todo`, `spawn`, `web_search`, and `fetch`. MCP tools are namespaced by server. Public fetches reject private, loopback, link-local, credential-bearing, and non-HTTP URLs.

## License

Apache License 2.0. See [LICENSE](LICENSE).
