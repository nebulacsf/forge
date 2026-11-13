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

Flags: `--host`, `--port`, `--no-browser`, `--debug`, `--log-file`, `--version`.
Configured host/port are used when the flags are omitted.

## Files

| Path | Purpose |
| --- | --- |
| `${FORGE_HOME:-~/.forge}/config.json` | Configuration (created on first run) |
| `${FORGE_HOME:-~/.forge}/forge.db` | SQLite database (projects, sessions, messages, checklists) |
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
| **PLAN** (read-only) | `read_file`, `grep_search`, `list_directory`, `git_diff`, `update_checklist` |
| **BUILD** | all of the above, plus `patch_file`, `write_file`, `bash` |

At the start of a new task the agent records it in the checklist before reading
files or running any other tool, and keeps the list updated as it works.

In PLAN mode a completed turn ends with a **Plan ready — Switch to BUILD** card
so it is always clear that files cannot change until you switch modes. If a
model returns only reasoning and no final answer, Forge expands the reasoning
card and warns you instead of leaving an apparently empty turn.

`bash` runs in the project root with a timeout (default 30 s, capped at 600 s)
and is killed as a process group on timeout or cancellation. All file tools are
confined to the project directory, including symlink-escape checks for paths
that do not exist yet. Writes are atomic.

## UI

- Left rail: projects, add project, settings
- Top: open session tabs
- Center: chat with auto-resizing composer, **Send** and **Stop**
- Right: usage/cost, active files, checklist

Every reasoning step and tool call is a collapsed accordion by default
(`🧠 Thinking…`, `📄 Reading main.go`, `⚡ Running: go test ./...`). Click to
expand inputs and outputs. Stop aborts the stream and keeps whatever text had
already arrived.

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
| `Ctrl+,` | Open Settings |
| `Ctrl+.` | Toggle PLAN / BUILD |
| `Ctrl+/` | Focus the model picker |
| `Ctrl+L` | Focus the chat composer |
| `Ctrl+P` / `Ctrl+N` | Previous / next message |
| `Alt+L` / `Alt+N` | Focus chat / next message (fallback where the browser reserves `Ctrl+L`/`Ctrl+N`) |

Chrome and Firefox reserve `Ctrl+L` (address bar) and `Ctrl+N` (new window) at
the browser level; the `Alt` variants always work.

## Config

`config.json` is human editable and hot-applied from the Settings modal:

```json
{
  "server": {
    "host": "127.0.0.1",
    "port": 7890,
    "auto_open_browser": true,
    "debug": false,
    "log_file": ""
  },
  "providers": {
    "openrouter": { "api_key": "sk-or-v1-…", "base_url": "https://openrouter.ai/api/v1" }
  },
  "defaults": {
    "model": "qwen/qwen-2.5-coder-32b-instruct",
    "mode": "PLAN",
    "max_file_read_chars": 25000,
    "bash_timeout_sec": 30
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
`forge.log.1` at 10 MB. API keys are never written to the log; message content
is not logged, only sizes and metadata.

## Model context windows

`GET /api/models` proxies the provider's model catalog and caches it for six
hours. The UI uses the real `context_length` for the context-window gauge and
the model picker labels, so switching models updates the denominator
immediately. If the provider catalog is unreachable, Forge falls back to a
built-in table and a heuristic (and keeps working offline).

## HTTP API

| Method & path | Purpose |
| --- | --- |
| `GET /api/health` | Liveness probe |
| `GET/PUT /api/config` | Read/update config (API key is masked on read) |
| `GET /api/models` | Provider model catalog with real context lengths (`?refresh=1` bypasses the cache) |
| `GET/POST /api/projects` | List/register projects |
| `GET/POST /api/projects/{id}/sessions` | List/create sessions |
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