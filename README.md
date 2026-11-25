# Forge

Forge is a local, single-binary AI coding agent harness. It serves an embedded
web UI, keeps all state in local SQLite, and talks to any OpenAI-compatible
provider over SSE — OpenRouter, Ollama, LM Studio, vLLM, Groq, DeepSeek,
OpenCode Go, or a custom gateway. No CGO, no Node build step, no external
runtime dependencies.

## Requirements

- Go 1.23+ to build
- An API key for a hosted provider, or a local OpenAI-compatible endpoint
  (Ollama and LM Studio usually need no key)

## Build & run

```bash
make build      # CGO_ENABLED=0, stripped static binary at ./forge
make run        # build, then start and open the browser
make test       # unit/integration tests
make vet
make vendor     # re-download the pinned frontend assets (only when bumping versions)
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
| `${FORGE_HOME:-~/.forge}/forge.db` | SQLite database (projects, sessions, messages, checklists, clarifying questions, repo outline, symbol index, provider profiles) |
| `${FORGE_HOME:-~/.forge}/forge.log` | Application log (rotated to `forge.log.1` at 10 MB) |

Both the directory (0700) and the files are created automatically. `FORGE_HOME`
overrides the directory, which is handy for testing without touching real data.

An existing `forge.db` from an incompatible schema is moved aside to
`forge.db.legacy-<timestamp>` instead of being overwritten or crashing startup.

## First steps

1. Start Forge and open the UI.
2. Click the gear (bottom of the project rail) → **LLM providers**.
3. Pick a preset (**OpenRouter**, **Ollama Local**, **LM Studio**, or **Custom**),
   fill in the base URL/key if needed, and click **Test connection**.
4. Click **+** to register a local project directory.
5. Pick **PLAN** or **BUILD** and start chatting.

The active provider is global; each chat can also pin its own provider from the
header selector. Model names can be overridden per chat too. Both take effect on
the next turn without restarting Forge.

Sessions are stored per project; open tabs are restored after a reload.

## Modes

| Mode | Tools |
| --- | --- |
| **PLAN** (read-only) | `read_file`, `grep_search`, `symbol_lookup`, `list_directory`, `git_diff`, `update_checklist`, `ask_user` |
| **BUILD** | all of the above, plus `patch_file`, `write_file`, `bash` |

At the start of a new task the agent records it in the checklist before reading
files or running any other tool, and keeps the list updated as it works.

PLAN is a bounded research turn. It has its own budget (`plan_rounds` and
`plan_tool_calls`, default 16 rounds / 32 calls, no upper bound) and never
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
- **Non-destructive compaction.** `/compact` stores a `compactions` row with the
  summary and the ID of the first message that still goes to the model. The next
  turns send the summary plus that tail; the transcript itself is never deleted,
  so the history stays visible and each summary renders as an expandable
  **Context compacted** card where the new window starts.
- **Local repo map.** Forge scans the project with Go's AST (and fast regex
  scanners for other languages), caches symbols and line numbers in SQLite, and
  injects a compact outline into every request as the trailing ephemeral state
  message. No LLM calls are used to build it. The same scan feeds a queryable
  `project_symbols` table with exact start/end ranges for `symbol_lookup`. The
  cache refreshes when files change and when a project is opened.
- **Project instructions, read once per chat.** The repository conventions every
  coding agent expects are picked up automatically, with no `@` attachment and
  no configuration:

  | Priority | Artifact |
  | --- | --- |
  | 1 | `AGENTS.md` |
  | 2 | `CLAUDE.md` |
  | 3 | `.cursorrules` |
  | 4 | `.github/copilot-instructions.md` |
  | fallback | `README.md`, only when none of the above exists |

  All matching agent files are captured (duplicates such as a `CLAUDE.md`
  symlink to `AGENTS.md` are dropped by content). Agent files share a 24,000
  character budget; the `README.md` fallback is capped at 8,000 and labeled as
  background documentation, because a README describes the product rather than
  prescribing how to work. The block is injected into the system prompt with its
  source paths, states that the harness rules (mode, allowed tools, response
  language) win on any conflict, and reminds the model to `read_file` when a
  file was truncated. The snapshot is captured on the **first turn of a chat and
  reused verbatim afterwards**, so it keeps the prompt prefix cacheable: edits to
  `AGENTS.md` apply to new chats, not to the current one.

In PLAN mode a completed turn ends with a **Plan ready — Switch to BUILD** card
so it is always clear that files cannot change until you switch modes. The plan
itself is a review document, not an implementation: it uses fixed sections
(**Diagnosis & Approach**, **Key Decisions**, **Implementation Plan** grouped by
file path) and never contains raw code blocks, SQL, or verbose inline signatures.
If a genuine ambiguity blocks a correct plan, the agent calls `ask_user` instead
of guessing. The questions are persisted in SQLite and rendered as an inline
card in the chat. Each question offers single- or multi-choice options plus a
custom answer field; sending the answers stores them and automatically starts
the next turn, so the agent continues from what you chose. Answered cards remain
in the transcript as a read-only record, and **Plan ready** is not shown while a
question batch is still pending. If a model returns only reasoning and no final
answer, Forge expands the reasoning card and warns you instead of leaving an
apparently empty turn.

`bash` runs in the project root with a timeout (default 30 s, no upper bound;
set it to 0 for no timeout at all)
and is killed as a process group on timeout or cancellation. Each call is a
fresh shell, so `cd` does not persist; when a command needs another directory,
use one absolute, quoted literal path in the same call (`cd "/abs/path" && …`).
All file tools are confined to the project directory, including symlink-escape
checks for paths that do not exist yet. Writes are atomic.

## UI

- Left rail: projects, add project, theme toggle, settings
- Top: open session tabs
- Center: chat with auto-resizing composer, **Send** and **Stop**, and inline
  clarifying-question cards with selectable options and a custom answer
- Right: collapsible inspector with usage/cost, active files, checklist

Forge ships with dark and light themes. The theme can be switched from the left
rail; the choice is stored in `localStorage` and applied before the first paint
to avoid a flash. The initial theme is dark.

All third-party frontend assets (Bootstrap, Bootstrap Icons, marked and
highlight.js) are vendored under `web/vendor` and embedded in the binary, so the
UI loads with no network access. The files are pinned by version and refreshed
with `make vendor`; the vendored URLs carry the version as a query string and
are served with a long-lived immutable cache, while `index.html`, `app.js` and
`app.css` stay revalidated.

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
`max_tool_rounds` rounds (default **16**) and keeps going automatically until
`max_turn_rounds` (default **64**) when **auto-continue** is enabled. A
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
All limits are configurable in **Settings → BUILD limits** and **Settings → PLAN
research budget**, each knob documented by an inline tooltip.

In PLAN mode, tools that write files or run commands are rejected **before
execution** and rendered as a `🚫 … blocked` card, so you never see a tool start
only to fail because of the mode.

### Slash commands

| Command | Action |
| --- | --- |
| `/compact` | Summarize the older messages into a summary the model receives instead of them. Nothing is deleted: the full history stays in the transcript and the summary is shown as an expandable **Context compacted** card where the new context window starts |

### Keyboard shortcuts

| Shortcut | Action |
| --- | --- |
| `Enter` | Send message |
| `Shift+Enter` | Newline |
| `Ctrl+Shift+,` | Open Settings |
| `Ctrl+Shift+.` | Toggle PLAN / BUILD |
| `Ctrl+Shift+/` | Focus the model picker |
| `Ctrl+Shift+L` | Focus the chat composer |
| `Ctrl+Shift+↑` / `Ctrl+Shift+↓` | Jump to the start of the previous / next answer |
| `Ctrl+Shift+K` / `Ctrl+Shift+J` | Previous / next user message |

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

`log_file` defaults to `<forge home>/forge.log`; set it to an absolute path to
move the log.

Every limit in `defaults` follows one contract: **`0` means no limit** and is
applied exactly as written, while a negative value means "not configured" and
falls back to the shipped default. There is no upper bound, so how far a turn,
a read or a command may go is the developer's decision. The one exception is
`history_prune_chunk_rounds`, which is the granularity of the pruning boundary
rather than a ceiling: `0` selects the default chunk of 4.

The `providers.openrouter` block is kept for backwards compatibility. On first
startup with an empty database, Forge seeds it as the active `openrouter`
provider profile (`OPENROUTER_API_KEY` / `OPENROUTER_BASE_URL` environment
variables win when present). After that, provider profiles live in SQLite and
are managed from **Settings → LLM providers**; the legacy config block is only a
fallback when no profile exists.

## LLM providers

Forge ships one generic driver, `openai_compatible`, so any endpoint that speaks
the OpenAI chat-completions protocol works without a provider-specific
connector. A profile stores:

| Field | Meaning |
| --- | --- |
| `id` / `name` | Stable slug and display name |
| `driver` | `openai_compatible` (the only shipped driver) |
| `base_url` | Endpoint prefix, e.g. `https://openrouter.ai/api/v1` or `http://localhost:11434/v1` |
| `api_key` | Optional; `Authorization` is omitted when empty |
| `default_model` | Model used for new chats on that provider |
| `extra_headers` | Optional per-provider headers, e.g. OpenRouter `HTTP-Referer` / `X-Title` |
| `is_active` | The global default provider |

