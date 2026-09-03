# pgrun — for coding agents

`pgrun` creates, uses, and deletes disposable PGRun Postgres branches from
the command line or over MCP. A branch is a real, connectable Postgres —
copy-on-write off a project's base branch, ready in roughly the time it
takes a normal database to boot, gone the moment you delete it or its TTL
expires. It exists so an agent doing destructive database work (migrations,
load tests, "try this and see what breaks") never has to touch anything that
matters.

One static Go binary, one API: `/api/v1/projects/:project/branches`,
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
pgrun auth login          # interactive: prompts for URL + token, verifies, saves
claude                    # or any MCP host — pick up the pgrun-branching skill/tools from here
```

## Auth

**`pgrun auth login`** is the everyday path: an interactive prompt for the
API URL (defaulting to whatever's already configured, or pgrun's built-in
default) and a token, read with terminal echo off, verified against the API
before anything is saved, and confirmed by printing the token's fingerprint
only. Get the token itself from the dashboard's Tokens page — `auth login`
prints that URL as part of the prompt.

```sh
pgrun auth login
```

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

A connection string is printed/exported in exactly three places:
`branch create --wait` on ready (`DATABASE_URL=...` in human mode,
`database_url` in `--json`), `branch url` (`DATABASE_URL=...`), and
`branch env` (`export DATABASE_URL="..."`, or `{"database_url":...}` with
`--format=json`). `branch exec` sets it directly in the child process's
environment without ever printing it. Nowhere else — `branch get`/`branch
list` never show a connection string in human mode, by design.

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

Four tools, one per CLI branch subcommand:

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
   `pgrun auth login` themselves — it reads the token with terminal echo
   off and verifies it before saving, so it never has to pass through you
   or end up in a transcript.
7. **Inject `DATABASE_URL` via the environment only.** Never write a
   branch's credentials into `config/database.yml`, `.env`, `.env.local`,
   or any other project file, and never commit them anywhere. `branch env`
   and `branch exec` exist specifically so a task never needs to touch
   project config to point at a branch.
