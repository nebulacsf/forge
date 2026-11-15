# Forge Architecture

> **Goal:** High-level documentation for a PM/Architect to quickly understand how the system works.

---

## What is Forge

**Single binary** (Go + embedded assets) serving a **SPA** and exposing REST API + SSE. Uses **OpenRouter** (OpenAI-compatible API) for LLM and **local SQLite** (`~/.forge/forge.db`) for persistence. Config in `~/.forge/config.json`.

**Two modes:**
- **PLAN** — Read-only (explore, search, checklist)
- **BUILD** — Read + write + command execution

---

## Core Components

```
┌────────────────────────────────────────────────────────────┐
│                      FORGE BINARY                          │
├──────────────┬──────────────┬──────────────┬───────────────┤
│   config     │    store     │     llm      │    tools      │
│  (config.go) │ (sqlite.go)  │(openrouter.go)│  (tools.go)   │
└──────┬───────┴──────┬───────┴──────┬───────┴───────┬───────┘
       │              │              │               │
       └──────────────┼──────────────┼───────────────┘
                      ▼              ▼
             ┌─────────────────────────────┐
             │      server (router.go)     │
             │  HTTP + SSE + Web Assets    │
             └──────────────┬──────────────┘
                            ▼
                   ┌────────────────┐
                   │   web/ (SPA)   │
                   │  index.html    │
                   │  app.js/css    │
                   └────────────────┘
                            │
              ┌─────────────┴─────────────┐
              ▼                           ▼
       ┌─────────────┐             ┌─────────────┐
       │  OpenRouter │             │ Project Dir │
       │  (LLM API)  │             │  (file ops) │
       └─────────────┘             └─────────────┘
```

---

## Core Flows

### 1. Startup (`main.go`)
1. Parse flags → 2. Load config → 3. Init logging → 4. Open SQLite (WAL, migrate) → 5. Embed assets → 6. Mount HTTP handler → 7. Listen + open browser (if loopback) → 8. Graceful shutdown (SIGINT/TERM, 3s)

### 2. Chat Turn (`stream.go:handleChatStream`) — **Core loop**
```
POST /api/chat/stream (session_id, content)
         │
         ▼
┌────────────────────────────────────────────────────────┐
│ 1. Validate session/project/API key                    │
│ 2. Save user message                                   │
│ 3. Create tools.Manager (project root, mode)           │
│ 4. Ensure local repo outline (AST/regex scan, cached)  │
│ 5. Build system prompt (outline + checklist + rules)   │
│ 6. Load history from DB                                │
└────────────────────────────────────────────────────────┘
         │
         ▼
┌────────────────────────────────────────────────────────┐
│ SEGMENTED ROUND LOOP (default: 24 / segment, 96 / turn)│
│                                                         │
│  FOR EACH SEGMENT (max_tool_rounds):                   │
│   FOR EACH ROUND (until max_turn_rounds):              │
│    A. SSE: round_start {round, limit, segment}         │
│    B. llm.StreamChat() → SSE: reasoning/content        │
│    C. Save usage → SSE: usage                          │
│    D. IF no tool_calls:                                │
│       - Save assistant → SSE: done → END               │
│    E. FOR EACH tool_call:                              │
│       1. Allowed(mode, tool)? → tool_blocked if not    │
│       2. SSE: tool_start                               │
│       3. Manager.Execute()                             │
│       4. guard.observe(call, result)                   │
│       5. SSE: tool_end (result/error/duration)         │
│       6. If checklist changed → SSE: checklist         │
│    F. Save assistant + tool calls + results            │
│    G. Stop on loop_detected / tool_error_limit / cost  │
│   END FOR                                               │
│   IF turn budget spent → SSE: limit → END              │
│   IF auto-continue → SSE: checkpoint → next segment    │
│   ELSE → SSE: checkpoint (paused) → END                │
│  END FOR                                                │
└────────────────────────────────────────────────────────┘
```

A **round** is one model call; it may contain several tool calls. The 24-round
limit is therefore the **checkpoint/segment size**, not the total work a turn can
do. `max_turn_rounds` (default 96) bounds the whole turn, and `auto_continue`
crosses segment boundaries automatically while the model is still working.

### 3.1 Loop guard (`server/guard.go`)
Every executed (or blocked) tool call is fingerprinted as
`tool + canonical JSON args + result hash + error flag`. The guard keeps a
six-call window and:

- nudges the model when the same call+result appears twice (ephemeral user
  message, never persisted);
- stops the turn with `limit {reason: loop_detected}` on the third repetition;
- nudges after 4 consecutive tool errors and stops at 6
  (`reason: tool_error_limit`);
- leaves legitimate cycles alone: `test → patch → test` naturally produces
  different fingerprints, and any different call moves the window forward.

### 3. Tool Execution (`tools.go`)
**Security:** `resolve()` converts relative paths → absolute and **validates they don't escape project root** (resolves symlinks).config.go

