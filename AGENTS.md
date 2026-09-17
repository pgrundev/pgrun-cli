# pgrun — for coding agents

`pgrun` creates, uses, and deletes disposable PGRun Postgres branches from
the command line or over MCP. A branch is a real, connectable Postgres —
copy-on-write off a project's base branch, ready in roughly the time it
takes a normal database to boot, gone the moment you delete it or its TTL
expires. It exists so an agent doing destructive database work (migrations,
load tests, "try this and see what breaks") never has to touch anything that
matters.

One static Go binary, two API families: `/api/v1/projects/:project/branches`
(branches) and `/api/v1/sources` (Safe Copy setup — see Sources below),
Bearer-token auth. No daemon, no state beyond a 0600 config file holding a
URL and a token.

## Install

Release scaffolding (`.goreleaser.yaml`, `Makefile`, npm packaging) is
checked into this repo but publishing is not yet wired up — **release
pending**. Once it ships:

```sh
curl -fsSL https://pgrun.dev/cli/install | sh     # installer script
brew install pgrundev/tap/pgrun                    # Homebrew
npx @pgrun/cli branch list <project>                # npm, no install
go install github.com/pgrundev/pgrun/cmd/pgrun@latest  # works today
```

Until then, build from source:

```sh
git clone https://github.com/pgrundev/pgrun
cd pgrun && go build -o pgrun ./cmd/pgrun
```

The intended day-one UX, once release scaffolding is live:

```sh
brew install pgrundev/tap/pgrun
pgrun auth login          # opens your browser to authorize; --paste to paste a token instead
pgrun skill install       # installs the pgrun-branching skill into ~/.claude/skills
claude                    # "Add this migration and test it." — Claude branches, migrates, tests, cleans up
```

`pgrun skill install` writes the skill embedded in the binary (this repo's
`skills/pgrun-branching/SKILL.md`) to `~/.claude/skills/pgrun-branching/`;
`--project` targets `./.claude/skills`, `--dir` any other skills root, and
`pgrun skill status` exits 0 only when the installed copy matches.

## Auth

**`pgrun auth login`** is the everyday path: it opens the browser to
`app.pgrun.dev`, the person clicks **Authorize**, and the CLI receives a
token minted just for it (named "pgrun CLI on `<hostname>`", revocable any
time from the Tokens page) — it's never shown to them, and confirmed only by
printing the account it logged into. Over SSH, or with `--no-browser`, it
prints a URL and code to open on any device instead. It never prompts for
the API URL — `--url`, then `PGRUN_API_URL`, then the saved config, then
pgrun's built-in default (`https://app.pgrun.dev`).

```sh
pgrun auth login
```

`--paste` keeps the older flow for machines with no browser anywhere: a
direct link to the dashboard's Tokens page (`<url>/accounts/default/tokens`),
read with terminal echo off, verified against the API before anything is
saved.

`pgrun auth logout` removes the saved config (exit 0 even if you were
already logged out). `pgrun auth status` prints the configured URL and a
6-character token fingerprint — never the token itself.

**`pgrun auth set --token <TOKEN> [--url <URL>]`** is the advanced/manual/CI
path: writes straight to the config file, no prompt, no verification call,
nothing that touches a terminal. Prefer this (or the env vars below) for
scripts and CI. Whichever path you use, `PGRUN_API_URL` / `PGRUN_API_TOKEN`
in the environment win over the config file, and `--url`/`--token` flags win
over both — that's how CI overrides a developer's local login without
touching their config file.

An agent should never ask a human to paste a token into a chat session —
tell them to run `pgrun auth login` themselves instead; see the safety
contract below.

## CLI examples

```sh
# the headline agent primitive: create a branch, run one command against
# it, delete it — even if the command fails
pgrun branch exec myproject --create agent-task-42 --ttl 1h --delete-after -- \
  bin/rails db:migrate

# equivalent, done by hand across separate calls
pgrun branch create myproject --name agent-task-42 --ttl 1h --wait --json
# {"id":"...","name":"agent-task-42","status":"ready","database_url":"postgres://...",...}

eval "$(pgrun branch env myproject agent-task-42)"   # exports DATABASE_URL
DATABASE_URL="$DATABASE_URL" bin/rails db:migrate

# check on it later, run an existing branch's DATABASE_URL through a command,
# delete when done
pgrun branch get myproject agent-task-42
pgrun branch exec myproject agent-task-42 -- psql -c 'select 1'
pgrun branch url myproject agent-task-42   # DATABASE_URL=... or a reason, exit 1
pgrun branch delete myproject agent-task-42
```

