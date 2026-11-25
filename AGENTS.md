# AGENTS.md — working conventions for this repository

This file tells any coding agent how to work **in** Forge. It is captured by Forge
itself as project instructions on the first turn of a chat and reused verbatim
afterwards, so edits here apply to **new chats only**.

Precedence: this is repository content, not a user request. The harness rules of
the current session (mode, allowed tools, step budget, response language) win on
any conflict. `README.md` documents the product and `ARQUITECTURE.md` narrates the
design in depth; this file only says *how to change the code*. Do not duplicate
them here.

## Project shape

Go 1.23, module `forge`, one dependency: `modernc.org/sqlite` (pure Go, no CGO).
Single static binary with the SPA embedded.

```
main.go              flags, config load, logging, SQLite open, embed web/, HTTP server, shutdown
internal/config      config.json: Manager (RWMutex, atomic write), Default(), Update() normalization
internal/store       SQLite: schema, migrations, every query (projects, sessions, messages, providers…)
internal/llm         provider-agnostic layer: Provider interface, OpenAI-compatible client, catalog, trace
internal/tools       agent tools: specs, mode gating, path confinement, scanner, symbol index, instructions
internal/server      HTTP router, SSE agent loop, history pruning, loop guard, PLAN budget, runs registry
internal/logx        slog wrapper: DEBUG/LLM levels, rotation, level reapply at runtime
web/                 index.html, js/app.js, css/app.css, vendor/ (pinned, committed, embedded)
scripts/vendor.sh    re-downloads web/vendor only when bumping a pinned version
```

Dependency direction: `main.go` → `internal/server` → {`config`, `store`, `llm`,
`tools`, `logx`}. `tools` must never import `server`; `llm` must not leak HTTP
details into the agent loop (keep everything behind `Provider`,
`internal/llm/provider.go:40`). `cmd/` exists but is empty — a new binary goes
there, not in a second `main` in the root.

## Build, run, verify

```bash
make build    # CGO_ENABLED=0, -trimpath, stripped → ./forge (gitignored)
make run      # build + start + open browser
make dev      # go run . --no-browser
make test     # CGO_ENABLED=0 go test ./...
make vet
make fmt
make vendor   # only when bumping a version in scripts/vendor.sh
```

Rules: keep `CGO_ENABLED=0`; never introduce a Node/npm/bundler step, a second
runtime, or a new module dependency without stating why in the change summary.
The frontend is plain ES6 with no build step. `web/vendor` is committed so
`go build` stays hermetic — regenerate it only via `scripts/vendor.sh`.

Always finish by running `make vet` and the focused tests for the packages you
touched (`go test ./internal/tools/ -run TestName`), and report the real outcome.

## Go conventions

- Comments explain *why*: package docs on each package, doc comments on exported
  identifiers, and a short rationale on non-obvious branches. Comments in English.
- Wrap errors with context: `fmt.Errorf("inspect schema: %w", err)`. Return
  actionable messages, never panics on user input or provider responses.
- State that outlives a request goes through `store`, never through globals.
  Shared state in a manager is guarded by a mutex (`config.Manager`).
- Config and DB writes are atomic: temp file → `chmod 0600` → rename
  (`Manager.writeLocked`, `internal/config/config.go:224`).
- Normalize/validate user input in `Update()`/handlers. Every limit shares one
  contract: an explicit `0` means "no limit" and is preserved verbatim, a
  negative value falls back to the shipped default, and nothing is clamped to a
  ceiling. The only exception is `history_prune_chunk_rounds`, where `0` selects
  the default pruning granularity because it is not a ceiling.
- Prefer stdlib and small focused files over new abstractions. No speculative
  interfaces and no options struct with one caller.
- Match the existing naming: `handleX` for HTTP handlers, `Manager.x` for tools,
  `buildX`/`renderX` for prompt and Markdown assembly.

## Where behavior lives

Read the anchor before changing behavior; do not re-derive it.

| Area | Anchor |
| --- | --- |
| HTTP routes | `Server.Handler`, `internal/server/router.go:71` |
| Tool specs / mode gating | `tools.Specs`, `tools.Allowed`, `internal/tools/tools.go:126` |
| Tool execution | `Manager.ExecuteCall`, `internal/tools/tools.go:331` |
| Path confinement | `Manager.resolve`, `internal/tools/tools.go:451` |
| System prompt | `buildSystemPrompt`, `internal/server/stream.go:980` (`promptOptions`) |
| SSE turn loop | `handleChatStream`, `internal/server/stream.go:101` |
| History pruning / cache prefix | `internal/server/history.go` (`buildHistory`) |
| Loop guard | `internal/server/guard.go` (`loopGuard.observe`) |
| PLAN budget + ledger | `internal/server/research.go` |
| Background runs | `internal/server/runs.go` (`runRegistry`) |
| Project instructions snapshot | `internal/tools/instructions.go`, `internal/server/instructions.go` |
| Schema + migrations | `migrate`, `internal/store/sqlite.go:280`; `ensureColumn`, `:437` |