**Permissions by mode:**

| Tool | PLAN | BUILD |
|------|:----:|:-----:|
| read_file, grep_search, list_directory, git_diff, update_checklist | ✅ | ✅ |
| patch_file, write_file, bash | ❌ | ✅ |

**Bounded reads:** `read_file` requires `start_line` and `end_line`; at most 200
lines are returned, each prefixed with its real line number (`142 | code`).
Requests above the cap are truncated and report the requested vs. returned
range. The agent is instructed to use the repo outline or `grep_search` before
reading.

### Repository Map (`tools/scanner.go`)
Forge builds a zero-AI outline of the project and stores one row per file in
`repo_outline`:

- `.go` files: `go/parser` + `go/ast` (`SkipObjectResolution`) for package,
  structs, interfaces, types, functions, and methods with declaration lines.
- Other languages: fast regex/line scanners for JS/TS, Python, Rust, Java,
  Ruby, PHP, and HTML.
- The walk skips vendor/build/hidden directories, binary files, and files
  larger than 512 KB; only files whose mtime/size changed are reparsed.
- The rendered outline is capped at ~2,000 runes (~450–500 tokens). It is
  refreshed on project creation and on each chat turn; `write_file`/`patch_file`
  refresh just the affected file.
- The outline and checklist are **not** part of the system prompt. They are
  injected in a trailing ephemeral user message (see 3.2) so the system prompt
  stays byte-stable and cacheable.

### 3.2 History Pruning (`server/history.go`)
Every provider round rebuilds the request from persisted rows. Before sending,
Forge prunes tool traffic so a long turn does not re-inject every prior result:

- The last `history_keep_recent_rounds` tool rounds (default 4) are sent
  verbatim, except that each result is clipped head+tail to
  `history_max_result_chars` (default 4,000) and oversized arguments are
  replaced with a compact, valid JSON summary (`history_max_arg_chars`, default
  4,000). A large `write_file` payload is therefore never re-sent.
- Older rounds are **stubbed in place**: the assistant tool call remains at its
  original position with compacted arguments, and its tool result becomes a
  one-line `[pruned] tool target → outcome` digest. Nothing is removed,
  reordered, or merged, so the message count and `tool_call_id` pairing stay
  valid. Old errors keep a short `ERROR:` digest.
- Pruning advances in chunks of `history_prune_chunk_rounds` (default 4). Within
  a chunk the request is strictly append-only; only when a whole chunk becomes
  old do those rounds switch to stubs. This keeps most consecutive requests as
  an exact prefix of the next one, which is what provider prompt caching needs.
- The system prompt contains only stable content (project, mode, rules). The
  volatile checklist, repository outline, continue note, and guard note are
  appended as the final ephemeral `user` message of each request and are never
  persisted. Changing them cannot invalidate the cached prefix.
- Setting a max to `0` disables that cap; negative values fall back to defaults.
- Full results remain in SQLite and in the UI (`/api/sessions/{id}/messages`);
  only the provider request is trimmed. Each round logs `messages`, `chars`,
  `est_tokens`, `pruned_rounds`, `pruned_chars`, `cached_tokens`, and
  `cache_ratio`.

### 4. Compaction (`compact.go`)
`POST /api/sessions/{id}/compact` → Summarize history with LLM → Replace in DB: summary + last 1-2 exchanges → Return new list.

### 5. Config (`config.go`)
- `Manager` with `RWMutex`, atomic writes (temp → chmod 600 → rename)
- Validation + normalization on every `Update()`
- `defaults.history_*` control history pruning (see 3.2)
- Hot-reload: invalidates model cache if API key/base URL changes

### 6. Model Catalog (`router.go`)
- Cache TTL: 6h (OpenRouter) / 2m (builtin fallback)
- `GET /api/models?refresh=1` forces refresh
- Falls back to `BuiltinModels()` on failure

---

## Data Model (SQLite)

```
projects: id, name, path(UNIQUE), icon_color, last_opened_at, total_cost_usd, session_count
sessions: id, project_id(FK), title, model, mode(PLAN|BUILD), tokens_in/out, cached_tokens, cost_usd, duration_ms, is_open, updated_at
messages: id, session_id(FK), role(user|assistant|tool), reasoning, content, tool_calls(JSON), duration_ms, created_at
checklists: id, session_id(FK), task_text, is_done, position
repo_outline: project_path, file_path, language, mtime_ns, size_bytes, symbols, updated_at
              PRIMARY KEY(project_path, file_path)
```

Indexes: sessions(project_id, is_open, updated_at), messages(session_id, id),
checklists(session_id, position, id). `repo_outline` is covered by its composite
primary key `(project_path, file_path)`.

`repo_outline` is keyed by the project's absolute CWD. Rows are upserted only
when a file's mtime or size changes; removed files are pruned in the same
transaction.

