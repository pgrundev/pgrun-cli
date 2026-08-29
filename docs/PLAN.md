# pgrun agent kit — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Fresh repo — work directly on `main` (nothing exists to protect; controller-sanctioned).

**Goal:** Everything an agent needs to create/use/delete PGRun branches from anywhere: a standalone Go CLI (`pgrun`), an MCP server (`pgrun mcp serve`, same binary), an agents.md API contract section (served by the Rails app), and a Claude Code skill — all against the existing `/api/v1/projects/:project/branches` Bearer-token API.

**Architecture:** Go 1.22, STDLIB ONLY (no external deps — offline-buildable; local toolchain is 1.22.5). One module `github.com/pgrundev/pgrun`, one binary. MCP is hand-rolled JSON-RPC 2.0 over stdio (initialize / tools/list / tools/call — protocolVersion "2024-11-05"), not an SDK. Release scaffolding copied from pgbot's working goreleaser/brew/npm patterns but NOT wired to CI here (publishing is operator-gated).

**API contract consumed (existing, deployed dark; live on the demo):**
- Auth: `Authorization: Bearer <token>` (account-scoped ApiToken from the dashboard Tokens page).
- `POST /api/v1/projects/<project>/branches` body `{"name","ttl"?("1h"|"6h"|"24h"|"7d"),"parent_branch_id"?(id|"branch_<id>"|name)}` → 201 `{"id","name","status",...}`; 422 `{"error"}`; 409/404/401.
- `GET .../branches` → `{"branches":[{...}]}` (never connection_url).
- `GET .../branches/<name>` → branch json + `"connection_url"` iff ready+credentialed.
- `DELETE .../branches/<name>` → 202 `{"status":"deleting"}`; base → 409.
- Statuses: creating|snapshotting|provisioning|starting|ready|failed|deleting|deleted|stopped|unhealthy.

## Global constraints
- Stdlib only; `go vet ./...` + `go test ./...` + `go build ./...` green; gofmt-clean.
- The token is a secret: never in argv of subprocesses, never logged, never in error text; config file written 0600.
- CLI exit codes: 0 success (create --wait exits 0 only on ready), 1 operation/API failure, 2 auth/config missing, 64 usage.
- `--json` on every read/write command prints the raw API JSON (machine mode); default output is terse human lines; DATABASE_URL printed ONLY by `branch create --wait` (on ready) and `branch url` — the deliberate reveal points, matching the in-app CLI.
- MCP: tools mirror the CLI; tool results are `content:[{type:"text",text:<json>}]`; errors use `isError:true` with a sanitized message; the server must never crash on malformed input (reply JSON-RPC error).
- No Co-Authored-By trailers. House README tone: monospace-terse like pgbot.

## Task 1 — CLI core
**Create:** `go.mod`, `cmd/pgrun/main.go`, `internal/config/config.go`, `internal/api/client.go`, `internal/cli/cli.go` (+ per-command files as natural), tests `internal/api/client_test.go` (httptest), `internal/cli/cli_test.go`.
- Config resolution: flags `--url/--token` > env `PGRUN_API_URL`/`PGRUN_API_TOKEN` > file `~/.config/pgrun/config.json` (0600). `pgrun auth set --token T [--url U]` writes it; `pgrun auth status` prints url + token fingerprint (first 6 chars + "…"), never the token.
- Commands:
  - `pgrun branch create <project> --name <n> [--ttl 1h|6h|24h|7d] [--parent <name>] [--wait] [--timeout 300s] [--json]` — `--wait` polls GET every 2s until ready/failed/timeout; ready → print `DATABASE_URL=<connection_url>` (exit 0); failed → sanitized error (exit 1); no `--wait` → print status line (exit 0).
  - `pgrun branch list <project> [--json]` — table NAME STATUS BASE PARENT CREATED EXPIRES.
  - `pgrun branch get <project> <name> [--json]` (alias `status`) — human line or raw json; never prints connection_url in human mode.
  - `pgrun branch url <project> <name>` — ready+credentialed → `DATABASE_URL=...` exit 0; else sanitized reason exit 1.
  - `pgrun branch delete <project> <name>` — 202 → "deleting" exit 0; 409 → message exit 1.
  - `pgrun version`.
