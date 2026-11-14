---
name: pgrun-branching
description: Use when a coding task needs a real disposable Postgres — a migration, a schema change, a DB-dependent test, or "test this against real data" — or when you need to check a schema change before finishing. Creates an isolated PGRun branch (PGX by default where the project allows it, PostgreSQL otherwise), hands you its DATABASE_URL, deletes it when you're done, and verifies migrations against real PostgreSQL with PgBot (`pgrun bot verify`). Framework-independent (Rails, Django, Prisma, or anything else that reads DATABASE_URL).
---

# pgrun branching

**pgrun is not a Rails integration.** The primitive is: every coding agent
gets its own isolated, disposable Postgres, on demand, from the command
line. Rails is one first-class example of a consumer — so are Django,
Prisma, a raw Go/Node/Python script, a CI job, or a second agent. If the
task needs a real `DATABASE_URL` and you don't want to risk anything that
matters, this skill applies regardless of what's reading that URL.

A PGRun branch is copy-on-write off a project's base branch, ready in about
the time a normal database takes to boot, gone the moment you delete it or
its TTL expires.

## What you have

1. **PGRun creates isolated database branches** — copy-on-write copies of the project's Safe
   Copy (a privacy-safe copy of production), one per task, deleted when you are done.
2. **PGX is the default runtime** where the project allows it: a lightweight
   PostgreSQL-compatible engine, so a branch is cheap and fast to start. The server decides per
   branch and falls back to PostgreSQL when PGX cannot serve the project (an extension it lacks,
   a large base); the branch JSON's `engine` and `engine_reason` say which and why.
3. **PgBot inspects and verifies database changes**, read-only: schema, lint findings, query
   plans, and a migration review that flags table rewrites, blocking index builds, drops and
   renames.
4. **PostgreSQL verification is on demand**: `pgrun bot verify` runs your migration on a
   disposable *PostgreSQL* branch of the same Safe Copy, reports, and deletes it.

**First use in a session:** run `pgrun setup --json` (idempotent). It installs or refreshes this
skill and PgBot and reports `ready`, the `capabilities` available, and any `next` steps. If
`auth` is `missing`, tell the user to run `pgrun auth login` themselves. If `pgrun` itself is not
installed (you got this skill some other way), ask the user to install it —
`curl -fsSL https://pgrun.dev/install | sh` — rather than running an installer yourself; then
`pgrun setup --json`. Both paths end in the same setup.

## When to use this

- A migration, a schema change, or anything with `ALTER`/`DROP` in it.
- A test suite (or one test) that needs a real Postgres, not a mock.
- "Try this against real(-ish) data and see what breaks."
- Reproducing a bug that only shows up against production-shaped data.
- Anything else that would hand a `DATABASE_URL` to a second process (a
  deploy preview, a CI job, another agent).

Don't reach for a branch just because a task touches SQL — if a local
Postgres (docker, a fixture, sqlite standing in) is representative enough,
use that instead, it's faster. And regardless of which path got you here:
**never run destructive work directly against a production DSN.**

## Before the first branch

Some projects branch off a Safe Copy — a one-time, privacy-safe copy of a
connected Production Database, set up once by a human. If
`pgrun branch create` refuses (a 404, or any error naming a missing Safe
Copy) or `pgrun source status <project>` doesn't show `✓ Safe Copy ready`,
**stop — don't work around it.** Connecting the Production Database and
approving what gets copied, faked, or dropped are decisions for the human:

- Tell the human to run, themselves: `pgrun source status <project>` →
  `pgrun source protect <project>` (they review the table and add
  `--approve`) → `pgrun source copy <project> --wait`.
- Never run `pgrun source add`/`pgrun source update` on the human's behalf,
  and never ask them to paste their production connection URL into the
  chat — it goes straight into the hidden `Connection URL (input hidden):`
  prompt (or a `--url` flag they type themselves), never through you.
- Never pass `--approve` yourself. `pgrun source protect` only ever prints
  a review table — the human reads it and approves it, not you.

Once `pgrun source status <project>` reports `✓ Safe Copy ready`, branch
normally — the rest of this skill applies unchanged.

## The workflow

