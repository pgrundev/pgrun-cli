# Task 2 report — MCP server, docs, release scaffolding, app-side agents.md

Status: DONE.

## Rider: Branch struct fix (real API field shape)

Before starting Task 2, fixed the `api.Branch` struct against the real
controller (`app/controllers/api/v1/branches_controller.rb#branch_json` in
`postgresrun_app`, read directly to confirm — not guessed):

```ruby
{
  "id" => "branch_#{branch.id}", "name" => branch.name, "status" => branch.api_status,
  "is_base" => branch.is_base,
  "parent_branch_id" => branch.parent_branch_id && "branch_#{branch.parent_branch_id}",
  "postgres_version" => @project.golden_replica&.pg_version,
  "created_at" => branch.created_at.iso8601, "expires_at" => branch.expires_at&.iso8601
}
```

- `internal/api/client.go`: `Branch.BaseBranch string` (Task 1's guess) →
  `Branch.IsBase bool` (`json:"is_base"`) + `Branch.PostgresVersion string`
  (`json:"postgres_version"`). `ParentBranchID` unchanged in type (string,
  null decodes to `""`) but now correctly typed against the real field.
- `internal/cli/output.go`: `branch list`'s BASE column now renders from
  `is_base` (`"yes"`/`"-"` via a new `boolDash` helper, not a base-branch
  name string that never existed); added a VERSION column from
  `postgres_version`, per the rider. `branch get`'s single line updated to
  match.
- Also confirmed against the model (`app/models/branch.rb`) that
  `parent_branch_id` is **never** null except for the base branch itself —
  `Branching::CreateBranch.call` always resolves an explicit parent (via
  `resolve_parent`, defaulting to the base row) before saving, so a child
  branch created with no `--parent` still gets a real `parent_branch_id`
  pointing at the base. This mattered for getting the app-side agents.md
  section right too (see below) — an earlier draft of that doc claimed
  parent_branch_id could be null on a non-base branch, which is wrong, and I
  caught and fixed it before committing.
- Added `internal/api/client_test.go` coverage locking in the exact JSON
  shape (`TestGetBranch_RealFieldShape`, `TestGetBranch_BaseBranchNullParent`)
  and updated `internal/cli/cli_test.go`'s table test to the real fixture
  shape.
- Also extracted the create `--wait` poll loop out of `internal/cli` into
  `api.Client.WaitForTerminal` (`internal/api/wait.go`, `api.PollInterval`
  var) so `internal/mcpserver` could reuse the exact same polling behavior
  instead of duplicating it — this was going to be needed for
  `pgrun_create_branch`'s `wait:true` anyway, so did it as part of the same
  pass rather than after.

## What was built (Task 2 proper)

- **`internal/mcpserver/server.go` + `tools.go` + `server_test.go`** —
  hand-rolled JSON-RPC 2.0 over stdio, one message per line, stdlib `bufio`
  + `encoding/json` only. `initialize` (serverInfo `{name:"pgrun",version}`,
  `capabilities:{tools:{}}`, `protocolVersion:"2024-11-05"`),
  `notifications/initialized` (no reply — and generically, any request with
  no `"id"` key is treated as a notification and never replied to, not just
  that one method), `tools/list`, `tools/call`, unknown method → `-32601`.
  Four tools, one per CLI branch subcommand
  (`pgrun_create_branch`/`pgrun_list_branches`/`pgrun_get_branch`/`pgrun_delete_branch`),
  dispatched through a `map[string]toolHandler` so an unknown tool name is a
  one-line lookup miss, not a growing switch. `pgrun_create_branch` reuses
  `api.Client.WaitForTerminal` for its default `wait:true`. Tool failures
  are `isError:true` results (the MCP business-logic-error convention, not
  a JSON-RPC protocol error) with a sanitized message built from
  `apiToolError` (401 → generic auth message, other `*api.APIError` → the
  server's own `.Message`, anything else → `err.Error()`, which never
  contains the token per Task 1's guarantee). A malformed input line
  produces a `-32700` parse-error reply and the loop keeps running — proven
  by a test that sends garbage then a well-formed request and checks the
  server still answers. Missing/incomplete env (`Client == nil`) doesn't
  fail startup — `initialize`/`tools/list` still work so a host can
  discover the tools before credentials exist; every `tools/call` then
  returns `isError` with the setup hint.
  `server_test.go` drives the loop over **real `io.Pipe`s** (not a
  pre-buffered `[]byte`), matching how an actual MCP host talks to a
  subprocess over stdio, against an `httptest` API: a full
  initialize→tools/list→create(wait:false)→list→get→delete session, the
  default `wait:true` poll path (with `api.PollInterval` shrunk for the
  test), a malformed line, an unknown tool, an unknown top-level method, a
  missing client, and an explicit proof that a notification produces zero
  output (the next request's response arrives with the *next* id, not a
  stray line from the notification).
- **`internal/cli/mcp.go`** — wires `pgrun mcp serve` into the existing
  dispatcher. This command is the one place that reads `os.Stdin` directly
  rather than through `Run`'s `stdout`/`stderr` parameters, since an MCP
  stdio server inherently needs the process's real stdin and
  `internal/mcpserver` already has its own pipe-driven tests that don't go
  through `cli.Run` at all — didn't want to change `Run`'s signature
  (accepted as part of Task 1) just for this one command.
- **`AGENTS.md`** — what pgrun is, install (release pending — brew/npm/`go
  install`, the last of which works today), auth, CLI + MCP examples, and
  the safety contract: branches are disposable, never point destructive
  work at a production DSN, always set a TTL for CI/agent use, base/parented
  branches can't be deleted, a branch's `DATABASE_URL` is a live credential
  while it exists.
- **`skills/pgrun-branching/SKILL.md`** — frontmatter name+description per
  the plan's exact wording; body: decision flow (need real/production-shaped
  data or a reachable DSN for something else → branch; otherwise local
  Postgres), the three ways in (CLI, MCP tools, raw curl), poll semantics,
  cleanup rules, and a failure-modes table (401/404/409×2/422/failed status).
- **`README.md`** — quickstart, install (source today, packaging pending),
  auth, full command table, exit codes, MCP section with the
  `.mcp.json`/Claude Desktop `mcpServers` config snippet, dev commands.
  Deliberately has no CI/release badges — there's no CI wired up yet, and a
  badge pointing at a nonexistent workflow would be lying to the reader.
- **`Makefile`**, **`.goreleaser.yaml`**, **`.gitignore`**, **`LICENSE`**
  (MIT — pgbot itself is Apache-2.0; the plan calls for MIT here) — adapted
  from pgbot's working release patterns, module/binary/tap-formula/npm-scope
  renamed to pgrun throughout. Kept the goreleaser file to what the plan
  actually asked for (build, archive, checksum, GitHub release, the
  `homebrew-tap` formula) and **dropped** pgbot's cosign signing, CycloneDX
  SBOMs, `.deb`/`.rpm` (nfpm), and Docker image publishing — those aren't
  mentioned in the plan and would be scaffolding for infrastructure
  (cosign identity, ghcr, a Dockerfile) this repo doesn't have and wasn't
  asked to add. **`npm/`** — `build.mjs` (assembles per-platform
  `@pgrun/<os>-<arch>` packages from goreleaser's `dist/artifacts.json`) and
  the `@pgrun/cli` wrapper package (`npm/pgrun/package.json` + `bin/pgrun.js`,
  which `spawn`s the right platform binary and forwards argv/stdio/signals/exit
  code) — adapted from pgbot's, exit codes on the JS side adjusted to
  *pgrun's* contract (`64` usage, `1` operation failure — pgbot's wrapper
  uses `3` for a launch failure because pgbot's own CLI uses a different
  code scheme; pgrun has no `3`, so that path is `1` here). Did **not**
  port pgbot's `npm/test/*` JS test harness (wrapper unit tests, a pack
  smoke script) — out of scope for scaffolding that's explicitly
  publish-deferred and not wired to CI; flagging this as the one piece of
  pgbot's npm setup not carried over, in case a future task wants it.
