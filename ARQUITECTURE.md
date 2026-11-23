# Forge Architecture

> **Goal:** High-level documentation for a PM/Architect to quickly understand how the system works.

---

## What is Forge

**Single binary** (Go + embedded assets) serving a **SPA** and exposing REST API + SSE. Uses a **generic OpenAI-compatible LLM layer** (OpenRouter, Ollama, LM Studio, vLLM, Groq, DeepSeek, OpenCode Go, custom gateways) and **local SQLite** (`~/.forge/forge.db`) for persistence. Config in `~/.forge/config.json`; provider profiles live in SQLite.

**Two modes:**
- **PLAN** — Read-only (explore, search, checklist, clarify with `ask_user`)
- **BUILD** — Read + write + command execution

---

## Core Components

```
┌────────────────────────────────────────────────────────────┐
│                      FORGE BINARY                          │
├──────────────┬──────────────┬──────────────┬───────────────┤
│   config     │    store     │     llm      │    tools      │
│  (config.go) │ (sqlite.go)  │(provider.go) │  (tools.go)   │
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
       ┌─────────────────────┐     ┌─────────────┐
       │ OpenAI-compatible   │     │ Project Dir │
       │ provider (selected) │     │  (file ops) │
       └─────────────────────┘     └─────────────┘
```

---

## Core Flows

### 1. Startup (`main.go`)
1. Parse flags → 2. Load config → 3. Init logging → 4. Open SQLite (WAL, migrate) → 5. Seed the `openrouter` provider from legacy config/env when the provider table is empty → 6. Embed assets → 7. Mount HTTP handler → 8. Listen + open browser (if loopback) → 9. Graceful shutdown (SIGINT/TERM, 3s)

