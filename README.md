# Forge

Forge is a local, single-binary AI coding agent harness. It serves an embedded
web UI, keeps all state in local SQLite, and talks to OpenRouter over SSE. No
CGO, no Node build step, no external runtime dependencies.

## Requirements

- Go 1.23+ to build
- An OpenRouter API key

## Build & run

```bash
make build      # CGO_ENABLED=0, stripped static binary at ./forge
make run        # build, then start and open the browser
make test       # unit/integration tests
make vet
```

Or directly:

```bash
./forge --port 7890 --no-browser
```

Flags: `--host`, `--port`, `--no-browser`, `--debug`, `--debug-llm`, `--log-file`, `--version`.
Configured host/port are used when the flags are omitted.

## Files

| Path | Purpose |
| --- | --- |
| `${FORGE_HOME:-~/.forge}/config.json` | Configuration (created on first run) |
| `${FORGE_HOME:-~/.forge}/forge.db` | SQLite database (projects, sessions, messages, checklists, repo outline, symbol index) |
| `${FORGE_HOME:-~/.forge}/forge.log` | Application log (rotated to `forge.log.1` at 10 MB) |

Both the directory (0700) and the files are created automatically. `FORGE_HOME`
overrides the directory, which is handy for testing without touching real data.

An existing `forge.db` from an incompatible schema is moved aside to
`forge.db.legacy-<timestamp>` instead of being overwritten or crashing startup.

## First steps

1. Start Forge and open the UI.
2. Click the gear (bottom of the project rail) and paste your OpenRouter API key.
3. Click **+** to register a local project directory.
4. Pick **PLAN** or **BUILD** and start chatting.

Sessions are stored per project; open tabs are restored after a reload.

## Modes

| Mode | Tools |
| --- | --- |
| **PLAN** (read-only) | `read_file`, `grep_search`, `symbol_lookup`, `list_directory`, `git_diff`, `update_checklist` |
| **BUILD** | all of the above, plus `patch_file`, `write_file`, `bash` |

At the start of a new task the agent records it in the checklist before reading
files or running any other tool, and keeps the list updated as it works.

PLAN is a bounded research turn. It has its own budget (`plan_rounds` and
`plan_tool_calls`, default 16 rounds / 64 calls, max 24 / 200) and never
auto-continues. When the budget is reached, Forge requires the final plan. It
gives the model up to three final attempts, rejects empty answers and
tool calls written as text (`<tool_call>`, XML or JSON), and removes the tool
definitions entirely on the last attempt. The current turn's tool results are
kept unpruned and PLAN never lowers the per-result cap below 8,000 characters
(the shipped default is 16,000), so the model can write the plan from what it
already inspected instead of re-reading files. BUILD keeps the normal
checkpoint/auto-continue behavior.

### Token-saving behavior

- **Symbol first.** When the agent knows a declaration name, `symbol_lookup`
  returns only the symbol kind, file, and line range (never the body), so it can
  jump straight to the right window. Otherwise it uses `grep_search` or the
  repository outline before calling `read_file`.
- **Bounded reads.** `read_file` requires explicit `start_line` and `end_line`
  and returns at most **200 lines** per call. Larger requests are capped and
  report the requested vs. returned range.
- **Line-numbered output.** Every line is prefixed with its real line number
  (`142 | func Save() {`) so the agent can pick precise edit locations.
- **Local repo map.** Forge scans the project with Go's AST (and fast regex
  scanners for other languages), caches symbols and line numbers in SQLite, and
  injects a compact outline (~300–500 tokens) into the system prompt. No LLM
  calls are used to build it. The same scan feeds a queryable `project_symbols`
  table with exact start/end ranges for `symbol_lookup`. The cache refreshes
  when files change and when a project is opened.

In PLAN mode a completed turn ends with a **Plan ready — Switch to BUILD** card
so it is always clear that files cannot change until you switch modes. The plan
itself is a review document, not an implementation: it uses fixed sections
(**Diagnosis & Approach**, **Key Decisions**, **Implementation Plan** grouped by
file path, optional **Open Questions**) and never contains raw code blocks, SQL,
or verbose inline signatures. If a
model returns only reasoning and no final answer, Forge expands the reasoning
card and warns you instead of leaving an apparently empty turn.

`bash` runs in the project root with a timeout (default 30 s, capped at 600 s)
and is killed as a process group on timeout or cancellation. Each call is a
fresh shell, so `cd` does not persist; when a command needs another directory,
use one absolute, quoted literal path in the same call (`cd "/abs/path" && …`).
All file tools are confined to the project directory, including symlink-escape
checks for paths that do not exist yet. Writes are atomic.