- **`postgresrun_app` branch `agents-branching-docs`** (off `main`,
  `2e94713`) — appended a `## Branching API` section to
  `app/views/pages/agents.text.erb`: auth (shares the dashboard token with
  the database-intelligence API), the feature-gate 404 note (worded to
  also cover the ordinary unknown-project/unknown-branch 404, not just the
  disabled case — an early draft implied 404 only ever meant "disabled,"
  which isn't true per `BaseController`'s `RecordNotFound` rescue), all
  five endpoints with request/response shapes, the branch object shape
  (cross-checked field-by-field against `branch_json` and `Branch#base_row_consistency`/`Branching::CreateBranch.call`
  — including catching and fixing my own first-draft error that
  `parent_branch_id` could be null on a non-base branch), the 10 statuses
  from `Branch::API_STATUS`, TTL semantics (`TTL_CHOICES`, no extend
  endpoint), the poll-until-ready flow, and the `connection_url` reveal
  rule (`show` only, never `create`/`index` — confirmed against the
  controller). Verified the ERB parses (`ERB.new(src).src` in a one-off
  Ruby check) before running the suite.

## Verification

pgrun-cli:
```
$ gofmt -l .
(empty)
$ go vet ./...
(clean)
$ go build ./...
(clean)
$ go test ./... -race -count=1
ok  	github.com/pgrundev/pgrun/internal/api	1.671s
ok  	github.com/pgrundev/pgrun/internal/cli	1.312s
ok  	github.com/pgrundev/pgrun/internal/config	1.449s
ok  	github.com/pgrundev/pgrun/internal/mcpserver	1.873s
```
55 passing subtests/tests across the four packages, 0 failures.