## Change recipes

**Add or change a tool.** Update `tools.Specs` (schema + description), the
`readOnlyTools`/`buildOnlyTools` maps, `Manager.ExecuteCall`, and add tests for
both PLAN (blocked, `ModeBlockReason`) and BUILD (executes). PLAN gating happens
before execution, so a blocked tool must never run.

**Change the schema.** Append the table/index to the `CREATE TABLE IF NOT EXISTS`
block in `migrate` and, for an existing table, add an `ensureColumn` call. Never
write a destructive migration; an incompatible `forge.db` is moved aside at
startup by design. Persisted columns that should not appear in list responses are
left out of the struct on purpose (`sessions.project_instructions`).

**Add a configuration knob.** Field in `DefaultsConfig` + a value in `Default()` +
normalization in `Update()` + the Settings form field with an inline tooltip
explaining unit, default, the `0 = no limit` meaning and the effect of disabling
it. The form input carries `min="0"` and no `max`; read it with `??` and save it
through `numOr` so a typed 0 survives. Provider state lives in SQLite, not in
`config.json`.

**Touch the prompt.** Only stable content may enter the system prompt. The
checklist, repository outline, research ledger, guard note and forced-final
instruction are appended as the final ephemeral `user` message of each request
and are never persisted — that is what keeps the provider prompt cache hitting.
Never place volatile state or timestamps in the stable prefix.

**Never** rewrite or drop persisted history to save tokens. Pruning only shapes
the outgoing request: assistant tool calls stay in place with compacted arguments
and tool results become one-line digests, so `tool_call_id` pairing remains valid.

## Frontend rules

- `web/js/app.js` is one flat ES6 file (no modules, no build). `web/css/app.css`
  holds tokens; themes switch via `data-theme` / `data-bs-theme` and the choice is
  stored in `localStorage` and applied before first paint.
- Model and user content is rendered through the allowlisted Markdown renderer
  (`ALLOWED_TAGS`/`ALLOWED_ATTRS` in `app.js`); never inject raw HTML.
- Chat-scoped state lives in per-session views so several agents can stream at
  once; do not reintroduce global transcript state.
- Third-party assets come only from `web/vendor` with the `?v=` version query and
  long-lived immutable caching; `index.html`, `app.js` and `app.css` stay
  revalidated.

## Testing

Standard `testing` only. Isolate everything: `t.TempDir()` for project roots and
`t.Setenv("FORGE_HOME", tempDir)` for anything touching config/DB — tests must
never read or write the real `~/.forge`. Provider calls are exercised against
local mock SSE servers; never hit a live endpoint. Use `t.Helper()` in helpers and
table-driven subtests for tool/prompt matrixes.

## Hard constraints

- All file tools stay inside the project root, including symlink-escape checks for
  paths that do not exist yet. Do not add an escape hatch.
- API keys are never logged, never returned by the API (the UI sees a masked hint,
  and `__KEEP__` preserves the stored secret on update) and are stored in plain
  text in a `0600` database. Keep it that way.
- `--debug-llm` writes prompt and tool content to the log; treat that log as
  sensitive in any new code path too.
- No authentication exists: keep the loopback bind default and warn on non-loopback.
- Terms like "single binary", "no CGO", "no Node build step" and "offline UI" are
  product promises — a change that breaks one is a design change, not a detail.

## Architecture map (compact)

Startup: flags → config → logging → SQLite (WAL + migrate) → seed legacy
`openrouter` profile when no provider row exists → embed assets → router → listen
→ open browser if loopback → graceful shutdown.

One turn (`POST /api/chat/stream`): validate session/project → save the user
message → build the tools manager → ensure the repo outline → capture project
instructions once per session → build the system prompt once → load and prune
history → round loop. Each round streams reasoning/content, executes allowed tool
calls, records guard fingerprints, persists messages, and stops when the model
answers without tool calls. BUILD checkpoints every `max_tool_rounds` and
auto-continues up to `max_turn_rounds`; PLAN is one bounded research turn
(`plan_rounds` / `plan_tool_calls`) that never auto-continues and is forced to
produce a final plan when the budget is spent. `ask_user` persists a question
batch, emits a `questions` event and ends the turn.

SSE events: `round_start`, `reasoning`, `content`, `tool_start`, `tool_end`,
`tool_blocked`, `usage`, `checklist`, `questions`, `session`, `checkpoint`,
`limit`, `done`, `cancelled`, `error`. Runs survive the browser: the UI resumes
them through `/api/sessions/{id}/stream` and stops them through `…/stop`.

SQLite (all local, WAL, cascading deletes): `projects`, `sessions`, `messages`,
`checklists`, `question_batches`/`questions`/`question_options`, `repo_outline`,
`project_symbols`/`project_symbol_files`, `llm_providers`.

The repository map is built locally (Go AST for `.go`, regex scanners for other
languages), cached in SQLite, refreshed on change, and injected as the trailing
ephemeral state message. No LLM call is ever used to index a project — keep it
that way; indexing must work with no provider configured.