---

## API (REST + SSE)

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/health` | Health check |
| GET/PUT | `/api/config` | Config |
| GET | `/api/models` | Model catalog |
| GET/POST | `/api/projects` | Projects |
| GET/POST | `/api/projects/{id}/sessions` | Sessions |
| GET/PATCH/DELETE | `/api/sessions/{id}` | Session CRUD |
| POST | `/api/sessions/{id}/compact` | Compact |
| GET | `/api/sessions/{id}/messages` | History |
| GET/POST/PATCH/DELETE | `/api/checklist` | Checklist items |
| **POST** | **`/api/chat/stream`** | **Agent turn (SSE)** |
| GET | `/` | Embedded SPA |

**SSE Events** (from `/api/chat/stream`):
`round_start`, `reasoning`, `content`, `tool_start`, `tool_end`, `tool_blocked`, `usage`, `checklist`, `session`, `checkpoint`, `limit`, `done`, `cancelled`, `error`

---

## Frontend (`web/`)
**Vanilla ES6 SPA** (Bootstrap 5, marked.js, highlight.js):
- **Left:** Project rail + add/theme/settings buttons
- **Center:** Session tabs + chat (messages + composer)
- **Right (XL+):** Collapsible inspector with usage/cost, active files, checklist
- **Themes:** `data-theme` / `data-bs-theme` drive custom tokens, Bootstrap, and highlight.js; preference stored in `localStorage`
- **Timers:** Live session and per-response timers; persisted as `sessions.duration_ms` and `messages.duration_ms`
- **Modals:** Add Project, Settings
- **SSE:** `EventSource` parses typed events → incremental DOM updates

---

## Security & Isolation

| Boundary | Mechanism |
|----------|-----------|
| Project root | `tools.resolve()` — `filepath.Rel` + symlink resolution; rejects escapes |
| Mode | `tools.Allowed(mode, name)` checked **before** execution AND in specs sent to LLM |
| Sizes | Read window 200 lines (10MB file max), grep skips >2MB, scanner skips >512KB, outputs truncated |
| Bash | `NO_COLOR=1`, `TERM=dumb`, timeout (default 30s, max 600s) |
| Runaway turns | Segment/turn round budgets, optional per-turn USD cap, loop guard (`server/guard.go`) |
| Config | 0600; API key masked in responses |
| DB | WAL, FK cascade |
| Network | Default bind 127.0.0.1; warning if non-loopback |

---

## Concurrency

| Component | Strategy |
|-----------|----------|
| HTTP server | Goroutine per connection (stdlib) |
| Config | `sync.RWMutex` (read-heavy) |
| Models | `sync.Mutex` (cache) |
| SQLite | `database/sql` pool (max 4 conns) |
| SSE streams | 1 goroutine per `handleChatStream`; cancel via request context |
| Tools | Sequential per round |

---

## Observability
- Structured logging (`logx`): DEBUG/INFO/WARN/ERROR with key-value
- File: `~/.forge/forge.log`
- HTTP middleware: method, path, status, duration, bytes
- Provider logs: model, tokens, cost, latency per round
- Debug: `--debug`, `FORGE_DEBUG=1`, `config.server.debug` (hot-reload)

---

## Future Extension Points

| Area | Status | Idea |
|------|--------|------|
| Providers | OpenRouter only | Plugin interface for other compatible APIs |
| Tools | 8 built-in | Dynamic registration (MCP, custom scripts) |
| Auth | None (local) | Optional token for remote access |
| Multi-user | Single-user | User isolation, own projects |
| Telemetry | Local logs | Opt-in metrics, trace export |

---

## Quick File Map

```
main.go                      # Entry point, wiring, lifecycle
internal/
  config/config.go           # Config load/save/validate, paths
  store/sqlite.go            # Schema, CRUD projects/sessions/messages/checklists
  llm/openrouter.go          # Streaming chat client, SSE parsing
  llm/models.go              # Model catalog fetch + builtin fallback
  server/router.go           # HTTP routes, middleware, APIs config/models/projects/sessions/checklist
  server/stream.go           # handleChatStream — core agent loop
  server/history.go          # History pruning: recent rounds verbatim, older rounds stubbed in place
  server/guard.go            # Per-turn loop / error-streak detector
  server/compact.go          # Conversation summarization
  tools/tools.go             # Tool defs + execution (read/grep/list/git/patch/write/bash/checklist)
  tools/scanner.go           # Zero-AI repo map (Go AST + regex), SQLite cache, compact renderer
  logx/logx.go               # Leveled logging with file output
web/
  index.html                 # SPA structure, modals, CDN deps
  js/app.js                  # Frontend logic (state, SSE, rendering)
  css/app.css                # Custom styles (dark and light themes)
```

---

*Document generated from code analysis — keep synchronized with significant architectural changes.*