Also manually piped real JSON-RPC lines through the built binary
(`pgrun mcp serve`) and confirmed `initialize`/`tools/list` respond
correctly end to end (including that `SetEscapeHTML(false)` keeps
`"branch_<id>"` in tool descriptions from turning into
`"branch_<id>"`).

postgresrun_app (`agents-branching-docs`, via `bin/wt bin/test`):
```
916 runs, 3788 assertions, 0 failures, 0 errors, 1 skips
```
Matches main's baseline exactly (one pre-existing unrelated "missing
assertions" warning on `dns_reconcile_job_test.rb`, not a failure, not
touched by this change).

## Commits

pgrun-cli (`main`):
- `a4ccff9` — `fix: match Branch struct to the real API's JSON keys`
- `bd36cd9` — `feat: mcp server (pgrun mcp serve)`
- `47fd0f4` — `docs: AGENTS.md, skill, README, release scaffolding`

postgresrun_app (`agents-branching-docs`, branched off `main` @ `5a38d0c`):
- `2e94713` — `docs: branching API section in agents.md`

Branch left checked out back on `main` in `postgresrun_app` per instructions
— `2e94713` is not merged, just sitting on `agents-branching-docs` awaiting
integration.

## Concerns / follow-ups for whoever does Task 3

1. `agents-branching-docs` is unmerged — Task 3 (or whoever integrates it)
   needs to actually merge it into `postgresrun_app` main for the section to
   go live at `/agents.md`.
2. The MCP tool schemas (`internal/mcpserver/tools.go`) and the CLI's
   `--ttl` validation both hardcode the `1h`/`6h`/`24h`/`7d` enum — if the
   API's `TTL_CHOICES` ever changes, both need updating; there's no
   single source of truth shared between the Go client and the Rails app.
3. Didn't port pgbot's npm JS test harness (see above) — the npm packaging
   scaffolding is therefore less exercised than pgbot's; fine for
   publish-deferred scaffolding, worth revisiting before an actual npm
   publish.

## Review round 2 — controller-ruled fixes (F1-F4)

Four items came back from review. All four addressed; commits below.

