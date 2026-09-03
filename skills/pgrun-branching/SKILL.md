---
name: pgrun-branching
description: Use when a coding task needs a real disposable Postgres — a migration, a schema change, a DB-dependent test, or "test this against real data." Creates an isolated PGRun branch, hands you its DATABASE_URL, and deletes it when you're done. Framework-independent (Rails, Django, Prisma, or anything else that reads DATABASE_URL).
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

## The workflow

0. **Decide the task actually needs a database** (see "When to use this").
   If it doesn't, don't branch.
1. **Check auth.** `pgrun auth status`. If it reports not configured (exit
   code 2), **tell the user to run `pgrun auth login` themselves** — it's an
   interactive prompt that reads their token with echo off and verifies it
   before saving. **Never ask the user to paste a token into the chat**; a
   token pasted into a conversation is a token that ends up somewhere it
   shouldn't (logs, chat history, a shared session). `pgrun auth login`
   exists specifically so that never has to happen.
2. **Get a branch.** Either step, or the one-liner in "Simplest path" below:
   - `pgrun branch create <project> --name <task-slug> --ttl 1h --wait --json`
     — waits for ready, and the JSON includes `database_url` directly (no
     second call needed).
   - or reuse one you already created: `pgrun branch env <project> <name>`.
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

**MCP tools** (`pgrun mcp serve`, four tools:
`pgrun_create_branch`/`pgrun_list_branches`/`pgrun_get_branch`/`pgrun_delete_branch`):
call `pgrun_create_branch{project,name,ttl}` with `wait` left at its default
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
