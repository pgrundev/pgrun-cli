---
name: pgrun-branching
description: Use when a coding task needs a real disposable Postgres — create a PGRun branch before destructive DB work, use its DATABASE_URL, delete it after.
---

# PGRun branching

A PGRun branch is a real, connectable Postgres — copy-on-write off a
project's base branch, gone the moment you delete it or its TTL expires.
Use one whenever a task needs to run something against real(-ish) data that
you would not want to run against production: migrations, load tests,
"apply this and see what breaks," reproducing a bug against production-shaped
data.

## Decision flow

1. **Does the task need real/production-shaped data, or a database another
   service will actually connect to?** → create a branch (below).
2. **Is a local Postgres (docker, a test fixture, an in-memory sqlite
   stand-in) enough?** → use that instead. Don't reach for a branch just
   because a task touches SQL — branches are for when local data isn't
   representative enough, or when the task needs a real reachable
   `DATABASE_URL` to hand to something else (a deploy preview, a CI job, a
   second agent).
3. **Never run destructive work directly against a production DSN**,
   regardless of which path you took to get here.

## Three ways in

**CLI** (`pgrun`, see the repo's `AGENTS.md` for install):

```sh
pgrun branch create <project> --name <task-slug> --ttl 1h --wait
# DATABASE_URL=postgres://... on stdout, exit 0 — only on ready
psql "$DATABASE_URL" -c '...'
pgrun branch delete <project> <task-slug>
```

**MCP tools** (`pgrun mcp serve`, four tools:
`pgrun_create_branch`/`pgrun_list_branches`/`pgrun_get_branch`/`pgrun_delete_branch`):
call `pgrun_create_branch{project,name,ttl}` with `wait` left at its default
(`true`) — the result JSON carries `connection_url` once the branch is
ready. Call `pgrun_delete_branch{project,name}` when done.

**Raw curl**, when neither binary is available:

```sh
curl -sS -X POST "$PGRUN_API_URL/api/v1/projects/<project>/branches" \
  -H "Authorization: Bearer $PGRUN_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"<task-slug>","ttl":"1h"}'
# poll GET .../branches/<name> every ~2s until "status" is "ready" or "failed"
```

## Poll semantics

Branch creation is async. A fresh branch starts `creating`, moves through
`snapshotting`/`provisioning`/`starting`, and lands on `ready` or `failed`.
`connection_url` appears **only** on a `GET` of a single branch, and only
once it is `ready` and credentialed — never in the list endpoint, never
before ready. If you're not using the CLI's `--wait` or the MCP tool's
default `wait:true`, poll `GET .../branches/<name>` roughly every 2 seconds
and stop at `ready`/`failed`, with your own timeout (300s is a reasonable
default — provisioning shouldn't take that long, and something is wrong if
it does).

## Cleanup rules

- **Always set a TTL** (`1h`/`6h`/`24h`/`7d`) so an interrupted or forgotten
  agent doesn't leak a branch forever.
- **Delete explicitly when the task is done** — don't rely on the TTL as
  your only cleanup path; it's a backstop, not a plan.
- **The base branch can't be deleted**, and neither can a branch that has
  children (branch-from-branch). Both return 409. If you branched from a
  branch, delete the child before the parent.
- Deleting is async too (`202 {"status":"deleting"}`) — don't assume it's
  gone the instant the call returns if something else depends on that.

## Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `401` everywhere | No/invalid token | `PGRUN_API_TOKEN` (MCP) or `pgrun auth set --token <TOKEN>` (CLI); get a token from the dashboard's Tokens page |
| `404` on the project | Wrong project slug, or branching isn't enabled for this account | Confirm the project slug; branching may simply be dark for this account/environment |
| `409` on create | Name already taken in this project | Pick a different name, or delete/reuse the existing branch |
| `409` on delete | Deleting the base branch, or a branch with children | Delete children first; never delete the base programmatically |
| `422` on create | Bad `ttl` (must be `1h`/`6h`/`24h`/`7d`) or invalid name | Fix the request body — the error message names the field |
| Branch status `failed` | Provisioning failed server-side | Don't retry blindly in a loop; surface the failure, try a fresh name |

Never expose a branch's `DATABASE_URL` in a task's public-facing output —
it's disposable, but it's still a live credential while the branch exists.