- api.Client: base URL join (no double slashes), 15s timeout per request, decodes `{"error"}` bodies into typed APIError{Status,Message}; 401 → distinct AuthError so the CLI can exit 2 with "set a token with `pgrun auth set`".
- Tests: httptest server covering every endpoint/status path incl. 401/409/422; poll loop with a scripted status sequence (creating→ready) and (creating→failed); config precedence; token never in `auth status` output; usage errors → 64.

## Task 2 — MCP server + skill + docs + release scaffolding + app-side agents.md
**Create (this repo):** `internal/mcpserver/server.go` + `server_test.go`, `AGENTS.md`, `skills/pgrun-branching/SKILL.md`, `README.md`, `Makefile`, `.goreleaser.yaml`, `.gitignore`, `LICENSE` (MIT, match pgbot).
**Modify (Rails app, branch `agents-branching-docs` off main in /Users/alex/GPT/postgresrun/postgresrun_app — see worktree note in dispatch):** `app/views/pages/agents.text.erb` — append a `## Branching API` section: auth, the five endpoints with request/response examples, statuses, TTL semantics, the poll-until-ready flow, connection_url reveal rule, and a one-line note that branching is feature-gated (404 when disabled). Run the app suite.
- MCP `pgrun mcp serve`: stdio loop, Content-Length-less line-delimited JSON (one JSON-RPC message per line — the common stdio transport); handle `initialize` (reply serverInfo{name:"pgrun",version}, capabilities{tools:{}}, protocolVersion "2024-11-05"), `notifications/initialized` (no reply), `tools/list`, `tools/call`, unknown → -32601. Tools (each schema'd):
  - `pgrun_create_branch{project,name,ttl?,parent?,wait?=true,timeout_seconds?=300}` → on wait: final branch json incl connection_url when ready; wait:false → immediate create response.
  - `pgrun_list_branches{project}`; `pgrun_get_branch{project,name}`; `pgrun_delete_branch{project,name}`.
  - Config via env only (MCP hosts pass env); missing token → tools/call returns isError with the setup hint.
- server_test: drive the loop via in-memory pipes against an httptest API: full session (initialize→tools/list→create wait:false→get→delete), malformed json line → JSON-RPC parse error not crash, unknown tool → isError.
- `AGENTS.md` (repo): what the tool is, install (brew/npm/go install — marked "release pending"), auth, CLI + MCP examples, the safety contract (branches are disposable; never point destructive work at a production DSN; TTL always for CI/agents).
- `skills/pgrun-branching/SKILL.md`: frontmatter name+description ("Use when a coding task needs a real disposable Postgres: create a PGRun branch before destructive DB work, use its DATABASE_URL, delete it after"); body: decision flow (need real data? → branch; else local PG), the three ways in (CLI, MCP tools, raw curl), poll semantics, cleanup rules, failure modes (401→token, 404→project/flag, 409→base/children).
- README: quickstart (auth set → branch create --wait → psql → delete), MCP host config snippet (claude_desktop/claude-code .mcp.json: command "pgrun", args ["mcp","serve"], env PGRUN_API_TOKEN), command table, exit codes.
- `.goreleaser.yaml`/Makefile: adapted from /Users/alex/GPT/postgresrun/pgbot/pgbot (module/binary renamed; brew tap pgrundev/homebrew-tap formula pgrun; npm @pgrun/cli) — checked in but publishing explicitly deferred.

## Task 3 — live e2e + finish (controller-run, not a subagent task)
Start demo Puma, mint demo token, run the BUILT binary end-to-end (create --wait → url → list → delete; then an MCP session via piped stdio), stop Puma. Record transcript in docs/E2E.md (redact token). Commit.