Base URLs are normalised: trailing slashes and a trailing `/chat/completions` or
`/models` are accepted. Chat requests use SSE with native `tools`, `tool_choice`
and streaming `tool_calls`; token `usage` is parsed when present and treated as
zero when a local endpoint omits it. If an endpoint rejects
`stream_options.include_usage`, Forge retries once without it automatically.

Providers are stored in SQLite (plain text, alongside `forge.db`, which is
created with mode `0600`). API keys are never returned by the API; the UI only
sees a masked hint and `__KEEP__` preserves the stored secret on update.

## Logging

Forge logs to stderr and to the log file at `INFO` and above by default. With
debug enabled it also records every HTTP request, model round, tool execution,
provider latency, and stream summary.

Enable it with any of:

- Settings → **Logging & diagnostics → Debug logging** (applies immediately, saved to config)
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

- Settings → **Logging & diagnostics → LLM payload tracing** (applies immediately, implies debug)
- `./forge --debug-llm`
- `FORGE_LLM_DEBUG=1 ./forge`
- `"debug_llm": true` in `config.json`

Records are labelled `level=LLM` and dumps are written as indented JSON blocks
so they stay readable. This mode **does** write prompt and tool output content to
the log, so treat the log file as sensitive. API keys and the `Authorization`
header are never written at any level, and `--debug-llm` / `FORGE_LLM_DEBUG=1`
win over the config file like `--debug` does.