**F1 — `Terminal()` only covered ready/failed.** A branch that got deleted,
stopped, or went unhealthy mid-`--wait` fell through to the timeout path
instead of stopping immediately — the poll loop kept polling a branch that
was never going anywhere until the caller's `--timeout`/`timeout_seconds`
finally expired, then reported a generic "timed out" instead of the real
reason. Fixed in `internal/api/wait.go`: `Terminal()` now covers all five
non-progressing statuses (`ready`, `failed`, `deleted`, `stopped`,
`unhealthy`); a new `WaitFailureReason(name, status)` gives one specific,
shared message per outcome (`"branch X was deleted while waiting"`, etc.),
called from both `internal/cli/branch.go` and
`internal/mcpserver/server.go#toolCreateBranch` so the two surfaces can't
drift on wording. Also fixed a real bug this surfaced while touching the
same code: `branch create --wait --json` was unconditionally exiting 0
after any successful wait, even when the terminal status was `failed` —
`--json` was masking the outcome instead of just changing how it's
reported. Same bug existed in the MCP path (`toolCreateBranch` always
returned a non-`isError` result after `WaitForTerminal` succeeded,
regardless of status) and is fixed the same way: check `branch.Status ==
StatusReady` explicitly before treating the wait as a success, in both
places.
Tests: `internal/api/wait_test.go` (`TestTerminal`, `TestWaitFailureReason`,
and `TestWaitForTerminal_DeletedStopsImmediately` — a scripted "deleted at
the 2nd poll" sequence against an `httptest` server with a generous 2s
context timeout, asserting it returns in well under 500ms so a regression
back to the timeout path would make the test visibly slow, not silently
green); `internal/cli/cli_test.go`
(`TestBranchCreate_Wait_DeletedSequence`, same shape, 10s timeout budget,
asserts <2s elapsed and that stderr never says "timed out";
`TestBranchCreate_Wait_JSON_FailedExitsFailure` locks in the exit-code fix);
`internal/mcpserver/server_test.go`
(`TestCreateBranch_Wait_DeletedIsError`, same pattern via `timeout_seconds`).

**F2 — MCP `pgrun_create_branch` didn't validate `ttl` client-side.** It
relied entirely on the server's 422, unlike the CLI's `branch create`. Fixed
by adding `api.ValidTTL(ttl string) bool` to `internal/api/client.go` (the
four accepted values, `""` excluded — callers check emptiness separately)
and calling it from **both** `internal/cli/branch.go` (replacing the
previous CLI-local `validTTL`, removing the exact duplication the reviewer
was worried about) and `toolCreateBranch` before ever calling `CreateBranch`.
A bad value returns `isError` naming the valid set: `"ttl must be one of
1h, 6h, 24h, 7d (got %q)"`.
Test: `TestCreateBranch_InvalidTTL_IsError` in `server_test.go` — points the
client at a handler that calls `t.Fatal` if hit at all, proving validation
happens before any request; `TestValidTTL` in `client_test.go` for the
shared function directly.

**F3 — no defense-in-depth against `.`/`..`/empty path segments.** Added
`InvalidNameError{Field, Value}` and `validatePathSegment` to
`internal/api/client.go`, called at the top of `CreateBranch` (project
only — branch `name` is a JSON body field for create, not a path segment),
`ListBranches` (project only), and `GetBranch`/`DeleteBranch` (both project
and name) — before any request is built. `handleAPIError` (CLI) checks for
`*api.InvalidNameError` first and maps it to **exit 64** (a malformed
argument, not an operation/API failure — deliberately distinct from the
generic exit-1 path every other client error takes); `apiToolError` (MCP)
maps it to `isError`. Centralizing the check inside the four `Client`
methods means the CLI and MCP server both get it for free from one place,
rather than needing the same guard duplicated in `internal/cli` and
`internal/mcpserver` separately.
Tests: `TestInvalidPathSegment_NeverHitsTheNetwork` in `client_test.go`
(table over `""`/`"."`/`".."`, both `project` and `name`, against a handler
that fails the test if it's ever invoked — proves the client short-circuits
rather than sending a dot-segment path an HTTP stack might normalize away);
`TestBranchCreate_InvalidProjectName_ExitsUsage` and
`TestBranchGet_InvalidBranchName_ExitsUsage` in `cli_test.go` (exit 64, same
never-hits-the-network handler); `TestCreateBranch_InvalidProjectName_IsError`
in `server_test.go`.