### 2. Chat Turn (`stream.go:handleChatStream`) — **Core loop**
```
POST /api/chat/stream (session_id, content)
         │
         ▼
┌────────────────────────────────────────────────────────┐
│ 1. Validate session/project; resolve session provider   │
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
│    G. IF ask_user ran:                                 │
│       - SSE: questions (persisted batch) → done → END  │
│       (the user answers in the chat; no extra round)   │
│    H. Stop on loop_detected / tool_error_limit / cost  │
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

PLAN is special-cased: it is one bounded research turn with its own
`plan_rounds` (default 16, max 24) and `plan_tool_calls` (default 64, max 200)
budget. When either limit is reached, the next request keeps the tool
definitions for cache stability but sets `tool_choice=none` and appends a
final-answer instruction. Tool calls emitted in that round are ignored. The
finalization phase allows three attempts; empty answers, reasoning-only replies,
and tool calls written as text (`<tool_call>`, XML or JSON) are rejected and
retried. The last attempt removes the tool definitions entirely for providers
that ignore `tool_choice`. A guard-triggered stop in PLAN also transitions to
this forced final answer instead of ending with no plan. PLAN never
auto-continues.

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
| read_file, grep_search, symbol_lookup, list_directory, git_diff, update_checklist, ask_user | ✅ | ✅ |
| patch_file, write_file, bash | ❌ | ✅ |

`ask_user` is a terminal tool: it persists one batch of 1–4 questions with up to
6 options each, emits a `questions` SSE event, and ends the turn. The answer
endpoint stores the selected option IDs plus free text, returns a canonical
"Answers to your questions:" summary, and the frontend sends that summary as the
next user message to resume the agent.

**Bounded reads:** `read_file` requires `start_line` and `end_line`; at most 200
lines are returned, each prefixed with its real line number (`142 | code`).
Requests above the cap are truncated and report the requested vs. returned
range. The agent is instructed to use `symbol_lookup` for known declarations,
the repo outline, or `grep_search` before reading.

### Repository Map (`tools/scanner.go`)
Forge builds a zero-AI outline of the project and stores one row per file in
`repo_outline`. The same scan also feeds `project_symbols`, a queryable table
with one row per declaration and exact start/end lines for `symbol_lookup`:

- `.go` files: `go/parser` + `go/ast` (`SkipObjectResolution`) for package,
  structs, interfaces, types, functions, and methods with declaration lines.
- Other languages: fast regex/line scanners for JS/TS, Python, Rust, Java,
  Ruby, PHP, and HTML.
- The walk skips vendor/build/hidden directories, binary files, and files
  larger than 512 KB; only files whose mtime/size changed are reparsed.
- The rendered outline is capped at ~2,000 runes (~450–500 tokens). It is
  refreshed on project creation and on each chat turn; `write_file`/`patch_file`
  refresh just the affected file.
- `project_symbols` stores the complete symbol set (not capped at 32) with
  `start_line`–`end_line`, kind, and parent. `symbol_lookup` checks mtime/size
  before each query and reparses only changed files, so line ranges never drift.
- The outline and checklist are **not** part of the system prompt. They are
  injected in a trailing ephemeral user message (see 3.2) so the system prompt
  stays byte-stable and cacheable.

### 3.2 History Pruning (`server/history.go`)
Every provider round rebuilds the request from persisted rows. Before sending,
Forge prunes tool traffic so a long turn does not re-inject every prior result:

- Pruning is **budget driven**. The request must fit in
  `min(model context length × history_prune_percent, history_prune_max_tokens)`
  tokens, i.e. 70% of the window capped at 120,000 by default. The context
  length comes from the selected provider's cached catalog, then the built-in
  OpenRouter-style table (`llm.BuiltinContextLength`); an unknown model falls
  back to the token cap alone. The ceiling covers the whole
  request: system prompt, tool definitions, and conversation. Tool rounds are
  kept verbatim from the newest backwards until the ceiling is reached, so a
  large-context model actually gets to use its window.
- Kept rounds have each result clipped head+tail to `history_max_result_chars`
  (default 16,000) and oversized arguments replaced with a compact, valid JSON
  summary (`history_max_arg_chars`, default 4,000). A large `write_file` payload
  is therefore never re-sent.
- Setting either budget knob to `0` disables the budget and re-enables the
  legacy round-based window `history_keep_recent_rounds` (default 0, max 100).
  Both legacy knobs ship at `0` (disabled) because the budget replaces them.
- Older rounds are **stubbed in place**: the assistant tool call remains at its
  original position with compacted arguments, and its tool result becomes a
  one-line `[pruned] tool target → outcome` digest. Nothing is removed,
  reordered, or merged, so the message count and `tool_call_id` pairing stay
  valid. Old errors keep a short `ERROR:` digest.
- Pruning advances in chunks of `history_prune_chunk_rounds`. `0` (the shipped
  value) means "use the built-in chunk of 4", and the same chunk applies to both
  budget-driven and round-based pruning. Within a chunk the request is strictly
  append-only; only when a whole chunk becomes oversized do those rounds switch
  to stubs. This keeps most consecutive requests as an exact prefix of the next
  one, which is what provider prompt caching needs.
- The system prompt contains only stable content (project, mode, rules). The
  volatile checklist, repository outline, research ledger, continue note, guard
  note, and forced-final instruction are appended as the final ephemeral `user`
  message of each request and are never persisted. Changing them cannot
  invalidate the cached prefix.
- In PLAN, the budget gets a `MinRecent` floor of `plan_rounds + 2`, so every
  tool round of the current bounded research turn is sent unpruned even if one
  oversized read blows past the ceiling by itself. The per-result cap is also floored at 8,000 characters (unless
  the user disabled it with 0), which the shipped 16,000 default already
  exceeds, so a normal 100-line read is not clipped in half. Older turns are
  pruned normally. This prevents pruning and clipping from erasing the findings
  the model needs for the final plan.
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
- `defaults.plan_rounds` / `defaults.plan_tool_calls` bound PLAN research
  (defaults 16 / 64; clamped to 24 / 200)
- Hot-reload: invalidates the model cache when legacy config changes; provider
  profiles are read from SQLite on every turn, so provider changes need no restart

### 6. Model Catalog & Providers (`router.go`, `providers.go`)
- Profiles live in `llm_providers`; one row is `is_active` (partial unique index).
- Sessions may pin `provider_id`; empty means "follow the active provider".
- Cache is per provider: TTL 6h, or 2m after a fallback.
- `GET /api/models?provider_id=…&refresh=1` forces a refresh
- OpenRouter falls back to `BuiltinModels()`; other providers fall back to their
  default model.
- CRUD + `POST /api/providers/test` validate connectivity and `/models` support.

---

## Data Model (SQLite)

```
projects: id, name, path(UNIQUE), icon_color, last_opened_at, total_cost_usd, session_count
sessions: id, project_id(FK), title, model, mode(PLAN|BUILD), provider_id, tokens_in/out, cached_tokens, cost_usd, duration_ms, is_open, updated_at
messages: id, session_id(FK), role(user|assistant|tool), reasoning, content, tool_calls(JSON), duration_ms, created_at
checklists: id, session_id(FK), task_text, is_done, position
question_batches: id, session_id(FK), tool_call_id, created_at, answered_at
questions: id, batch_id(FK), session_id(FK), text, context, kind(single|multi), position, answer_option_ids(JSON), answer_text, answered_at
question_options: id, question_id(FK), label, description, position
repo_outline: project_path, file_path, language, mtime_ns, size_bytes, symbols, updated_at
              PRIMARY KEY(project_path, file_path)
llm_providers: id(TEXT PK), name, driver('openai_compatible'), base_url, api_key,
               default_model, extra_headers_json, is_active, created_at