## UI

- Left rail: projects, add project, theme toggle, settings
- Top: open session tabs
- Center: chat with auto-resizing composer, **Send** and **Stop**
- Right: collapsible inspector with usage/cost, active files, checklist

Forge ships with dark and light themes. The theme can be switched from the left
rail; the choice is stored in `localStorage` and applied before the first paint
to avoid a flash. The initial theme is dark.

The right panel can be collapsed from its header and reopened from the session
bar. The active-files list is collapsed by default and remembers whether it was
open. Completed checklist items are grouped under a collapsed **Completed**
section, so the remaining work stays visible.

The inspector shows two timers. **Session time** is the accumulated wall-clock
time the agent spent working in the session, persisted in SQLite. Each assistant
response also shows the time spent on that turn, including planning, reasoning,
and tool calls. Timer values use whole seconds (`0s`, `12s`, `1m 05s`,
`1h 02m`).

The context gauge shows the current request only (prompt + response). The
**input**, **output**, and **cached** counters are cumulative: every tool round
resends the whole conversation, so they normally grow much faster than the
context gauge. Both views are expected to differ.

Every reasoning step and tool call is a collapsed accordion by default
(`🧠 Thinking…`, `📄 Reading main.go`, `⚡ Running: go test ./...`). Click to
expand inputs and outputs. Stop aborts the stream and keeps whatever text had
already arrived.

### Long turns and checkpoints

A **round** is one model call and may run several tools. BUILD checkpoints every
`max_tool_rounds` rounds (default **24**) and keeps going automatically until
`max_turn_rounds` (default **96**) when **auto-continue** is enabled. A
checkpoint never loses work: every tool result is already persisted, and a
paused turn shows a **Continue** action.

PLAN does not use checkpoints or auto-continue. It has a separate research
budget (`plan_rounds` / `plan_tool_calls`), keeps this turn's tool results
unpruned with an 8,000-character per-result floor (the shipped default is
already 16,000), and forces a final plan when the budget is spent. The final
answer must be real Markdown; empty answers, reasoning-only replies, and tool
calls written as text are rejected and retried.

A loop guard runs alongside the budget. It fingerprints each tool call together
with its result and stops the turn when the same call produces the same result
three times in a short window, or after six consecutive tool errors. Budget and
loop stops are reported as a neutral checkpoint (`limit` event), not as failures.
All limits are configurable in **Settings → Defaults**.

In PLAN mode, tools that write files or run commands are rejected **before
execution** and rendered as a `🚫 … blocked` card, so you never see a tool start
only to fail because of the mode.

### Slash commands

| Command | Action |
| --- | --- |
| `/compact` | Summarize older messages into one summary and keep the last exchange, freeing context |

### Keyboard shortcuts

| Shortcut | Action |
| --- | --- |
| `Enter` | Send message |
| `Shift+Enter` | Newline |
| `Ctrl+Shift+,` | Open Settings |
| `Ctrl+Shift+.` | Toggle PLAN / BUILD |
| `Ctrl+Shift+/` | Focus the model picker |
| `Ctrl+Shift+L` | Focus the chat composer |
| `Ctrl+Shift+↑` / `Ctrl+Shift+↓` | Previous / next user message |

All modifier shortcuts use **Ctrl+Shift** and are handled with `preventDefault`,
so they do not reuse the combinations browsers and desktops reserve for the
address bar, print, new window, new tab, close tab, settings, or help. `Ctrl` is
used on every platform (including macOS) to avoid `Cmd+?`, `Cmd+,`, and similar
system shortcuts.

## Config

`config.json` is human editable and hot-applied from the Settings modal:

```json
{
  "server": {
    "host": "127.0.0.1",
    "port": 7890,
    "auto_open_browser": true,
    "debug": false,
    "debug_llm": false,
    "log_file": ""
  },
  "providers": {
    "openrouter": { "api_key": "sk-or-v1-…", "base_url": "https://openrouter.ai/api/v1" }
  },
  "defaults": {
    "model": "qwen/qwen-2.5-coder-32b-instruct",
    "mode": "PLAN",
    "max_file_read_chars": 25000,
    "bash_timeout_sec": 30,
    "max_tool_rounds": 24,
    "max_turn_rounds": 96,
    "plan_rounds": 16,
    "plan_tool_calls": 64,
    "auto_continue": true,
    "max_turn_cost_usd": 0,
    "history_prune_percent": 70,
    "history_prune_max_tokens": 120000,
    "history_keep_recent_rounds": 0,
    "history_max_result_chars": 16000,
    "history_max_arg_chars": 4000,
    "history_prune_chunk_rounds": 0
  }
}
```

