# Orrery

Orrery is an opinionated Go agent harness that chooses models inside the agent loop. Routing accounts for task phase, progress and failure signals, compatibility constraints, and the real cost of abandoning a warm prompt cache.

The project is under active construction. Its contract is one binary, one strict YAML config, a checked-in model catalog, durable SQLite state, built-in coding tools, in-process worker jobs, MCP clients, a local SSE web UI, and routing telemetry suitable for training a later learned policy.

## Build

```sh
go build ./cmd/orrery
cp orrery.example.yaml orrery.yaml
./orrery
```

Provider keys may be literal strings or `!cmd <command>` values. Secret commands are executed at startup and only their trimmed stdout is retained.