Verification after F1-F3 (before touching `postgresrun_app`):
```
$ gofmt -l . && go vet ./... && go build ./...
(all clean)
$ go test ./... -race -count=1
ok  	github.com/pgrundev/pgrun/internal/api	1.870s
ok  	github.com/pgrundev/pgrun/internal/cli	1.315s
ok  	github.com/pgrundev/pgrun/internal/config	1.644s
ok  	github.com/pgrundev/pgrun/internal/mcpserver	1.485s
```

**F4 — `postgresrun_app`, on `agents-branching-docs`:**
(a) The Branching section's `Base URL` line duplicated the `/branches` path
against the endpoint bullets (`.../api/v1/projects/:project/branches` +
`/projects/:project/branches` in each bullet). Fixed to end at `.../api/v1`,
matching the file's own Database-intelligence section's convention; the
bullets already carried the full `/projects/:project/branches...` paths, so
this was a one-line fix.
(b) The doc's 422-on-bad-`ttl` claim wasn't true server-side — `ttl_expires_at`
silently ignored anything outside `TTL_CHOICES` and created the branch with
no TTL. Added a guard at the top of both `create` actions (exact snippet the
reviewer specified for the API controller): `Api::V1::BranchesController#create`
now 422s `{"error"=>"invalid ttl — use 1h, 6h, 24h or 7d"}` before calling
`Branching::CreateBranch`; `ProjectBranchesController#create` mirrors it
(`flash.now[:alert]` + `render :new, status: :unprocessable_entity`) so a
forged POST bypassing the `<select>`'s four options gets the same rejection
the UI's own dropdown implies, instead of silently dropping the TTL.
Replaced the now-inaccurate `api/v1/branches_controller_test.rb` test
("invalid ttl choice leaves expires_at nil") with one asserting the 422 and
its exact message, and added a `ttl: ""` case to confirm the guard's
`.present?` check doesn't false-positive on "no TTL". Added a new
`project_branches_controller_test.rb` test for the forged-ttl 422 + alert
path. `skills/pgrun-branching/SKILL.md`'s failure-modes table already
claimed "422 on bad ttl" — checked it, no change needed, it's simply
accurate now.
`bin/wt` didn't exist in `postgresrun_app` (only in the `.worktrees/demo`
sibling checkout) — copied it over per instructions, stripping the
`PGRUN_BRANCHING_ENABLED`/`ADMIN_EMAIL` lines (already git-ignored, confirmed
via `git check-ignore -v bin/wt`).

Verification (`postgresrun_app`, on `agents-branching-docs`):
```
$ bin/wt bin/test 2>&1 | tail -3
918 runs, 3796 assertions, 0 failures, 0 errors, 1 skips
```
(916 baseline + 2 new tests = 918, matching the expected "918+/0F/1 skip".)
```
$ bin/wt bin/rubocop app/controllers/api/v1/branches_controller.rb app/controllers/project_branches_controller.rb
2 files inspected, no offenses detected
```

## Commits (round 2)

pgrun-cli (`main`):
- `b832be7` — `fix: terminal wait states, MCP ttl validation, path-segment refusal`

postgresrun_app (`agents-branching-docs`, still off `main` @ `5a38d0c`):
- `27ed192` — `fix: agents.md base URL; 422 on invalid ttl (API+UI)`

Checkout returned to `main` in `postgresrun_app` afterward, as instructed —
`27ed192` (like `2e94713` before it) lives only on `agents-branching-docs`,
awaiting merge.
