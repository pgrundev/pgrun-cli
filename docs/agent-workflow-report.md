# Agent-workflow report — auth login/logout, branch env/exec, database_url

Status: DONE.

## What was built

### 1. `pgrun auth login` / `pgrun auth logout` (`internal/cli/auth_login.go`)

`auth login` is a friendly interactive onboarding flow:

- Prompts for the API URL, defaulting to the existing config's URL if any,
  else a new `config.DefaultURL` built-in constant (`https://app.pgrun.dev`
  — confirmed against `postgresrun_app`'s `config/environments/production.rb`,
  which sets `MAIL_HOST` to the same host; the dashboard and the API are
  served from the same Rails app on that host per `config/routes.rb`).
- Prints the dashboard's Tokens page URL (derived from the URL just
  entered — the API and the dashboard share a host, so there's nowhere else
  to point) right before prompting for the token.
- Reads the token with terminal echo disabled via `stty -echo`/`stty echo`
  through `os/exec`, gated on `stdin == os.Stdin` — only meaningful when
  reading the process's real standard input, since that's the only stream a
  child `stty` process (given `cmd.Stdin = os.Stdin`) can affect. If `stty`
  fails (not installed, or stdin isn't actually a terminal — e.g. piped
  input), falls back to a visible read with a printed warning, never hangs.
  Verified against a real pseudo-terminal (Python's `pty` module) that echo
  is genuinely suppressed, and against piped input that the fallback
  triggers correctly with no hang.
- Verifies the token with one real API call — `Client.VerifyToken`
  (`internal/api/client.go`) calls `ListBranches` against an
  intentionally-bogus project slug; a 401 means the token was rejected, a
  404 (or any other non-401 response) means it got past auth. Only saves
  the config (0600) on a confirmed-good token; a bad token exits 2 and
  writes nothing.
- On success, prints the token's fingerprint only (never the token) and the
  saved URL.

`auth logout` deletes the config file outright, exits 0 whether or not one
existed.

`stdin` is a parameter to `authLogin` (not read from `os.Stdin` directly),
which is what makes it unit-testable: tests pass a plain `strings.Reader`,
which — being neither the sentinel `os.Stdin` value — skips the `stty` path
entirely and reads visibly, exercising exactly the same code path as the
"no TTY" production fallback. `runAuth` wires `os.Stdin` in for the real
CLI, the same pattern `mcp serve` already used for reaching past `Run`'s
stdout/stderr-only signature.

`auth set --token` is unchanged in behavior, documented in-code and in
AGENTS.md/README as the advanced/manual/CI path. `authHint` (shown on 401
and on missing config) now leads with `auth login` while still containing
the literal substring `pgrun auth set` that existing tests assert on.

### 2. `pgrun branch env <project> <name>` (`internal/cli/branch_env.go`)

Prints `export DATABASE_URL="postgres://..."` (POSIX double-quoted,
escaping `\ " $` `` ` `` so it's safe to `eval`) once the branch is ready and
credentialed; `--format=json` prints `{"database_url":"..."}` instead.
Not-ready or ready-but-uncredentialed both exit 1 with a sanitized reason,
matching `branch url`'s existing reveal-point discipline (raw API JSON is
never printed by this command).

### 3. `pgrun branch exec <project> <name> -- <command...>` and `--create` (`internal/cli/branch_exec.go`)

Resolves a branch's `DATABASE_URL`, execs `<command>` via `os/exec` with
`DATABASE_URL` appended to the child's environment only (never argv, never
a file), stdio passed through (`stdin` is the real `os.Stdin`; `stdout`/
`stderr` are whatever `Run` was given), and returns the child's own exit
code (`*exec.ExitError.ExitCode()`).

`--create <newname> [--ttl] [--from <parent>] [--delete-after] [--timeout]`
creates a fresh branch, waits for ready via the existing
`Client.WaitForTerminal`, runs the command, and — with `--delete-after` —
deletes the branch afterward. The delete is registered via `defer`
immediately after a successful `CreateBranch` (not just around the exec
call), so it fires on every exit from that point on: a failed/timed-out
wait, or any exit code from the command — stronger than the spec's stated
minimum ("even on command failure") on purpose, since a stuck-mid-provision
branch shouldn't leak either.

Argument grammar: `<project>` is always first; exactly one of a second
positional `<name>` or `--create <newname>` selects the branch (usage error
if both or neither); everything after a literal `--` is the command
(`flag.FlagSet` already treats `--` as its own terminator, so this falls
out of `fs.Parse` once the leading positionals are peeled off by hand, the
same technique `splitPositional` already used elsewhere in this codebase —
just generalized here to a variable-length prefix). `--ttl`/`--from`/
`--delete-after` without `--create` is a usage error.

### 4. `database_url` in `branch create --wait --json` and MCP `pgrun_create_branch`

`api.WithDatabaseURL(raw, url)` (`internal/api/database_url.go`) decodes
the response as `map[string]json.RawMessage`, adds `database_url`, and
re-encodes — every other field's bytes are preserved exactly as the server
sent them (not reinterpreted), only the new field and the enclosing object
are freshly marshaled. Falls back to `raw` unmodified if it doesn't decode
as an object. Shared by `internal/cli/branch.go` (`branchCreate`'s `--wait`
ready path) and `internal/mcpserver/server.go` (`toolCreateBranch`'s ready
path) — same pattern as `api.ValidTTL`/`api.WaitFailureReason` already
being shared to avoid two copies that could drift.

### 5. Skill rewrite (`skills/pgrun-branching/SKILL.md`)

Kept the name `pgrun-branching`. Rewritten framework-independent: leads
with "pgrun is not a Rails integration — every coding agent gets its own
isolated Postgres," a numbered workflow (check auth → tell the user to run
`pgrun auth login` if unauthenticated, never ask them to paste a token →
create/reuse a branch → get `DATABASE_URL` → run migrations/tests with the
env var injected, never editing `database.yml`/`.env`/`.env.local`, never
committing credentials → verify → delete), a "simplest path" callout for
`branch exec --create --delete-after`, and a framework-detection table
(Rails/Django/Prisma/generic) explicitly framed as examples, not the whole
story.

### 6. AGENTS.md / README.md

Both updated: `auth login`/`logout` as the everyday path, `auth set` framed
as advanced/CI, `branch env`/`branch exec` documented with examples, the
`brew install → pgrun auth login → claude` day-one snippet, `database_url`
noted in both the CLI `--json` and MCP tool docs, two new safety-contract
points in AGENTS.md (never ask a human to paste a token; inject
`DATABASE_URL` via environment only, never edit project config files).
Publishing-deferred framing (`.goreleaser.yaml`/npm checked in, not wired
up) left untouched in both.

## Testing

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean, `go test
./...` green — 68 tests total across `internal/api`, `internal/cli`,
`internal/config`, `internal/mcpserver` (up from 47 before this work).

New coverage:
- `internal/api/client_test.go`: `VerifyToken` (good token / 200, good
  token / 404-on-bogus-project, bad token / 401 → `*AuthError`),
  `WithDatabaseURL` (adds the field and preserves every other field;
  malformed input falls back unmodified).
- `internal/cli/auth_login_test.go`: happy path (verify call mocked via
  httptest, config saved at 0600, fingerprint-only output, dashboard URL
  printed); bad token (401 → exit 2, nothing saved, no leak in
  stdout/stderr); default-URL prefill from an existing config; empty-token
  failure; usage error; `auth logout` clears config and is idempotent
  (exit 0 already-logged-out).
- `internal/cli/branch_env_test.go`: ready (export line, exact match),
  `--format=json`, not-ready → exit 1, ready-but-uncredentialed → exit 1,
  bad `--format` → usage, missing args → usage, `shellDoubleQuote` escaping
  table.
- `internal/cli/branch_exec_test.go`: a `TestMain` re-exec harness (the
  standard Go idiom, no external commands needed) drives `branch exec`
  against a real child process — asserts `DATABASE_URL` arrives via the
  child's actual environment (not argv/a file), exit-code passthrough,
  not-ready exits without ever invoking the command, `--create
  --delete-after` deletes on both command failure and success (httptest
  asserts the `DELETE` call happened), and a table of usage errors (missing
  project, neither name nor `--create`, both name and `--create`, missing
  command after `--`, `--ttl` without `--create`, bad `--ttl` value).
- `internal/cli/cli_test.go` / `internal/mcpserver/server_test.go`: one
  test each locking in `database_url` in `branch create --wait --json` and
  the MCP `pgrun_create_branch` ready result, alongside the original
  fields.

Manually smoke-tested the built binary end-to-end beyond the unit suite
(not checked in, ad hoc against a throwaway local HTTP server): `auth
login` over a real pty (echo genuinely suppressed, no warning) and over a
piped non-TTY stdin (graceful fallback warning, no hang, login still
succeeds); `branch env` (both formats); `branch exec ... -- env` (confirmed
`DATABASE_URL` present in the real child process's environment via `env`).

## Notes / judgment calls

- `config.DefaultURL` (`https://app.pgrun.dev`) is new — there was no
  built-in default anywhere in this repo before. Confirmed against the
  Rails app rather than guessing.
- `branch env`'s `export DATABASE_URL="..."` uses a small POSIX
  double-quote escaper (backslash/`"`/`$`/backtick) rather than Go's `%q`,
  since `%q` produces Go-string escaping (not shell-safe) and doesn't
  neutralize `$`/backtick, which double quotes alone don't stop from
  expanding.
- `branch create`/`pgrun_create_branch`'s `database_url` augmentation only
  touches the one ready+`--wait`/`wait:true` path called out by the spec —
  `branch get --json` and `branch list --json` are untouched (still pure
  verbatim passthrough), matching the literal scope asked for.
- No changes made to the `postgresrun_app` Rails repo or its
  `agents.text.erb` — out of scope for this task (that's Task 2's app-side
  rider in `docs/PLAN.md`, a separate worktree/branch).

## Concerns

- None outstanding. All four numbered CLI/MCP items plus the skill/docs
  rewrite are implemented, tested, and manually verified against the real
  binary.