`base_url` can point at any OpenAI-compatible streaming endpoint, which is how
the test suite and local mocks are wired up. `log_file` defaults to
`<forge home>/forge.log`; set it to an absolute path to move the log.

## Logging

Forge logs to stderr and to the log file at `INFO` and above by default. With
debug enabled it also records every HTTP request, model round, tool execution,
provider latency, and stream summary.

Enable it with any of:

- Settings → **Logging → Debug logging** (applies immediately, saved to config)
- `./forge --debug`
- `FORGE_DEBUG=1 ./forge`
- `"debug": true` in `config.json`

`--debug` and `FORGE_DEBUG=1` win over the config file, so turning the switch
off in the UI will not silence a forced debug session. The log rotates to
`forge.log.1` at 10 MB. API keys are never written to the log; at the debug
level message content is not logged, only sizes and metadata.

### LLM payload tracing (`--debug-llm`)

`--debug-llm` adds a level *below* DEBUG that records everything exchanged with
the model:

- the full request body — model, `tool_choice`, the system prompt, every message
  in the pruned history (including tool results) and the tool schemas;
- every raw SSE chunk as it arrives (content deltas, reasoning deltas,
  tool-call fragments and the final `[DONE]`);
- the assembled response — reasoning, content, tool calls with their arguments
  and token usage — plus the request id and upstream headers of the response;
- the complete provider error body before it is truncated for the UI.

Enable it with any of:

- Settings → **Logging → LLM payload tracing** (applies immediately, implies debug)
- `./forge --debug-llm`
- `FORGE_LLM_DEBUG=1 ./forge`
- `"debug_llm": true` in `config.json`

Records are labelled `level=LLM` and dumps are written as indented JSON blocks
so they stay readable. This mode **does** write prompt and tool output content to
the log, so treat the log file as sensitive. API keys and the `Authorization`
header are never written at any level, and `--debug-llm` / `FORGE_LLM_DEBUG=1`
win over the config file like `--debug` does.

## Model context windows

`GET /api/models` proxies the provider's model catalog and caches it for six
hours. The UI uses the real `context_length` for the context-window gauge and
the model picker labels, so switching models updates the denominator
immediately. If the provider catalog is unreachable, Forge falls back to a
built-in table and a heuristic (and keeps working offline).

## Billing and prompt cache

The final charge is whatever OpenRouter reports in `usage.cost` for each
request; Forge accumulates it into **session cost** and **project cost**. The
token counters are informational:

- **input (session)** is the sum of `prompt_tokens` over every request. Each
  tool round re-sends the whole conversation, so this total grows quickly.
- **output (session)** is the sum of `completion_tokens`.
- **cached (session)** is the cumulative subset of input tokens that the
  provider served from its prompt cache (`prompt_tokens_details.cached_tokens`).
  Cached input is billed at a lower rate, so the actual charge is lower than
  `input × list price`.

A cache hit requires an identical prefix (system prompt, tool definitions, and
message history) and the same upstream provider. Forge now puts volatile
content (repository outline and checklist) at the end of the system prompt so
the stable instructions stay cacheable, and the UI accumulates cached tokens
for the whole session instead of showing only the last round. Cache behavior is
still controlled by the provider; routing, short cache TTLs, and model changes
can reduce the hit rate.

## HTTP API

| Method & path | Purpose |
| --- | --- |
| `GET /api/health` | Liveness probe |
| `GET/PUT /api/config` | Read/update config (API key is masked on read) |
| `GET /api/models` | Provider model catalog with real context lengths (`?refresh=1` bypasses the cache) |
| `GET/POST /api/projects` | List/register projects |
| `PATCH/DELETE /api/projects/{id}` | Rename/recolor/move a project, or delete it with its chats |
| `GET/POST /api/projects/{id}/sessions` | List (`?all=1` includes closed)/create sessions |
| `GET/PATCH/DELETE /api/sessions/{id}` | Session detail/rename/mode/model/close |
| `POST /api/sessions/{id}/compact` | Summarize the transcript and keep the last exchange |
| `POST /api/chat/stream` | SSE agent turn (reasoning, content, tool, usage, checklist, done) |
| `GET/POST/PATCH/DELETE` on checklist routes | Manual checklist edits |

## Security notes

Forge has no authentication and can read and write the registered project
directories. It binds to `127.0.0.1` by default and prints a warning if you bind
it to a non-loopback address; only do that on a trusted network.

## Tests

```bash
make test
```

Tests run against `FORGE_HOME` temp directories and a mock SSE server, so they
never touch your real `~/.forge` or call OpenRouter.