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

## Auth

Get a Bearer token from the dashboard's Tokens page (account-scoped). Then
either:

```sh
pgrun auth set --token <TOKEN> --url https://<your-pgrun-host>
```

which writes `~/.config/pgrun/config.json` at mode 0600 — or set
`PGRUN_API_URL` / `PGRUN_API_TOKEN` in the environment (wins over the config
file; CLI flags `--url`/`--token` win over both). `pgrun auth status` prints
the URL and a 6-character token fingerprint — never the token itself.

## CLI examples

```sh
# create a branch and block until it's connectable (exit 0 only on ready)
pgrun branch create myproject --name agent-task-42 --ttl 1h --wait
DATABASE_URL=postgres://...   # printed to stdout on success

# same, machine-readable
pgrun branch create myproject --name agent-task-42 --ttl 1h --wait --json

# check on it later, delete when done
pgrun branch get myproject agent-task-42
pgrun branch url myproject agent-task-42   # DATABASE_URL=... or a reason, exit 1
pgrun branch delete myproject agent-task-42
```

Exit codes are a stable interface: `0` success, `1` operation/API failure
(bad request, 409 on a base/parented branch, branch failed or timed out),
`2` auth/config missing or rejected, `64` usage (bad flags/args). `--json` on
`create`/`list`/`get`/`delete` prints the API's response bytes verbatim —
parse that, not the human-readable text, which isn't a stable format.

`DATABASE_URL=...` is printed in exactly two places: `branch create --wait`
on ready, and `branch url`. Nowhere else — `branch get`/`branch list` never
show a connection string in human mode, by design.

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
  `connection_url` once ready.
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
