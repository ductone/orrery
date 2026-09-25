# Terminal UI

`orrery tui` is an interactive terminal client for exactly one session. Like the web UI, it is a transport adapter: it renders the session's durable event log and sends messages through the same idempotent continuation path, so everything it shows can be replayed and everything it does can be audited.

```sh
# Embedded engine: start a session in the current directory
./orrery --config orrery.yaml tui "Fix the failing tests"

# Attach to (or resume) a session
./orrery --config orrery.yaml tui --session SESSION_ID

# Attach to a running `orrery serve` instead of embedding the engine
./orrery tui --server http://127.0.0.1:7433 --external-id TASK_ID
```

The session in scope is, in order: `--session`; the session bound to `--external-id` (created from the first message when it does not exist yet); or a new session created from the first message. A positional prompt, or `-p`, is sent as soon as the TUI starts, including when it attaches to an existing session.

## Local and remote modes

Without `--server` (or `$ORRERY_SERVER`), the TUI embeds the engine and opens the configured database, like `orrery run`. Engine, MCP, and language-server stderr goes to `.orrery/logs/tui.log`. Quitting while a turn runs stops that turn; the session is marked interrupted and resumes with the next message.

With `--server`, the TUI never opens a local store. It streams `/api/v1/sessions/{id}/events` over SSE and uses the `/api/v1` session endpoints, so a TUI and the web UI can watch and drive the same session. Remote sessions are keyed by an external identity, so remote mode requires `--session` or `--external-id`. Quitting leaves a running turn running on the server.

## Screen

Finished work is printed into normal terminal scrollback: prompts, routing decisions, tool calls with result previews, worker and review jobs, compaction and budget notices, questions, and a result card with cost, tokens, elapsed time, verification, and review status. Width changes and the expand and reasoning toggles reprint the transcript at the new settings.

The live region below it shows the running turn (phase, model, elapsed time, the tool in flight), background workers, the todo plan, queued messages, answer choices, and the composer. The footer shows the workspace, session, model and effort, token usage, spend against budget, and context use of the current model.

| Key | Action |
| --- | --- |
| `enter` | send; while a turn runs the message is queued and delivered next |
| `shift+enter`, `alt+enter`, `ctrl+j`, trailing `\` | newline |
| `esc` | interrupt the running turn |
| `ctrl+c` | clear the composer; press twice to exit |
| `ctrl+d` | exit when the composer is empty |
| `↑` `↓` | prompt history; choose an answer when the agent asks |
| `tab` | complete `/commands` and `@file` mentions |
| `ctrl+o` | expand tool output |
| `ctrl+t` | show model reasoning when the provider returns it |
| `ctrl+l` | redraw |

Commands: `/help`, `/status`, `/cancel`, `/budget <usd>`, `/compact`, `/checkpoint [label]`, `/checkpoints`, `/restore <#|id>`, `/copy`, `/expand`, `/thinking`, `/redraw`, `/quit`. `//text` sends a message that starts with a slash. Restore affects conversational state only; workspace files are never touched.

## Squire integration

The TUI implements the harness side of Squire's agent contract natively, so a Squire harness adapter can launch it in a task PTY without screen scraping. It is enabled when `$SQUIRE_TASK_ID` is set (or with `--squire`); `--squire=false` disables it.

**Launch and resume.** With `$SQUIRE_TASK_ID` set and no `--session` or `--external-id`, the session is bound to the external identity `(squire, $SQUIRE_TASK_ID)`. Relaunching the same command in the same task therefore resumes the same session, and a positional prompt on relaunch is delivered as the next message. `--incarnation` separates sessions that reuse a task id. The launch command is:

```sh
orrery --config /path/orrery.yaml tui [--server URL] [--budget USD] "initial prompt"
```

Use an absolute `database` path in the config for embedded mode, so every worktree resumes against the same store.

**Session binding.** `<dir>/pids/<pid>.json` is written atomically (write, then rename) as `{"pid", "started_at", "updated_at", "current_session"}` once the session exists, and removed on exit. `<dir>` defaults to `~/.orrery/squire` (`--squire-dir`).

**Prompt delivery.** `<dir>/ipc/<task>.sock` (directory `0700`, socket `0600`) accepts newline-delimited JSON commands. Only `prompt` is served:

```json
{"id": "1", "type": "prompt", "message": "Also add a regression test"}
{"id": "1", "type": "response", "command": "prompt", "success": true}
```

A response is written after the engine accepted the message: it starts a turn, is queued behind the running turn, or creates the session when none exists. Failures carry `"success": false` and an `error`. Messages from the socket are always literal text, never slash commands, and never touch a composer a human may be editing. Bracketed paste followed by carriage return also works: paste lands in the composer and `enter` sends it.

**Observation.** `<dir>/sessions/<session>.jsonl` mirrors the session's event log, one event envelope per line (the same objects as the SSE `data` field), rewritten from the start whenever the TUI binds. `usage.reported` carries per-call tokens and `cost_usd`; `session.terminal` carries the `TaskResult`. The terminal window title is `orrery · <title> · <state>`, and a running turn sets the OSC 9;4 indeterminate progress indicator. On screen, the composer line starts with `❯ `, a running turn shows `esc to interrupt`, and a question with choices shows `enter to select`.

**Cancellation.** Closing the PTY ends the TUI. In embedded mode that stops the running turn; in remote mode the turn continues on the server and can be cancelled with `POST /api/v1/sessions/{id}/cancel`.
