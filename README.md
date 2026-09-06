# pgrun

Create, use, and delete disposable [PGRun](https://pgrun.dev) Postgres
branches — from the command line or as an MCP tool. One static Go binary,
stdlib only, no external dependencies.

A branch is a real, connectable Postgres: copy-on-write off a project's base
branch, ready in about the time a normal database takes to boot, gone the
moment you delete it or its TTL expires.

> **Status:** built, not yet released. `.goreleaser.yaml`/`Makefile`/npm
> packaging are checked in but publishing is deferred — see
> [Install](#install).

## Quickstart

The agent-first path — three commands, then hand the work to Claude Code:

```sh
pgrun auth login       # interactive: prompts for URL + token, verifies, saves
pgrun skill install    # installs the pgrun-branching skill into ~/.claude/skills
claude                 # then: "Add an index to users.email and test it on a pgrun branch"
```

The skill teaches Claude when to create a branch, how to inject its
`DATABASE_URL` into only the commands that need it, and when to delete it.
Claude does the branching; you never create a branch by hand.

Prefer the CLI directly?

```sh
pgrun auth login    # interactive: prompts for URL + token, verifies, saves

pgrun branch create myproject --name scratch --ttl 1h --wait
# DATABASE_URL=postgres://... on stdout, only on ready (exit 0)

psql "$DATABASE_URL" -c 'select 1'

pgrun branch delete myproject scratch
```

Or, for the common "run one command against a fresh, disposable branch"
case — the primitive an agent reaches for most — one line:

```sh
pgrun branch exec myproject --create scratch --ttl 1h --delete-after -- \
  psql -c 'select 1'
```

Creates the branch, waits for it, runs the command with `DATABASE_URL` set
in its environment, deletes the branch afterward (even if the command
fails), and exits with the command's own exit code.

### First time? Connect your production database

Branches above fork whatever base a project already has. To branch off a
privacy-safe copy of your *real* production database instead, connect it
once — Connect Postgres → Protect Data → Safe Copy — then branch as usual:

```sh
pgrun auth login
pgrun project use production                                # alias: pgrun projects use

pgrun source add --name production --url "$DATABASE_URL"    # omit --url for a hidden prompt
pgrun source status production                               # poll until connected

pgrun source protect production                              # prints the review table, decides nothing
pgrun source protect production --set public.users.email=fake --approve

pgrun source copy production --wait                          # the one-time Safe Copy
pgrun branch create production --name dev --wait             # branch off the Safe Copy
```

`source protect` never activates anything on its own — it prints a table of
every column, resolve the ones marked `UNRESOLVED` with `--set` (add
`--acknowledge <table>.<column>` to copy a sensitive one unmasked), then
`--approve` yourself once you're satisfied. See [Sources](#sources) below
for the full command table and how secrets are handled.

## Install

```sh
curl -fsSL https://pgrun.dev/install | sh
```

The installer downloads the matching release binary from
[github.com/pgrundev/pgrun-cli](https://github.com/pgrundev/pgrun-cli) and
verifies its SHA-256. Or install from source with
`go install github.com/pgrundev/pgrun-cli/cmd/pgrun@latest`, or build directly:

```sh
git clone https://github.com/pgrundev/pgrun-cli
cd pgrun-cli && go build -o pgrun ./cmd/pgrun
```

(`brew install pgrundev/tap/pgrun` and `npx @pgrun/cli` are wired in goreleaser
but gated behind tap/npm credentials — curl|sh is the supported beta path.)

## Auth

`pgrun auth login` is the everyday path — an interactive prompt for the API
URL (pre-filled from whatever's already configured, or pgrun's built-in
default) and a token, read with terminal echo off and verified against the
API before it's saved. It also prints the dashboard's Tokens page URL, so
that's where the token itself comes from. `pgrun auth logout` clears the
saved config. `pgrun auth status` prints the URL and a 6-character token
fingerprint — never the token.

```sh
pgrun auth login
```

For scripts and CI, skip the prompt: `pgrun auth set --token <TOKEN>
[--url <URL>]` writes `~/.config/pgrun/config.json` at mode `0600` directly
(no prompt, no network call), or set `PGRUN_API_URL`/`PGRUN_API_TOKEN` in
the environment. Precedence (highest first): `--url`/`--token` flags on any
command → env → the config file.

Every `source` subcommand overrides the API base with `--api-url` instead
of `--url` — on `source add`/`source update`, `--url` is already the
production connection URL. See [Sources](#sources).

## Commands

| Command | What it does |
|---|---|
| `pgrun branch create <project> --name <n> [--ttl 1h\|6h\|24h\|7d] [--parent <name>] [--wait] [--timeout 300s] [--json]` | Create a branch. `--wait` polls every 2s until ready/failed/timeout; prints `DATABASE_URL=...` on ready (`--json` adds a `database_url` field). Without `--wait`, prints a status line and exits 0 immediately. |
| `pgrun branch list <project> [--json]` | Table: `NAME STATUS BASE PARENT VERSION CREATED EXPIRES`. Never shows a connection string. |
| `pgrun branch get <project> <name> [--json]` (alias `status`) | One branch's status. Human mode never shows `connection_url`. |
| `pgrun branch url <project> <name>` | `DATABASE_URL=...` if ready and credentialed (exit 0), else a sanitized reason (exit 1). |
| `pgrun branch env <project> <name> [--format=json]` | `export DATABASE_URL="..."` (eval-able), or `{"database_url":"..."}` with `--format=json`. Same readiness rule as `branch url`. |
| `pgrun branch exec <project> <name> -- <command...>` | Run `<command>` with `DATABASE_URL` set in its environment (never on argv, never in a file); exits with the command's own exit code. |
| `pgrun branch exec <project> --create <newname> [--ttl ...] [--from <parent>] [--delete-after] [--timeout 300s] -- <command...>` | Create a fresh branch, wait for it, run the command, and (`--delete-after`) delete it afterward — even if the command fails. |
| `pgrun branch delete <project> <name> [--json]` | `202` → deleting (exit 0); `409` on the base branch or a branch with children (exit 1). |
| `pgrun auth login` | Interactive: prompt for URL + token, verify, save. |
| `pgrun auth logout` | Remove the saved config. |
| `pgrun auth set --token <t> [--url <u>]` | Advanced/CI: write the config file directly, no prompt. |
| `pgrun auth status` | Print URL + token fingerprint. |
| `pgrun skill install [--project \| --dir <dir>]` | Install the embedded `pgrun-branching` skill for Claude Code (`~/.claude/skills/pgrun-branching/SKILL.md` by default; `--project` → `./.claude/skills`; `--dir` → any other skills root). Idempotent; overwrites a stale copy. |
| `pgrun skill status [--project \| --dir <dir>]` | Exit 0 only if the skill is installed and matches this binary's copy. |
| `pgrun skill show` | Print the embedded skill to stdout. |
| `pgrun mcp serve` | Run as an MCP server over stdio (see below). |
| `pgrun version` | Print the version. |

`--json` on `create`/`list`/`get`/`delete` prints the API's response bytes
**verbatim** — that's the machine-readable contract, not the human text.
(`branch create --wait --json`'s ready response is the one exception: it
adds `database_url` and `"ready": true` on top of the raw body, so an agent
scripting against `--json` never needs a second call just to learn the
connection string:

```json
{"id":"branch_123","name":"agent-add-users-index","status":"ready","database_url":"postgres://…","ready":true, …}
```
)

### Exit codes

`0` success (or, for `branch exec`, the exec'd command's own exit code) ·
`1` operation/API failure · `2` auth/config missing or rejected (with a
`pgrun auth login` hint) · `64` usage (bad flags/args).

## Sources

Connect a real production database, resolve its data-protection policy,
then create a one-time privacy-safe copy to branch from — Connect Postgres
→ Protect Data → Safe Copy:

| Command | What it does |
|---|---|
| `pgrun source list [--json]` | Table: `NAME STATUS PROTECTION SAFE COPY BRANCHES`. Exit 0 on any successful read. |
| `pgrun source get [<name>] [--json]` | One `key=value` summary line plus the next command to run. Exit 0 on any successful read. |
| `pgrun source status [<name>] [--json]` | The connect → schema → protect → Safe Copy checklist, plus `Next:`. Exits 1 when the source is `failed`, `action_required`, or its last connection check failed; 0 otherwise. |
| `pgrun source add --name <n> [--url <postgres://…>] [--wait] [--timeout 120s] [--json]` | Connects a new Production Database. Omit `--url` for a hidden prompt (`Connection URL (input hidden): `, echo off on a terminal); `--json` requires `--url` — a prompt would corrupt the JSON stream. `--wait` polls the connection check to settled (exit 0 once connected, 1 on a failed check or timeout). |
| `pgrun source update [<name>] [--url <postgres://…>] [--wait] [--timeout 120s] [--json]` | Replaces the connection URL — accepted only while the source has none yet or its last check failed (`409` otherwise). Same secret handling and exit codes as `add`. |
| `pgrun source protect [<name>] [--set <table>.<column>=copy\|fake\|null\|remove ...] [--acknowledge <table>.<column> ...] [--approve] [--review] [--json]` | Reviews the data-protection policy — prints the table, activates nothing by itself. `--approve` activates it and is refused (422) while any column is unresolved; `--review` asks pgrun to flag the unresolved columns. A repeated `--set` key is a usage error; there is no "copy everything" shortcut. Exit 1 while columns are unresolved, 0 once the review is complete or the policy is activated. |
| `pgrun source copy [<name>] [--wait] [--timeout 30m] [--json]` | Creates the Safe Copy. Refuses (exit 1, no request sent) if production's schema changed since the policy was approved, a Safe Copy already exists, the last one failed, or protection isn't active yet. `--wait` polls to `ready` (exit 0) or `failed` (exit 1). |

`[<name>]` is the Production Database (= project) name — sources *are*
projects — and falls back to `.pgrun/project` exactly like a branch
command's `[<project>]`; set it once with `pgrun project use <slug>`
(alias: `pgrun projects use`).

**Secrets.** On `add`/`update`, `--url` *is* the production connection URL:
never printed, never included in `--json`, never repeated in an error, sent
only in the request body's `connection_url` field. Prefer the hidden
prompt (`pgrun source add --name production`, no `--url`) over typing the
URL on a shared shell — a flag's value is visible to other local processes
and lands in shell history, the prompt is not. Never assign the URL to a
boolean flag (e.g. `--wait=postgres://…`) — Go's `flag` package echoes an
invalid boolean value verbatim, which would print the secret; always pass
it to `--url` on its own.

Because `--url` is taken as the connection URL on `add`/`update`, the
API-base override on **every** source subcommand (including those two) is
`--api-url`, not `--url` (`--token` is unchanged). Every other command
family — `branch`, `project`, `auth` — keeps `--url` as the API base.

A Safe Copy is a one-time copy in this release — refresh/continuous sync
(Phase 6) does not exist yet; `sync` always reads `snapshot_only`.

## MCP — use pgrun as an agent tool

`pgrun mcp serve` speaks the [Model Context Protocol](https://modelcontextprotocol.io)
over stdio (hand-rolled JSON-RPC 2.0, line-delimited, no SDK) so an agent can
create/list/get/delete branches as tools instead of shelling out. Config is
**env-only** — an MCP host launches `pgrun` as a subprocess and passes
credentials through its own env, not flags or the config file:

```json
{
  "mcpServers": {
    "pgrun": {
      "command": "pgrun",
      "args": ["mcp", "serve"],
      "env": {
        "PGRUN_API_URL": "https://<your-pgrun-host>",
        "PGRUN_API_TOKEN": "<TOKEN>"
      }
    }
  }
}
```

(Drop that under `mcpServers` in Claude Desktop's config, or in a project's
`.mcp.json` for Claude Code.)

Tools, one per CLI branch subcommand (no `source` tools yet — connect,
protect, and copy a Production Database from the CLI):

- `pgrun_create_branch{project,name,ttl?,parent?,wait?=true,timeout_seconds?=300}`
  — waits for ready/failed by default; result JSON includes
  `connection_url`, a `database_url` alias, and `"ready": true` once ready,
  so one call is enough to start using the branch.
- `pgrun_list_branches{project}`
- `pgrun_get_branch{project,name}`
- `pgrun_delete_branch{project,name}`

Results are `content:[{type:"text",text:<api-json>}]`; failures come back as
`isError:true` with a sanitized message — never a crash, never the token.

**Pair it with the skill** — MCP/the CLI give an agent the *tools*, the
[`pgrun-branching` skill](skills/pgrun-branching/SKILL.md) gives it the
*playbook* (when to branch vs. use local Postgres, poll semantics, cleanup
rules, what each error means). `pgrun skill install` drops it into
`~/.claude/skills` — the same file is embedded in the binary, so the
installed copy always matches the commands this version understands. See [`AGENTS.md`](AGENTS.md) for the full
agent-facing contract, including the safety rules around production DSNs.

## Development

Stdlib only — no external dependencies, offline-buildable.

```sh
make build   # bin/pgrun
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt -w
```

Go 1.22+. `go test ./...`, `go vet ./...`, and `gofmt -l .` are expected to
stay clean at all times.

## License

MIT — see [LICENSE](LICENSE).