## Model context windows

`GET /api/models?provider_id=…` proxies the selected provider's model catalog
and caches it per provider for six hours. The UI uses the real `context_length`
for the context-window gauge and the model picker labels, so switching providers
or models updates the denominator immediately. If the catalog is unreachable,
OpenRouter falls back to the built-in table; other providers fall back to their
configured default model. The model field stays free-text so local models that
are not listed can still be used.

## Billing and prompt cache

The final charge is whatever the provider reports in `usage.cost` for each
request (OpenRouter does; most local providers omit it, so the cost stays zero).
Forge accumulates it into **session cost** and **project cost**. The token
counters are informational:

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
| `GET/PUT /api/config` | Read/update config (legacy API key is masked on read) |
| `GET /api/models` | Model catalog for the active or `?provider_id=` provider (`?refresh=1` bypasses the cache) |
| `GET/POST /api/providers` | List (keys masked)/create provider profiles |
| `PUT/DELETE /api/providers/{id}` | Update/delete a profile; `is_active` switches the global default |
| `POST /api/providers/test` | Validate connectivity and list `/models` when supported |
| `GET/POST /api/projects` | List/register projects |
| `PATCH/DELETE /api/projects/{id}` | Rename/recolor/move a project, or delete it with its chats |
| `GET/POST /api/projects/{id}/sessions` | List (`?all=1` includes closed)/create sessions |
| `GET/PATCH/DELETE /api/sessions/{id}` | Session detail/rename/mode/model/provider/close; includes `question_batches` |
| `POST /api/sessions/{id}/compact` | Record a compaction: the summary plus the newest exchange become the context sent to the model, while the stored transcript is left untouched (`compactions` in the payload) |
| `GET /api/sessions/{id}/questions` | List question batches and their answers |
| `POST /api/sessions/{id}/questions/{qid}/answers` | Answer a question batch; returns the batch and the canonical user summary |
| `POST /api/chat/stream` | SSE agent turn (reasoning, content, tool, usage, checklist, questions, done) |
| `GET/POST/PATCH/DELETE` on checklist routes | Manual checklist edits |

## Security notes

Forge has no authentication and can read and write the registered project
directories. It binds to `127.0.0.1` by default and prints a warning if you bind
it to a non-loopback address; only do that on a trusted network. Provider API
keys are stored in plain text in `forge.db` (mode `0600`) and are never included
in logs or API responses.

## Tests

```bash
make test
```

Tests run against `FORGE_HOME` temp directories and mock SSE servers, so they
never touch your real `~/.forge` or call a live provider.