0. **Decide the task actually needs a database** (see "When to use this").
   If it doesn't, don't branch.
1. **Check auth.** `pgrun auth status`. If it reports not configured (exit
   code 2), **tell the user to run `pgrun auth login` themselves** — it
   opens their browser, they click Authorize, and the token never appears
   anywhere they could paste it. **Never ask the user to paste a token into
   the chat**; a token pasted into a conversation is a token that ends up
   somewhere it shouldn't (logs, chat history, a shared session). `pgrun
   auth login` exists specifically so that never has to happen.
2. **Get a branch.** Either step, or the one-liner in "Simplest path" below:
   - `pgrun branch create <project> --name <task-slug> --ttl 1h --wait --json`
     — waits for ready, and the JSON includes `database_url` directly (no
     second call needed). The server picks the engine (PGX Beta when the
     project is eligible, else PostgreSQL); add `--engine postgres` when the
     task needs the source's exact engine, extensions or settings, or
     `--engine pgx` to insist on PGX (an `engine_unavailable` error that
     names the reason and offers PostgreSQL, never a silent switch). An
     engine you name explicitly is always respected.
   - or reuse one you already created: `pgrun branch env <project> <name>`.
> The URL is a TLS endpoint on a per-branch hostname and already contains
> `sslmode=require`. Use it exactly as given — do not rewrite the host, the port
> or the sslmode, and do not expect an IP address. An unencrypted connection is
> refused. `channel_binding=require` is not supported.

3. **Get `DATABASE_URL`.** From step 2's JSON (`database_url`), or:
   ```sh
   eval "$(pgrun branch env <project> <task-slug>)"   # exports DATABASE_URL
   ```
4. **Run the work with `DATABASE_URL` injected via environment only:**
   ```sh
   DATABASE_URL="$DATABASE_URL" <migration or test command>
   ```
   - **Never edit `config/database.yml`, `.env`, `.env.local`, or any other
     project config file** to point at the branch. The whole point is that
     nothing in the repo changes — set the env var for that one command
     and nothing else.
   - **Never commit the branch's credentials anywhere** — not to a file, not
     to a commit, not to a PR description. They're real, live credentials
     for as long as the branch exists.
5. **Verify** the migration/test actually did what it was supposed to
   (check the output, query the branch, whatever the task calls for).
6. **Delete the branch:** `pgrun branch delete <project> <task-slug>`. Do
   this even though the TTL would eventually clean it up — TTL is the
   backstop for a crashed/forgotten agent, not the plan.

## Simplest path: `branch exec`

For "run one command against a fresh branch, then throw it away," skip
steps 2–6 above entirely:

```sh
pgrun branch exec myproject --create agent-task-42 --ttl 1h --delete-after -- \
  bin/rails db:migrate
