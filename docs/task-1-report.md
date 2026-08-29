# Task 1 report — CLI core

Status: DONE.

## What was built

- `go.mod` — `module github.com/pgrundev/pgrun`, `go 1.22`, zero dependencies.
- `internal/api/client.go` — `Client` (BaseURL, Token) with `CreateBranch`,
  `ListBranches`, `GetBranch`, `DeleteBranch`. Every method returns the raw
  response bytes alongside the typed result/error, so `--json` can pass the
  server's own JSON through verbatim on both success and failure. Non-2xx
  responses decode `{"error": "..."}` into `*APIError{StatusCode,Message}`;
  401 wraps that as `*AuthError` (has `Unwrap() error` back to `*APIError`)
  so the CLI can give it a distinct exit code. `joinURL` guarantees exactly
  one slash between base and path; project/branch names are
  `url.PathEscape`d into the path.
- `internal/config/config.go` — `Config{URL,Token}`, `Path()` (fixed
  `~/.config/pgrun/config.json`), `Load`/`Save` (0600, enforced via
  `os.Chmod` even when overwriting a looser-permissioned file), `Resolve`
  (file → env `PGRUN_API_URL`/`PGRUN_API_TOKEN` → flags, each layer
  overriding the last), `Fingerprint` (first 6 chars + "…", the only
  sanctioned way to print a token).
- `internal/cli/` — `Run(args, stdout, stderr) int`, dispatch for
  `branch {create,list,get,status,url,delete}` and `auth {set,status}` and
  `version`, each subcommand its own `flag.FlagSet` (`flag.ContinueOnError`,
  no `os.Exit` inside). `cmd/pgrun/main.go` is a two-line wrapper.
- Tests: `internal/api/client_test.go` (httptest, every endpoint × every
  status incl. 401/404/409/422, error-message-never-contains-token, URL
  joining, path escaping), `internal/config/config_test.go` (save/load
  round trip, 0600 enforcement, precedence, fingerprint), `internal/cli/cli_test.go`
  (full CLI surface via `cli.Run`: usage errors → 64, auth set/status,
  config precedence through the CLI, every branch subcommand against an
  httptest server, the `--wait` poll loop scripted both to ready and to
  failed, a real timeout case, connection_url never appearing in human
  output, `--json` passthrough).

## Decisions / assumptions (flagged for Task 3 to verify against the live API)

1. **`Branch` struct fields beyond `id`/`name`/`status`/`connection_url`**
   (the only ones the plan's API contract explicitly names) are guesses at
   Rails-conventional JSON keys: `base_branch`, `parent_branch_id`,
   `created_at`, `expires_at`. These only affect the human-readable
   `BASE`/`PARENT`/`CREATED`/`EXPIRES` columns (they render `-` if the real
   API uses different keys) — `--json` output is completely unaffected
   since it's the server's raw bytes, never re-marshaled. Worth a quick
   diff against the real demo API in Task 3; only `internal/api/client.go`'s
   `Branch` struct and `internal/cli/output.go` would need a field-name fix.
2. **`--json` on `branch delete`**: the plan's per-command usage bullets
   only show `--json` for `create`/`list`/`get`, but the global constraints
   section says "`--json` on every read/write command," and delete's 202
   body is legitimately interesting JSON. Added it; `branch url` still never
   has `--json` since its whole job is the one deliberate human-format
   reveal.
3. **`--url`/`--token` on every subcommand**: not shown per-command in the
   plan's usage lines but required by the global config-precedence rule
   (flags beat env beat file); implemented on all of `branch *` and
   `auth status` (not needed on `auth set`, whose own `--token`/`--url` set
   the values being saved).
4. **Positional-args-before-flags parsing**: stdlib `flag.Parse` stops at
   the first non-flag argument, which would otherwise swallow `--name` etc.
   in `pgrun branch create <project> --name x` (project comes first).
   `internal/cli/flags.go`'s `splitPositional` peels off the fixed-count
   leading positional args (project, or project+name) before handing the
   rest to `fs.Parse`, matching every usage line in the plan literally.
5. **`auth status` with no token configured exits 2** (not 0) with the
   `pgrun auth set` hint — consistent with "2 = auth/config missing"
   applying globally, and `auth set --url` only, without `--token`, is a
   usage error (--token is required) rather than silently updating just the
   URL — the URL is preserved instead by *omitting* `--url` on a later
   `auth set --token`, not by an updates-one-field-at-a-time flag.
6. **`--wait` + `--json` together**: on ready/failed, dumps the terminal
   GET's raw JSON (which naturally includes `connection_url` when ready)
   instead of printing `DATABASE_URL=...`; on timeout, dumps the last known
   raw JSON (if any) plus a sanitized stderr message, exit 1. There's no
   "raw API JSON" to show for a client-side timeout, so it's synthesized
   only as best-effort supplementary output, never in place of the exit
   code / stderr message.
7. **`pollInterval` is a package var, not a literal `2*time.Second`**, so
   `internal/cli/cli_test.go` can shrink it to 1ms for the scripted
   create→ready and create→failed sequences instead of a multi-second
   sleep-bound test. Production default is unchanged (2s, per spec). A
   manual smoke test against a real (non-test) local server confirmed the
   real 2s cadence end-to-end (~4s for a 2-poll ready sequence).

## Verification

```
$ gofmt -l .
(empty)

$ go vet ./...
(clean)

$ go build ./...
(clean)

$ go test ./... -race -count=1
ok  	github.com/pgrundev/pgrun/internal/api	1.266s
ok  	github.com/pgrundev/pgrun/internal/cli	1.495s
ok  	github.com/pgrundev/pgrun/internal/config	1.642s
```

Also manually built the binary and ran it end-to-end against both a
zero-dependency fake API server and pure local-only paths (`auth
set`/`status`, usage errors, missing-config exit 2): `--wait` printed
`DATABASE_URL=...` only on ready after the expected ~2s-cadence polling,
`branch get`/`branch list` never showed `connection_url`, `--json`
round-tripped the server's bytes verbatim (including on a 409), and the
config file landed at `0600`.

## Commits

- `3dabe36` — `feat: api client with typed errors, config resolution`
- `5014ac8` — `feat: cli dispatch for auth and branch commands`

Both on `main` in `/Users/alex/GPT/postgresrun/pgrun-cli` (fresh repo, no
prior history besides `83b0723` docs: agent-kit plan).