```

Indexes: sessions(project_id, is_open, updated_at), messages(session_id, id),
checklists(session_id, position, id), question_batches(session_id, id),
questions(batch_id, position, id), question_options(question_id, position, id),
and a partial unique index on `llm_providers(is_active) WHERE is_active = 1`.
`repo_outline` is covered by its composite primary key
`(project_path, file_path)`. Deleting a session cascades to its question batches,
questions, and options.

`repo_outline` is keyed by the project's absolute CWD. Rows are upserted only
when a file's mtime or size changes; removed files are pruned in the same
transaction.

---

## API (REST + SSE)

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/health` | Health check |
| GET/PUT | `/api/config` | Config |
| GET | `/api/models` | Model catalog for the selected/active provider |
| GET/POST | `/api/providers` | List (masked keys)/create provider profiles |
| PUT/DELETE | `/api/providers/{id}` | Update/delete; `is_active` switches the global default |
| POST | `/api/providers/test` | Connectivity + `/models` probe |
| GET/POST | `/api/projects` | Projects |
| PATCH/DELETE | `/api/projects/{id}` | Project rename/recolor/move, delete (cascades chats) |
| GET/POST | `/api/projects/{id}/sessions` | Sessions (`?all=1` includes closed tabs) |
| GET/PATCH/DELETE | `/api/sessions/{id}` | Session CRUD |
| POST | `/api/sessions/{id}/compact` | Compact |
| GET | `/api/sessions/{id}/messages` | History |
| GET | `/api/sessions/{id}/questions` | Question batches and answers |
| POST | `/api/sessions/{id}/questions/{qid}/answers` | Answer a batch; returns the batch and canonical summary |
| GET/POST/PATCH/DELETE | `/api/checklist` | Checklist items |
| **POST** | **`/api/chat/stream`** | **Agent turn (SSE)** |
| GET | `/` | Embedded SPA |

**SSE Events** (from `/api/chat/stream`):
`round_start`, `reasoning`, `content`, `tool_start`, `tool_end`, `tool_blocked`, `usage`, `checklist`, `questions`, `session`, `checkpoint`, `limit`, `done`, `cancelled`, `error`

---

## Frontend (`web/`)
**Vanilla ES6 SPA** (Bootstrap 5, marked.js, highlight.js):
- **Left:** Project rail + add/theme/settings buttons
- **Center:** Session tabs + chat (messages, inline clarifying-question cards, composer)
- **Right (XL+):** Collapsible inspector with usage/cost, active files, checklist
- **Themes:** `data-theme` / `data-bs-theme` drive custom tokens, Bootstrap, and highlight.js; preference stored in `localStorage`
- **Timers:** Live session and per-response timers; persisted as `sessions.duration_ms` and `messages.duration_ms`
- **Chat controls:** provider selector (per-chat override + global default) and model input
- **Modals:** Add Project, Settings — the Settings form is grouped into ordered sections (LLM providers with presets/test/edit, model, BUILD limits, PLAN budget, tool limits, context/history, server, logging) and every field carries an inline tooltip explaining its unit, default and effect
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
| Config | 0600; legacy API key masked in responses |
| DB | WAL, FK cascade; provider keys stored in `llm_providers` and never returned raw; DB mode 0600 |
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
- LLM trace: `--debug-llm`, `FORGE_LLM_DEBUG=1`, `config.server.debug_llm`
  (hot-reload). An extra level below DEBUG that dumps the full provider
  request body, every raw SSE chunk, the assembled response and provider error
  bodies. Implies DEBUG; API keys and the `Authorization` header are never
  logged.

---

## Future Extension Points

| Area | Status | Idea |
|------|--------|------|
| Providers | `openai_compatible` driver + profiles | Additional drivers (native APIs, OAuth) behind `llm.Provider` |
| Tools | 10 built-in | Dynamic registration (MCP, custom scripts) |
| Auth | None (local) | Optional token for remote access |
| Multi-user | Single-user | User isolation, own projects |
| Telemetry | Local logs | Opt-in metrics, trace export |

---

## Quick File Map

```
main.go                      # Entry point, wiring, lifecycle
internal/
  config/config.go           # Config load/save/validate, paths
  store/sqlite.go            # Schema, CRUD projects/sessions/messages/checklists/questions
  store/providers.go         # Provider profile CRUD, active invariant, session binding
  llm/provider.go            # Profile + Provider/ChatClient interface + driver factory
  llm/openai_compatible.go   # Generic streaming chat client, SSE + tool calls + usage
  llm/models.go              # Model catalog fetch/probe + builtin fallback
  server/providers.go        # Provider resolver, CRUD/test handlers, legacy seeding
  server/router.go           # HTTP routes, middleware, APIs config/models/providers/projects/sessions/checklist
  server/questions.go        # Question batch API + canonical answer summary
  server/stream.go           # handleChatStream — core agent loop
  server/history.go          # History pruning: recent rounds verbatim, older rounds stubbed in place
  server/guard.go            # Per-turn loop / error-streak detector
  server/compact.go          # Conversation summarization
  tools/tools.go             # Tool defs + execution (read/grep/list/git/patch/write/bash/checklist/ask_user)
  tools/scanner.go           # Zero-AI repo map (Go AST + regex), SQLite cache, compact renderer
  logx/logx.go               # Leveled logging with file output
web/
  index.html                 # SPA structure, modals, CDN deps
  js/app.js                  # Frontend logic (state, SSE, rendering)
  css/app.css                # Custom styles (dark and light themes)
```

---

*Document generated from code analysis — keep synchronized with significant architectural changes.*