```

This creates the branch, waits for it to be ready, runs the command with
`DATABASE_URL` set in its environment (never on argv, never in a file),
and deletes the branch afterward — **even if the command fails.** Exit code
is the command's own exit code. This is the default choice unless something
about the task needs the branch to outlive one command (e.g., a migration
followed by a separate manual verification step).

## Verify before you finish: PgBot

PgBot is read-only. `pgrun bot` only ever inspects PGRun branches (it resolves them through the
API with the project's token and never accepts a connection string), so it cannot reach
production. Its optional AI layer is off: nothing is sent to a model provider.

**When to invoke it**

| Situation | Command |
|---|---|
| You wrote or changed a migration (any `ALTER`, `CREATE INDEX`, `DROP`, rename) | `pgrun bot verify --json -- <migrate command>` before you report the task done |
| You developed against a PGX branch and the change is schema-level | add `--compare-pgx`: same migration on PGX and PostgreSQL, schemas compared |
| You need the current schema of a branch (tables, columns, indexes, constraints) | `pgrun bot schema <branch>` (JSON, catalog only, no data) |
| A query is slow or you added a query on a large table | `pgrun bot explain <branch> "<SELECT …>" --json` (plan only, never executed) |
| Schema health after a change (missing FK indexes, invalid/duplicate indexes) | `pgrun bot lint <branch> --json` |
| A database error you don't understand, or "is this database healthy?" | `pgrun bot inspect <branch> --json` |

Don't run `verify` after every small query or test edit — it provisions a PostgreSQL branch.
Run it once per migration change, and again only if you change the migration.

**Reading `pgrun bot verify --json`** (`kind: pgrun-verify`):

- `verdict`: `pass` · `dangerous` (a schema-review finding at/above `--fail-on`, default
  `critical`) · `fail` (the migration exited non-zero on PostgreSQL) · `error` (no branch).
  Exit code 0 only for `pass`.
- `schema_review.findings[]`: `id`, `severity`, `object`, `detail`, `advice`. Act on
  `critical`/`warning`: `table_rewritten`, `index_created_on_existing_table`,
  `constraint_validated_on_existing_table`, `not_null_added` are lock hazards on large tables;
  `table_dropped`/`column_dropped` lose data; `*_renamed` breaks code that is still deployed.
  Sizes are the Safe Copy's — production may be larger.
- `new_lint_findings[]`: schema findings the migration introduced.
- `pgx_compatible` / `pgx_comparison` (with `--compare-pgx`): whether PGX produced the same
  schema and exit code. If not, verify and ship against PostgreSQL behaviour, and say so.
- `cleanup[]`: every verification branch and whether it was deleted.

Report the verdict and the non-info findings to the user in plain words; fix what you can (for
example `algorithm: :concurrently`, `NOT VALID` + `VALIDATE CONSTRAINT`) and verify again.

If `pgrun bot status` says pgbot is not installed, run `pgrun bot install` (or `pgrun setup`).
If it says the `schema` review is unavailable, `verify` still runs the migration on PostgreSQL
and lints; say that the rewrite/lock review was skipped.

## Framework detection (examples, not the whole story)

Detect the project's stack from what's on disk, then run its normal
migrate/test command with `DATABASE_URL` set — that's the entire pattern.
These four are common starting points, not an exhaustive list:

| Signal on disk | Framework | Command |
|---|---|---|
| `Gemfile` + `bin/rails` + `config/database.yml` | Rails | `DATABASE_URL="$DATABASE_URL" bin/rails db:migrate`, then the relevant tests against the **same** branch: `DATABASE_URL="$DATABASE_URL" bin/rails test`. **Do not modify `config/database.yml`** — Rails reads `DATABASE_URL` from the environment automatically when it's set. |
| `manage.py` | Django | `DATABASE_URL="$DATABASE_URL" python manage.py migrate` |
| `prisma/schema.prisma` | Prisma | `DATABASE_URL="$DATABASE_URL" npx prisma migrate deploy` |
| anything else | generic | `DATABASE_URL="$DATABASE_URL" <the project's normal migrate/test command>` |

If a project has its own idiosyncratic way of taking a `DATABASE_URL`
(a wrapper script, a Makefile target), prefer that over inventing a raw
framework command — the pattern is "inject the env var," not "always type
`bin/rails` literally."

## Three ways in

**CLI** — see above, plus `pgrun auth login`/`pgrun auth status`.