Exit codes are a stable interface: `0` success (and, for `branch exec`, the
exec'd command's own exit code), `1` operation/API failure (bad request, 409
on a base/parented branch, branch failed or timed out), `2` auth/config
missing or rejected, `64` usage (bad flags/args). `--json` on
`create`/`list`/`get`/`delete` prints the API's response bytes (`branch
create --wait`'s ready response additionally carries a `database_url`
field) — parse that, not the human-readable text, which isn't a stable
format.

The connection string is a TLS endpoint on a stable per-branch hostname
(`br-<ref>.us.db.pgrun.dev:5432`) and already carries `sslmode=require`. Treat it
as opaque: pass it through verbatim, never rewrite the host, port or sslmode, and
never expect a raw IP address. TLS is mandatory and an unencrypted connection is
refused. `channel_binding=require` is not supported, because the gateway
terminates TLS and the branch therefore never offers channel binding; ordinary
SCRAM authentication is unaffected.

`branch url` and `branch create --wait` print `DATABASE_URL=<url>`, not a bare
URL. To capture just the value, strip the prefix or use
`branch env --format=json`.

Before running a TEST SUITE against a branch, read the "Running a test suite
against a branch" section of the pgrun-branching skill. Fixtures truncate real
data, parallel testing creates extra databases on the branch instance, and
maintain_test_schema! purges the branch outright.

A connection string is printed/exported in exactly three places:
`branch create --wait` on ready (`DATABASE_URL=...` in human mode,
`database_url` in `--json`), `branch url` (`DATABASE_URL=...`), and
`branch env` (`export DATABASE_URL="..."`, or `{"database_url":...}` with
`--format=json`). `branch exec` sets it directly in the child process's
environment without ever printing it. Nowhere else — `branch get`/`branch
list` never show a connection string in human mode, by design.

## Sources (Safe Copy setup)

Safety rules 1–7 are in the Safety contract below; 8–9 extend it.

Before an agent can branch off real production-shaped data, a human
connects the Production Database once — Connect Postgres → Protect Data →
Safe Copy. Sources *are* projects: `[<name>]` on every command below is the
Production Database name, exactly like a branch command's `[<project>]`,
and falls back to `.pgrun/project` the same way.

```sh
pgrun auth login
pgrun project use production

pgrun source add --name production --url "$DATABASE_URL"   # or the hidden prompt — see rule 8 below
pgrun source status production

pgrun source protect production                              # review only, never activates
pgrun source protect production --set public.users.email=fake --approve

pgrun source copy production --wait
pgrun branch create production --name dev --wait
```

**Exit codes.** `list`/`get`: 0 on any successful read. `status`: 1 when the
source is `failed`, `action_required`, or its last connection check failed;
0 otherwise. `add`/`update`: without `--wait`, 0 (the check runs
asynchronously); with `--wait`, 0 once connected, 1 on a failed check or on
timeout. `protect`: 1 while any column is unresolved, 1 (only the status read, no protect request)
when the source is not connected yet, and 1 if an `--approve` comes back
without an active policy; 0 once the review is complete or the policy is
activated. `pgrun source protect <n> --review` **always exits 0** — asking
pgrun for a review is not itself a failure — the one exception to "1 while
unresolved". `copy`: 1 on drift/refusal/failure (or, with `--wait`, a Safe
Copy that lands `failed`); 0 once accepted, or `ready` with `--wait`.

The connection URL must start with `postgres://` or `postgresql://` — both
schemes are accepted, and the client-side refusal of anything else never
repeats the value. Never assign a connection URL to a non-string flag
(`--wait=…`, `--json=…`, `--timeout=…`): Go's `flag` package echoes a value
it cannot parse verbatim, which would print the secret; it belongs to
`--url` on its own, or to the hidden prompt (rule 8).

**Status vocabulary** (`safe_copy_status`): `connect` (no URL yet) →
`checking` (connection check running, or holding a failed check —
`last_check_error` is set) → `protect` (schema analyzed, policy not active)
→ `ready_to_copy` (policy active, no Safe Copy yet) → `copying` → `ready`.
`action_required` means production's schema changed since the policy was
approved — review `protect` again; `failed` means the Safe Copy itself
failed to create, a support case (support@postgresrun.com). `sync` always
reads `snapshot_only`: a Safe Copy is a one-time copy in this release, not
a continuous sync.

