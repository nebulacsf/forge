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

Flags: `--host`, `--port`, `--no-browser`, `--version`. Configured host/port are
used when the flags are omitted.

## Files

| Path | Purpose |
| --- | --- |
| `${FORGE_HOME:-~/.forge}/config.json` | Configuration (created on first run) |
| `${FORGE_HOME:-~/.forge}/forge.db` | SQLite database (projects, sessions, messages, checklists) |

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

## Config

`config.json` is human editable and hot-applied from the Settings modal:

```json
{
  "server": { "host": "127.0.0.1", "port": 7890, "auto_open_browser": true },
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
the test suite and local mocks are wired up.

## HTTP API

| Method & path | Purpose |
| --- | --- |
| `GET /api/health` | Liveness probe |
| `GET/PUT /api/config` | Read/update config (API key is masked on read) |
| `GET/POST /api/projects` | List/register projects |
| `GET/POST /api/projects/{id}/sessions` | List/create sessions |
| `GET/PATCH/DELETE /api/sessions/{id}` | Session detail/rename/mode/model/close |
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