**MCP tools** (`pgrun mcp serve`: `pgrun_create_branch`/`pgrun_list_branches`/
`pgrun_get_branch`/`pgrun_delete_branch`, plus read-only PgBot tools `pgrun_bot_lint`,
`pgrun_bot_schema`, `pgrun_bot_explain` that take `{project, name}`; migration
verification stays the CLI's `pgrun bot verify`):
call `pgrun_create_branch{project,name,ttl[,engine]}` with `wait` left at its default
(`true`) — the result JSON carries `database_url` (and `connection_url`)
once the branch is ready. Call `pgrun_delete_branch{project,name}` when
done.

**Raw curl**, when neither binary is available:

```sh
curl -sS -X POST "$PGRUN_API_URL/api/v1/projects/<project>/branches" \
  -H "Authorization: Bearer $PGRUN_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"<task-slug>","ttl":"1h"}'
# poll GET .../branches/<name> every ~2s until "status" is "ready" or "failed"
```

## Poll semantics

Branch creation is async: `creating` → `snapshotting`/`provisioning`/`starting`
→ `ready` or `failed`. `connection_url`/`database_url` appear only on a
`GET`/`branch env`/`branch exec` of a single ready, credentialed branch —
never in the list endpoint, never before ready. `--wait` (CLI) and
`wait:true` (MCP, the default) handle this polling for you; if you're doing
it by hand, poll every ~2s with a ~300s timeout.

## Never

- Modify `config/database.yml` (or any framework equivalent) to point at a branch.
- Write branch credentials into `.env`, `.env.local`, or any file in the repo.
- Commit, paste, or print a branch's `DATABASE_URL` anywhere public.
- Run destructive work (migrations, `DROP`, load tests) against a production DSN.
- Ask the user to paste a token into the chat — `pgrun auth login` exists for that.

## Running a test suite against a branch

A branch carries REAL data. Most test suites assume an empty, disposable
database and will destroy that data before a single migration is tested. Three
specific hazards, all observed on a real run:

- **`fixtures :all` truncates every fixture table first.** On one app that was
  ~18,500 real rows, and because branch roles are SUPERUSER the truncate is not
  even stopped by foreign keys. The surviving non-fixture tables are then
  orphaned and every test errors on FK violations. This is Rails behaving
  normally, not a pgrun defect.
- **`maintain_test_schema!` purges and reloads the schema** when
  `schema_migrations` looks stale. That is what destroys a branch outright. Set
  `config.active_record.maintain_test_schema = false` for the run.
- **Parallel testing creates EXTRA databases on the branch instance.**
  `parallelize(workers: :number_of_processors)` makes `<db>-0`, `<db>-1`, … and
  loads `schema.rb` into each. Run with `PARALLEL_WORKERS=1`.

So: a branch is excellent for **migrations, console work, and querying real
data**. A fixture-based suite is a different job. If you must run one, say so
plainly first and let the user choose, because the data goes either way.

**If a migration fails, never "repair" it by loading the schema.**
`db:schema:load`, `db:reset`, `db:setup`, `db:prepare` and `db:test:prepare`
all purge the branch. An empty `schema_migrations` means the branch is faulty —
report it and create a new branch. Do not stamp it and do not reload it.

## Getting the URL into a variable

`branch url` prints `DATABASE_URL=<url>`, not a bare URL, so strip the prefix
or use the JSON form:

```bash
U="$(pgrun branch url <project> <branch>)"; U="${U#DATABASE_URL=}"
# or
U="$(pgrun branch env <project> <branch> --format=json | jq -r .database_url)"
```

Prefer `pgrun branch exec` where it fits — it puts `DATABASE_URL` straight into
the child process and never prints it.

## Cleanup rules

- **Always set a TTL** (`1h`/`6h`/`24h`/`7d`) so a crashed or forgotten agent
  doesn't leak a branch forever.
- **Delete explicitly when done anyway** (`pgrun branch delete`, or
  `branch exec --delete-after`) — TTL is the backstop, not the plan.
- **The base branch can't be deleted**, and neither can a branch with
  children (branch-from-branch) — both return 409. Delete children first.
- Deleting is async too (`202 {"status":"deleting"}`) — don't assume it's
  instantly gone if something else depends on that.

## Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `pgrun auth status` exits 2 | Not authenticated | Tell the user to run `pgrun auth login` — never ask them to paste a token into the chat |
| `401` everywhere | Token missing/rejected | Same as above — `pgrun auth login` re-verifies and re-saves it |
| `404` on the project | Wrong project slug, or branching not enabled for this account | Confirm the project slug with the user |
| `409` on create | Name already taken in this project | Pick a different name, or delete/reuse the existing branch |
| `409` on delete | Deleting the base branch, or a branch with children | Delete children first; never delete the base programmatically |
| `422` on create | Bad `ttl` (must be `1h`/`6h`/`24h`/`7d`) or invalid name | Fix the request — the error names the field |
| Branch status `failed` | Provisioning failed server-side | Don't retry blindly in a loop; surface the failure, try a fresh name |
| `branch env`/`branch url` exits 1 | Branch isn't ready yet, or has no credentials yet | Wait/poll, or use `--wait`/`wait:true` next time |

Never expose a branch's `DATABASE_URL` in a task's public-facing output —
it's disposable, but it's a live credential for as long as the branch
exists.