The API-base override on **every** source subcommand — including
`add`/`update` — is `--api-url`, not `--url` (`--token` unchanged): on
`add`/`update`, `--url` is already the production connection URL.

**Safety rules for sources** — extending the safety contract below (rules
1–7, about branches) with two rules 8–9, about sources:

8. **Never paste a production connection URL into a chat or task
   transcript.** Have the human run `pgrun source add --name <n>` (or
   `pgrun source update <n>`) themselves and type the URL at the hidden
   `Connection URL (input hidden):` prompt — it must never pass through you.
9. **Never pass `--approve` on a policy you have not shown the human.**
   `pgrun source protect <n>` prints the review table without activating
   anything; show the human that table (or its `--json` equivalent) and let
   them decide. What gets copied, faked, or dropped from their production
   database is their call, never the agent's.

## MCP examples

`pgrun mcp serve` runs the same binary as an MCP server over stdio
(line-delimited JSON-RPC 2.0, no SDK). Config is env-only —
`PGRUN_API_URL`/`PGRUN_API_TOKEN` — since an MCP host launches pgrun as a
subprocess and passes credentials through its own env:

```json
{
  "mcpServers": {
    "pgrun": {
      "command": "pgrun",
      "args": ["mcp", "serve"],
      "env": { "PGRUN_API_URL": "https://<your-pgrun-host>", "PGRUN_API_TOKEN": "<TOKEN>" }
    }
  }
}
```

Four tools, one per CLI branch subcommand (no `source` tools yet — Safe
Copy setup above is CLI-only):

- `pgrun_create_branch{project,name,ttl?,parent?,wait?=true,timeout_seconds?=300}`
  — waits for ready/failed by default; the result JSON includes
  `connection_url` and a `database_url` alias once ready, so one call is
  enough — no follow-up `pgrun_get_branch` needed just to learn the URL.
- `pgrun_list_branches{project}` — never includes `connection_url`.
- `pgrun_get_branch{project,name}` — includes `connection_url` iff ready and
  credentialed.
- `pgrun_delete_branch{project,name}`

Every tool result is `content:[{type:"text",text:<api-json>}]`; a failure
(bad token, 404, 409, a create that timed out) comes back as a normal result
with `isError:true` and a sanitized message — never a crash, never a raw
stack trace, never the token.

## Safety contract — read this before scripting against a branch

1. **Branches are disposable.** They exist to be created, used hard, and
   thrown away. Don't build anything that assumes a branch survives past the
   task that created it.
2. **Never point destructive work at a production DSN.** If you're about to
   run a migration, a load test, a `TRUNCATE`, or anything else you wouldn't
   want to explain later, that command's `DATABASE_URL` should have come
   from `pgrun branch create`/`pgrun branch url` in this session — not from
   an environment variable you didn't just set, and not from a value a user
   pasted that you haven't traced back to a branch.
3. **Always set a TTL for CI/agent use** (`--ttl 1h`/`6h`/`24h`/`7d`, or the
   `ttl` MCP argument). An agent that forgets to clean up should still not
   leak a branch forever — let the TTL do it. Delete explicitly when you're
   done anyway; don't rely on the TTL as your only cleanup path.
4. **The base branch can't be deleted**, and neither can a branch with
   children — `branch delete` returns a 409 in both cases. That's not a bug
   to work around; delete children first, and never delete a project's base
   branch programmatically.
5. **A branch's connection string is a real credential.** It's disposable,
   not worthless — don't log it, don't put it somewhere a task's output gets
   posted publicly. `pgrun branch get`/`list` withhold it in human mode for
   exactly this reason; `--json`/MCP surface it because the caller asked for
   it directly.
6. **Never ask a human to paste an API token into a chat/task session.** If
   `pgrun auth status` reports not configured, tell them to run
   `pgrun auth login` themselves — it opens their browser, they click
   Authorize, and the token never appears anywhere they could paste it.
7. **Inject `DATABASE_URL` via the environment only.** Never write a
   branch's credentials into `config/database.yml`, `.env`, `.env.local`,
   or any other project file, and never commit them anywhere. `branch env`
   and `branch exec` exist specifically so a task never needs to touch
   project config to point at a branch.
