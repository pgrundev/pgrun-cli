# pgrun source CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pgrun source list|get|status|add|update|protect|copy` — the customer completes Connect Postgres → Protect Data → Create Safe Copy from the terminal, over the deployed `/api/v1/sources` API, without ever seeing or leaking a production connection URL.

**Architecture:** Same shape as the branch commands: an `internal/api` client method per endpoint returning `(raw []byte, typed, error)`; one `internal/cli/source*.go` file per concern; `--json` prints the API bytes verbatim; human output derives every line from the source object's fields (`safe_copy_status`, `step`, `protection`, `unresolved_count`, `last_check_error`, `schema_changed`) — never a client-side state machine. A source IS a project (the API's `/projects` rows are the same `SourceDatabase` records), so `[<name>]` on every source command falls back to `.pgrun/project` exactly like `[<project>]` on branch commands. The connection URL is a secret: it travels only in a JSON request body under `connection_url`, is read hidden when prompted, and every byte the add/update commands write passes through a redactor that scrubs the URL and its password.

**Tech Stack:** Go 1.22, stdlib only (no external deps — `go build`, `go vet`, `gofmt -l` and `go test ./...` must stay clean). httptest for every test. No MCP changes in this slice.

**Spec:** `docs/specs/pgrun-source-cli.md` (operator brief, verbatim). Backend authority: the Rails app's `app/controllers/api/v1/sources_controller.rb` at postgresrun_app main (the `approve`/`rules`/`schema_changes` fields were added on 2026-09-06 for this slice; the contract below is copied from that controller).

## Global Constraints

- **Never print, log, or echo the connection URL** — not on stdout, stderr, in `--json`, in usage errors, in API-error messages, in timeouts. The only place it goes is the JSON request body key `connection_url` (never a query string, never a path segment, never a header).
- **Redaction is structural:** `source add`/`source update` wrap stdout and stderr in the redactor from Task 3 before anything is written, so even a hostile/misconfigured server echoing the URL back cannot leak it.
- **Fail closed, server-authoritative:** the CLI never bypasses a backend validation; every pre-flight message it prints is derived from the source object, and the POST still happens where the API is the authority (protect, copy). No `copy everything` option, no automatic approval, `--approve` is the only way a policy version is minted.
- **Exit codes are the existing public contract:** `0` success · `1` operation/API failure (incl. connection failed, unresolved policy, schema drift, Safe Copy failed) · `2` auth/config missing or rejected · `64` usage.
- **`--json` = the API's response bytes verbatim** (same as the branch commands), on success and, when a body was received, on API errors (`handleAPIError` already does this).
- **Customer vocabulary only** in human output and docs: Production Database / source, Protect Data / data protection, Safe Copy, Branch. Never golden, BranchSource, SourceDatabase, masking policy.
- **Do not touch** `branch`, `project`, `auth`, `skill`, `mcp` behaviour; add to `usage` in `cli.go` only.
- No Co-Authored-By trailers. Commit after each task.

## API contract (deployed; copied from the Rails controller)

All under `Authorization: Bearer <token>`, JSON in and out, 401 `{"error"}` on a bad token, 404 `{"error":"not_found"}` when branching is not enabled for the account, 404 `{"error":…}` for an unknown source name.

| Call | Success | Errors |
|---|---|---|
| `GET /api/v1/sources` | 200 `{"sources":[<source>…]}` | |
| `GET /api/v1/sources/<name>` | 200 `<source>` | 404 |
| `POST /api/v1/sources` body `{"name":"<slug>","connection_url":"postgres://…"}` | 201 `<source>` (`safe_copy_status:"checking"`) | 422 `{"error"}` (bad URL, taken name) |
| `PATCH /api/v1/sources/<name>` body `{"connection_url":"…"}` | 200 `<source>` (`checking`) | 422 bad URL · 409 `{"error":"this database is already connected"}` (only `connect`/`checking` sources accept a URL) |
| `POST /api/v1/sources/<name>/protect` body `{"decisions":{"<table>.<column>":"copy"\|"fake"\|"null"},"acknowledge":["<table>.<column>"…],"approve":true\|false}` (all optional) | 200 `<protect>` | 422 `{"error"}` (invalid disposition for the column type, sensitive Copy without its key in `acknowledge`, policy incomplete) · 422 `<protect> + "error":"N columns still need a decision before the policy can be approved"` when `approve` is true with unresolved columns |
| `POST /api/v1/sources/<name>/review` | 200 `{"name","unresolved":["<t>.<c>"…],"requested":bool}` | |
| `POST /api/v1/sources/<name>/copy` | 202 `<source>` (`copying`) | 409 `{"error":"this database already has a Safe Copy"}` (ready/copying) · 422 `{"error":"production schema changed — review protect before creating or re-creating the Safe Copy"}` (action_required) · 422 `{"error":"finish protect first — …"}` (connect/checking/protect) · 422 other import refusals |

`<source>`:

```json
{"name":"production","safe_copy_status":"ready","step":4,"postgres_version":"17","provider":"rds",
 "size_bytes":123456,"tables":42,"sensitive_columns":7,"unresolved_count":0,
 "protection":"active","policy_version":2,"sync":"snapshot_only",
 "safe_copy_updated_at":"2026-09-06T03:40:00Z","branches":1,"last_check_error":null,
 "schema_changed":false,"schema_changes":null}
```

- `safe_copy_status` ∈ `connect` (no URL yet) · `checking` (check queued/running, OR the last check failed — then `last_check_error` is an exception class name such as `PG::ConnectionBad`) · `protect` (schema analyzed, policy not active) · `ready_to_copy` (policy active, no Safe Copy) · `copying` · `ready` · `action_required` (production schema changed since the approved policy — `step` 2 when there is no Safe Copy yet, 4 when a Safe Copy exists) · `failed` (Safe Copy creation failed — support case).
- `step` 1..4 = the wizard step the source is on. `protection` ∈ `none|draft|active`. `policy_version` is null until approved. `schema_changes` = `{"added":[…],"removed":[…],"retyped":[…]}` of `"<table>.<column>"` names, or null.

`<protect>`:

```json
{"name":"production","safe_copy_status":"protect","policy_version":null,"activated":false,
 "unresolved_count":1,
 "unresolved":[{"column":"public.events.metadata","sensitive":false,"valid":["copy","null"]}],
 "rules":[{"column":"public.users.email","disposition":"fake","recommended":true,"sensitive":true},
          {"column":"public.users.id","disposition":"copy","recommended":true,"sensitive":false}],
 "table_rules":[{"table":"public.solid_queue_jobs","disposition":"schema_only"}]}
```

`disposition` ∈ `copy|fake|null` (column) and `copy_data|schema_only|exclude` (table). `valid` lists which of `copy|fake|null` the column's type allows.

## Execution rulings (controller, 2026-09-06)

1. **Sources are projects.** `[<name>]` on `get/status/update/protect/copy` is the source name = project slug, with the `.pgrun/project` fallback via the existing `resolveProject`. `source add --name` creates one; no new selection behaviour. Cost if wrong: none (the server's `/projects` and `/sources` rows are the same records).
2. **`--wait` is opt-in** on `add`, `update`, `copy` (matches `branch create`); defaults `--timeout 120s` for a connection check, `30m` for a Safe Copy. Without `--wait` the command prints one line and the next command, exit 0.
3. **Approval is explicit.** `pgrun source protect <name>` reviews (applies recommended rules + decisions, prints the table, never activates); `--approve` activates and is refused server-side (422) while anything is unresolved. Cost if wrong: one extra command in the flow.
4. **Exit codes for reads:** `list`/`get` exit 0 on any successful read; `status` exits 1 when the source is `failed`, `action_required`, or its last connection check failed (actionable), 0 otherwise. `protect` exits 1 while columns are unresolved, 0 when the review is complete or the policy was activated. `copy` exits 1 on drift/refusal/failure, 0 on accepted (or ready with `--wait`).
5. **`--set` accepts `copy|fake|null|remove`** (`remove` is an alias the CLI maps to `null` — the UI's word) and prints dispositions as `COPY` / `FAKE` / `REMOVE`.
6. **Hidden prompt when `--url` is omitted** (`readToken`'s echo-off convention), refused with a usage error when combined with `--json` (a prompt would corrupt the JSON stream). The URL must start with `postgres://` or `postgresql://` — checked client-side, and the usage error never repeats the value.
7. **No MCP tools for sources in this slice** (deferred; the MCP server is unchanged). No release tag (operator-gated).
8. **`--url` collision (found in Task 3):** on `add`/`update` `--url` is the connection URL and the API-base override is `--api-url`; a URL-shaped positional/`--name` is refused with a value-free usage error. Cost if wrong: a script passing an `https://` API URL as `--url` fails the scheme check (safe).

---

### Task 1: API client for sources

**Files:**
- Create: `internal/api/sources.go`
- Test: `internal/api/sources_test.go`

**Interfaces (produces — every later task consumes these exactly):**

```go
package api

// Source mirrors one source object (see the API contract). Only used for
// human formatting; --json always prints the raw bytes.
type Source struct {
	Name              string         `json:"name"`
	SafeCopyStatus    string         `json:"safe_copy_status"`
	Step              int            `json:"step"`
	PostgresVersion   string         `json:"postgres_version"`
	Provider          string         `json:"provider"`
	SizeBytes         int64          `json:"size_bytes"`
	Tables            int            `json:"tables"`
	SensitiveColumns  int            `json:"sensitive_columns"`
	UnresolvedCount   int            `json:"unresolved_count"`
	Protection        string         `json:"protection"`
	PolicyVersion     int            `json:"policy_version"` // null decodes to 0
	Sync              string         `json:"sync"`
	SafeCopyUpdatedAt string         `json:"safe_copy_updated_at"`
	Branches          int            `json:"branches"`
	LastCheckError    string         `json:"last_check_error"` // exception class name, "" when null
	SchemaChanged     bool           `json:"schema_changed"`
	SchemaChanges     *SchemaChanges `json:"schema_changes"`
}

type SchemaChanges struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Retyped []string `json:"retyped"`
}

// Safe Copy statuses (safe_copy_status), verbatim from the API.
const (
	SourceConnect        = "connect"
	SourceChecking       = "checking"
	SourceProtect        = "protect"
	SourceReadyToCopy    = "ready_to_copy"
	SourceCopying        = "copying"
	SourceReady          = "ready"
	SourceActionRequired = "action_required"
	SourceFailed         = "failed"
)

// CheckFailed: the last connection check failed (the API keeps such a source
// in "checking" so a new URL can be supplied; last_check_error carries the
// exception class). CheckSettled: the check has produced an answer either
// way. CopySettled: a Safe Copy creation is no longer in progress.
func CheckFailed(s Source) bool  { return s.SafeCopyStatus == SourceChecking && s.LastCheckError != "" }
func CheckSettled(s Source) bool { return s.SafeCopyStatus != SourceChecking || s.LastCheckError != "" }
func CopySettled(s Source) bool  { return s.SafeCopyStatus != SourceCopying }

type ProtectRequest struct {
	Decisions   map[string]string `json:"decisions,omitempty"`
	Acknowledge []string          `json:"acknowledge,omitempty"`
	Approve     bool              `json:"approve,omitempty"`
}

type UnresolvedColumn struct {
	Column    string   `json:"column"`
	Sensitive bool     `json:"sensitive"`
	Valid     []string `json:"valid"`
}

type ColumnRule struct {
	Column      string `json:"column"`
	Disposition string `json:"disposition"` // copy | fake | null
	Recommended bool   `json:"recommended"`
	Sensitive   bool   `json:"sensitive"`
}

type TableRule struct {
	Table       string `json:"table"`
	Disposition string `json:"disposition"` // copy_data | schema_only | exclude
}

type ProtectResult struct {
	Name            string             `json:"name"`
	SafeCopyStatus  string             `json:"safe_copy_status"`
	PolicyVersion   int                `json:"policy_version"`
	Activated       bool               `json:"activated"`
	UnresolvedCount int                `json:"unresolved_count"`
	Unresolved      []UnresolvedColumn `json:"unresolved"`
	Rules           []ColumnRule       `json:"rules"`
	TableRules      []TableRule        `json:"table_rules"`
}

type ReviewResult struct {
	Name       string   `json:"name"`
	Unresolved []string `json:"unresolved"`
	Requested  bool     `json:"requested"`
}

func (c *Client) ListSources(ctx context.Context) (raw []byte, sources []Source, err error)
func (c *Client) GetSource(ctx context.Context, name string) (raw []byte, src Source, err error)
func (c *Client) CreateSource(ctx context.Context, name, connectionURL string) (raw []byte, src Source, err error)    // POST, want 201
func (c *Client) UpdateSourceURL(ctx context.Context, name, connectionURL string) (raw []byte, src Source, err error) // PATCH, want 200
func (c *Client) ProtectSource(ctx context.Context, name string, req ProtectRequest) (raw []byte, res ProtectResult, err error) // POST, want 200
func (c *Client) ReviewSource(ctx context.Context, name string) (raw []byte, res ReviewResult, err error)          // POST, want 200
func (c *Client) CopySource(ctx context.Context, name string) (raw []byte, src Source, err error)                  // POST, want 202
// WaitForSource polls GetSource every PollInterval until done(src) is true,
// ctx is done, or a request errors — always returning the latest raw body,
// exactly like WaitForTerminal.
func (c *Client) WaitForSource(ctx context.Context, name string, raw []byte, src Source, done func(Source) bool) ([]byte, Source, error)
```

Request bodies are private structs: `createSourceRequest{Name string `json:"name"`; ConnectionURL string `json:"connection_url"`}` and `updateSourceRequest{ConnectionURL string `json:"connection_url"`}`. Paths: `sourcesPath() = "/api/v1/sources"`, `sourcePath(name) = "/api/v1/sources/" + url.PathEscape(name)`, `sourceActionPath(name, action)`. Every method validates `name` with the existing `validatePathSegment("name", …)` (CreateSource validates too — the slug goes in the body but must not be empty/dot). Reuse `c.do`. `ListSources` decodes the `{"sources":[…]}` envelope.

- [ ] **Step 1: Failing tests** — `internal/api/sources_test.go`, one httptest server routed on `r.Method + " " + r.URL.Path`, asserting for each call: method, path, `Authorization: Bearer`, `Content-Type: application/json` on bodies, the decoded request body (`connection_url` key present, `url` key absent, **no query string** — `r.URL.RawQuery == ""`), the decoded typed result, and the error mapping (`*APIError` with status 409/422, `*AuthError` on 401, `*InvalidNameError` for `".."`). Add a `WaitForSource` test with `PollInterval` shrunk (copy `withFastPoll`'s approach from `wait_test.go`) driving `checking → checking(last_check_error:"PG::ConnectionBad")` and asserting `done = CheckSettled` stops on the second poll; and a ctx-deadline case. Plus table tests for `CheckFailed`/`CheckSettled`/`CopySettled`.
- [ ] **Step 2: Run** `go test ./internal/api/` — expect compile failures.
- [ ] **Step 3: Implement** `internal/api/sources.go` per the interface block. Package doc comment of `client.go` says "branches API" — leave it; add a file comment in `sources.go` describing the sources API.
- [ ] **Step 4: Run** `go test ./internal/api/ && go vet ./... && gofmt -l internal` — all green, gofmt prints nothing.
- [ ] **Step 5: Commit** `feat(api): sources client — list/get/create/update/protect/review/copy + WaitForSource`

---

### Task 2: `source list`, `source get`, `source status` — dispatch, table, checklist, next step

**Files:**
- Create: `internal/cli/source.go` (dispatch + list/get/status), `internal/cli/source_output.go` (all derivation + rendering)
- Modify: `internal/cli/cli.go` (`case "source", "sources": return runSource(args[1:], os.Stdin, stdout, stderr)`; usage lines below)
- Test: `internal/cli/source_test.go`

**Interfaces:**
- Consumes Task 1.
- Produces (Tasks 3–5 reuse these): `runSource(args []string, stdin io.Reader, stdout, stderr io.Writer) int`; `sourceNameArgs(cmd string, args []string, stderr io.Writer) (name string, rest []string, code int, ok bool)` — peels `[<name>]` with `leadingPositionals`, at most one positional, falls back through `resolveProject`; `writeSourceTable(w, []api.Source)`; `sourceLine(api.Source) string`; `sourceStatusView(s api.Source) string`; `sourceNext(s api.Source) string`; `sourceActionable(s api.Source) bool`; `safeCopyCell(s api.Source) string`; `dispositionLabel(d string) string` (`copy→COPY`, `fake→FAKE`, `null→REMOVE`, `copy_data→COPY`, `schema_only→SCHEMA ONLY`, `exclude→EXCLUDE`, else upper-cased input).

Usage lines to add to `usage` in `cli.go` (after the branch block):

```text
  pgrun source list [--json]
  pgrun source get [<name>] [--json]
  pgrun source status [<name>] [--json]
  pgrun source add --name <n> [--url <postgres://…>] [--wait] [--timeout 120s] [--json]   (no --url: hidden prompt)
  pgrun source update [<name>] [--url <postgres://…>] [--wait] [--timeout 120s] [--json]
  pgrun source protect [<name>] [--set <table>.<column>=copy|fake|null ...] [--acknowledge <table>.<column> ...] [--approve] [--review] [--json]
  pgrun source copy [<name>] [--wait] [--timeout 30m] [--json]
```

and the sentence: `A source command's [<name>] is the Production Database (= project) name; it falls back to .pgrun/project like branch commands.`

**Rendering rules (the whole UX — implement exactly):**

`writeSourceTable`: tabwriter, header `NAME\tSTATUS\tPROTECTION\tSAFE COPY\tBRANCHES`; STATUS = `safe_copy_status` verbatim; PROTECTION = `protection` verbatim (+ ` v<N>` when active and `PolicyVersion > 0`); SAFE COPY = `safeCopyCell`: `ready`, `copying`, `failed`, `action required` (for `action_required`), else `-`; BRANCHES = the number.

`sourceLine`: `name=%s status=%s step=%d protection=%s policy=%s safe_copy=%s unresolved=%d branches=%d check_error=%s` (policy `v<N>` or `-`, check_error `dash(...)`), followed on the next line by `next: <sourceNext>`.

`sourceStatusView` — a block, exactly:

```text
Production Database: <name>

<connection line>
<schema line>
<protection line>
<safe copy line>

Next:
  <sourceNext>
```

Lines (glyphs `✓` done, `→` in progress, `!` needs attention, `○` not yet, `✗` failed):
- connection: `connect` → `○ Not connected — pgrun needs a PostgreSQL connection URL`; `checking` && `LastCheckError==""` → `→ Checking connection (read-only, nothing is copied)`; `CheckFailed` → `✗ Connection failed (<LastCheckError>)`; else → `✓ Connected` + `, Postgres <PostgresVersion>` when non-empty.
- schema: `Step >= 2` → `✓ Schema analyzed (<Tables> tables, <SensitiveColumns> potentially sensitive columns)`; else `○ Schema not analyzed yet`.
- protection: `active` && `!SchemaChanged` → `✓ Data protection active (policy v<N>)`; `active` && `SchemaChanged` → `! Data protection policy v<N> was approved against an older schema`; `draft` && `UnresolvedCount>0` → `! Data protection needs review (<UnresolvedCount> columns need a decision)` (singular `column needs`); `draft` && 0 → `! Data protection reviewed but not approved`; `none` → `○ Data protection not started`.
- safe copy: `ready` → `✓ Safe Copy ready`; `copying` → `→ Creating Safe Copy`; `failed` → `✗ Safe Copy failed`; `action_required` && `Step==4` → `! Safe Copy ready, but production schema changed since the policy was approved`; `action_required` (other) → `○ Safe Copy not created — production schema changed, review protection first`; else `○ Safe Copy not created`.

`sourceNext` (commands only, no prose):
- `connect`, or `CheckFailed` → `pgrun source update <name> --url "$DATABASE_URL"`
- `checking` (in progress) → `pgrun source status <name>`
- `protect` → `pgrun source protect <name>`
- `ready_to_copy` → `pgrun source copy <name> --wait`
- `copying` → `pgrun source status <name>`
- `ready` → `pgrun branch create <name> --name dev --wait`
- `action_required` → `pgrun source protect <name>`
- `failed` → `write to support@postgresrun.com (a failed Safe Copy is a support case)`

`sourceActionable(s)`: `failed`, `action_required`, or `CheckFailed`.

Commands: `list` (`--json`, table); `get [<name>]` (`--json`, `sourceLine`); `status [<name>]` (`--json` raw; else `sourceStatusView`; exit 1 when `sourceActionable`, else 0). All three: `resolveOrHint`, `api.New`, `handleAPIError` on error. `list` with zero sources prints `no production databases yet — run \`pgrun source add --name <n> --url "$DATABASE_URL"\`` to stderr, exit 0.

- [ ] **Step 1: Failing tests** in `source_test.go` (reuse `withServer`, `run`, `chdir`, `config.SaveProject`): list table (header + rows + `action required` cell + `--json` verbatim + empty list message); get line + `next:`; get via `.pgrun/project` fallback; get with no name and no project → exit 64; status view for each of the eight statuses via a table test asserting the four glyph lines and the Next command (use fixtures as JSON literals); `status` exit codes (ready→0, failed→1, action_required→1, checking+error→1); 401 → exit 2; 404 → exit 1 with the API message; and for every status fixture, the human output contains no `postgres://` (belt and braces — the source object never carries a URL, and neither may any rendering).
- [ ] **Step 2: Run** `go test ./internal/cli/ -run 'Source'` — compile failures.
- [ ] **Step 3: Implement** `source_output.go` and `source.go` (list/get/status only; `add/update/protect/copy` return `usageErrf(stderr, "source %s: not implemented yet", …)` placeholders that Tasks 3–5 replace). Wire `cli.go`.
- [ ] **Step 4: Run** `go test ./... && go vet ./... && gofmt -l cmd internal` — green.
- [ ] **Step 5: Commit** `feat(cli): pgrun source list/get/status — Production Database table, checklist view, next step`

---

### Task 3: `source add` and `source update` — secret handling, hidden prompt, wait

**Files:**
- Create: `internal/cli/source_connect.go`, `internal/cli/redact.go`
- Test: `internal/cli/source_connect_test.go`, `internal/cli/redact_test.go`

**Interfaces:**
- Consumes Task 1 (`CreateSource`, `UpdateSourceURL`, `WaitForSource`, `CheckSettled`, `CheckFailed`) and Task 2 (`sourceNameArgs`, `sourceStatusView`, `sourceNext`).
- Produces: `newRedactor(secretURL string) *strings.Replacer` and `redactWriter(w io.Writer, r *strings.Replacer) io.Writer`; `readConnectionURL(stdin io.Reader, stdout, stderr io.Writer) (string, error)`; `validConnectionURL(s string) bool`.

`redact.go`:

```go
// newRedactor scrubs a connection URL from anything the CLI prints. It
// replaces the full URL, the URL with its password stripped, the raw
// password, and the password's percent-encoded form with "[redacted]", so
// even an API error that echoes the request back cannot leak the secret.
func newRedactor(secretURL string) *strings.Replacer {
	pairs := []string{}
	add := func(s string) {
		if s != "" {
			pairs = append(pairs, s, "[redacted]")
		}
	}
	add(secretURL)
	if u, err := url.Parse(secretURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			add(pw)
			add(url.QueryEscape(pw))
			add(url.PathEscape(pw))
		}
		stripped := *u
		stripped.User = url.User(u.User.Username())
		add(stripped.String())
	}
	return strings.NewReplacer(pairs...)
}

type redactingWriter struct {
	w io.Writer
	r *strings.Replacer
}

func (rw redactingWriter) Write(p []byte) (int, error) {
	if _, err := rw.r.WriteString(rw.w, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func redactWriter(w io.Writer, r *strings.Replacer) io.Writer { return redactingWriter{w: w, r: r} }
```

(Order matters for `strings.NewReplacer`: longest first — add the full URL before the password, which the code above does. A password shorter than 4 characters is still replaced; that is acceptable.)

`readConnectionURL`: prints `Connection URL (input hidden): ` via the existing `readToken(bufio.NewReader(stdin), stdin, stdout, stderr, prompt)` (it already disables echo only when `stdin == os.Stdin`); returns the trimmed line. `validConnectionURL`: `strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")`.

`source add` flow (`sourceAdd(args, stdin, stdout, stderr)`): flags `--name` (required), `--url`, `--wait`, `--timeout 120s`, `--json`, auth flags; no positionals. If `--url` is empty: `--json` → usage error `source add: --url is required with --json (the URL prompt would corrupt the JSON output)`; else prompt. Empty after prompt → usage error `a connection URL is required`. Not `validConnectionURL` → usage error `source add: the connection URL must start with postgres:// or postgresql://` (never the value). Then — before any API call — `stdout = redactWriter(stdout, red)`, `stderr = redactWriter(stderr, red)` with `red := newRedactor(url)`; every later write goes through them. `CreateSource` → on error `handleAPIError`. Without `--wait`: `--json` → dump raw; else print `✓ Production database <name> added` / `→ Checking connection (read-only, nothing is copied)` / `` / `Next:` / `  pgrun source status <name>`; exit 0. With `--wait`: `WaitForSource(ctx, name, raw, src, api.CheckSettled)`; deadline → (`--json` dumps raw) stderr `pgrun: still checking <name> after <timeout> — run \`pgrun source status <name>\``, exit 1; `CheckFailed` → (`--json` dumps raw) stderr:

```text
pgrun: connection failed (<LastCheckError>).

Update the connection URL and try again:
  pgrun source update <name> --url "$DATABASE_URL"
```

exit 1; settled and connected → `--json` dumps raw, else prints `✓ Production database <name> added` then a blank line then `sourceStatusView(src)`; exit 0.

`source update [<name>]`: same flags minus `--name`; name via `sourceNameArgs`; `UpdateSourceURL`; identical redaction/prompt/wait logic; the no-wait line is `✓ Connection URL updated for <name>`.

- [ ] **Step 1: Failing tests** — `redact_test.go`: full URL, password, percent-encoded password (`p@ss w%rd` style), stripped URL all become `[redacted]`; a URL without a password; an unparsable secret still redacts the literal. `source_connect_test.go` (server captures the request body): add without wait (201, body has `connection_url`, no query, stdout has the two lines and never the URL/password); add `--json` verbatim; add with hidden prompt (call `sourceAdd` directly with `strings.NewReader(url+"\n")`, assert stdout contains `Connection URL (input hidden):` and not the URL); add `--json` without `--url` → 64; bad scheme → 64 and stderr lacks the value; missing `--name` → 64; add `--wait` sequence checking→protect → exit 0 and the status view printed; add `--wait` sequence checking→checking+`PG::ConnectionBad` → exit 1, stderr has `connection failed (PG::ConnectionBad)` and the `source update` hint; add `--wait` timeout → exit 1 + `still checking`; **hostile server**: 422 body `{"error":"rejected postgres://u:SECRETPW@h/db"}` → human stderr and `--json` stdout both contain `[redacted]` and never `SECRETPW`; update happy path (PATCH, body key) and 409 → exit 1 with the API message; update via `.pgrun/project` fallback; 401 → exit 2.
- [ ] **Step 2: Run** `go test ./internal/cli/ -run 'Redact|SourceAdd|SourceUpdate'` — failures.
- [ ] **Step 3: Implement** and replace the Task 2 placeholders for `add`/`update` in `runSource`.
- [ ] **Step 4: Run** `go test ./... && go vet ./... && gofmt -l cmd internal` — green.
- [ ] **Step 5: Commit** `feat(cli): pgrun source add/update — hidden URL prompt, structural redaction, --wait on the connection check`

---

### Task 4: `source protect` — review, decisions, acknowledgment, explicit approval, review request

**Files:**
- Create: `internal/cli/source_protect.go`
- Test: `internal/cli/source_protect_test.go`

**Interfaces:** consumes Task 1 (`GetSource`, `ProtectSource`, `ReviewSource`) and Task 2 (`sourceNameArgs`, `dispositionLabel`, `sourceNext`).

Flags: `--set <table>.<column>=<copy|fake|null|remove>` (repeatable — implement a `flag.Value` that appends; `remove` → `null`; malformed → usage error naming the bad item), `--acknowledge <table>.<column>` (repeatable), `--approve`, `--review`, `--json`, auth flags.

Flow:
1. `--review` (exclusive of the others): `ReviewSource` → `--json` raw; else `review requested for <n> columns: <list>` or `nothing is unresolved — no review needed`; exit 0.
2. `GetSource` (404 → `handleAPIError`). If `connect`/`checking`: stderr `pgrun: <name> is not connected yet — <sourceNext>`, exit 1 (nothing to protect before the schema is analyzed).
3. `ProtectSource(name, ProtectRequest{Decisions, Acknowledge, Approve})`. Error → `handleAPIError` (server messages: invalid disposition, missing acknowledgment, "N columns still need a decision…") — exit 1.
4. `--json` → dump raw, exit per rule 4 (1 while `UnresolvedCount > 0`, else 0).
5. Human output, exactly:

```text
Data protection review — <name>
```

then, when `src.SchemaChanged` and `src.SchemaChanges != nil`, a block:

```text
Production schema changed since policy v<PolicyVersion> was approved:
  added:   <comma list or ->
  removed: <comma list or ->
  retyped: <comma list or ->
```

then a tabwriter table `COLUMN\tDISPOSITION\tSOURCE` with one row per `Rules` entry: disposition via `dispositionLabel`, SOURCE = `recommended` or `decided`, with ` (sensitive)` appended to the column when `Sensitive`; then one row per `Unresolved` entry with `UNRESOLVED` and SOURCE = `valid: copy, fake, null` (+ `(sensitive — copying needs --acknowledge)` when sensitive); then, if any `TableRules`, a `TABLE\tDISPOSITION` table. Then a blank line and:
   - `UnresolvedCount > 0`: `<N> column(s) need a decision.` / `Run:` / `  pgrun source protect <name> --set <first unresolved>=copy|fake|null …` (list every unresolved key as a `--set` placeholder, one line each; sensitive ones also get `--acknowledge <key>` on the same line) / `or ask pgrun to review them:` / `  pgrun source protect <name> --review`; exit 1.
   - `Activated`: `✓ Data protection active — policy v<N>` / `Next:` / `  pgrun source copy <name> --wait`; exit 0.
   - else (complete, not approved): `Every column is resolved. Approve to activate the policy:` / `  pgrun source protect <name> --approve`; exit 0.

- [ ] **Step 1: Failing tests** — review table rendering (rules + unresolved + table rules, labels `FAKE`/`COPY`/`REMOVE`/`SCHEMA ONLY`, `(sensitive)` marker); unresolved → exit 1 with the `--set` hint lines and the `--review` hint; `--set a.b=remove` sends `"null"` and `--set` repeats produce a map with both keys; `--acknowledge` appears in the body; `--approve` sends `"approve":true`; server 422 on `--approve` with unresolved → exit 1 and the server message; server 422 "sensitive copy without acknowledgment" message → exit 1; activated response → exit 0 + `policy v1` + copy hint; complete-but-not-approved → exit 0 + `--approve` hint; schema-change block rendered from a `schema_changes` fixture; `connect` source → exit 1 with the update hint and **no POST** (assert the server saw no protect call); `--review` → POST review, exit 0; malformed `--set` → 64; `--json` verbatim with the exit code following `unresolved_count`.
- [ ] **Step 2: Run** `go test ./internal/cli/ -run 'SourceProtect'` — failures.
- [ ] **Step 3: Implement**, replace the placeholder in `runSource`.
- [ ] **Step 4: Run** `go test ./... && go vet ./... && gofmt -l cmd internal` — green.
- [ ] **Step 5: Commit** `feat(cli): pgrun source protect — review table, --set/--acknowledge decisions, explicit --approve, --review`

---

### Task 5: `source copy` — checklist, refusal messages, `--wait`

**Files:**
- Create: `internal/cli/source_copy.go`
- Test: `internal/cli/source_copy_test.go`

**Interfaces:** consumes Task 1 (`GetSource`, `CopySource`, `WaitForSource`, `CopySettled`) and Task 2.

Flags: `--wait`, `--timeout 30m`, `--json`, auth flags. Flow:
1. `GetSource`. Pre-flight by status (messages to stderr, no POST):
   - `action_required`: `pgrun: Safe Copy cannot be created.` / `` / `Production schema changed after the protection policy was approved.` / `` / `Review changes:` / `  pgrun source protect <name>` — exit 1.
   - `ready`: `pgrun: <name> already has a Safe Copy` — exit 1.
   - `failed`: `pgrun: the Safe Copy for <name> failed — write to support@postgresrun.com` — exit 1.
   - `connect`/`checking`/`protect`: `pgrun: finish data protection first — <sourceNext>` — exit 1.
   - `copying`: without `--wait` print `→ Safe Copy for <name> is being created` + `Next:` + `  pgrun source status <name>` exit 0; with `--wait` skip to step 3.
   - `ready_to_copy`: print the checklist `✓ Connection verified` / `✓ Data protection active (policy v<N>)` / `✓ Schema unchanged` then step 2.
2. `CopySource` (server is authoritative — 409/422 go through `handleAPIError`). Print `→ Creating Safe Copy`. Without `--wait`: `--json` raw, else `Next:` / `  pgrun source status <name>`; exit 0.
3. `--wait`: `WaitForSource(ctx, name, raw, src, api.CopySettled)`; deadline → `pgrun: still creating the Safe Copy for <name> after <timeout> — run \`pgrun source status <name>\``, exit 1; `ready` → `--json` raw else `✓ Safe Copy ready` / `Next:` / `  pgrun branch create <name> --name dev --wait`, exit 0; `failed` → `pgrun: ✗ Safe Copy failed — write to support@postgresrun.com`, exit 1; anything else terminal (`action_required`) → `pgrun: Safe Copy ended in <status> — run \`pgrun source status <name>\``, exit 1. `--json` dumps the last raw on every non-zero path (as `branch create --wait` does).

- [ ] **Step 1: Failing tests** — drift refusal (exit 1, the four-line message, no POST); ready → exit 1; failed → exit 1; protect → exit 1 with the protect hint; ready_to_copy no wait → POST, 202, checklist + `Creating Safe Copy`, exit 0; `--json` verbatim; `--wait` sequence copying→copying→ready → exit 0 + `Safe Copy ready` + branch hint; `--wait` copying→failed → exit 1; `--wait` timeout → exit 1; server 422 on POST → exit 1 with the API message; copying without wait → exit 0 + status hint; 401 → 2.
- [ ] **Step 2: Run** `go test ./internal/cli/ -run 'SourceCopy'` — failures.
- [ ] **Step 3: Implement**, replace the placeholder.
- [ ] **Step 4: Run** `go test ./... && go vet ./... && gofmt -l cmd internal` — green.
- [ ] **Step 5: Commit** `feat(cli): pgrun source copy — server-enforced preconditions surfaced, --wait to Safe Copy ready`

---

### Task 6: Documentation — README, AGENTS.md, skill, usage

**Files:**
- Modify: `README.md`, `AGENTS.md`, `skills/pgrun-branching/SKILL.md`, `internal/cli/cli.go` (usage already extended in Task 2 — verify it matches the docs)
- Test: `internal/cli/cli_test.go` gains one test that `pgrun help` output contains `pgrun source add` and `pgrun source copy`.

Content:
- README: Quickstart gains a "First time? Connect your production database" block with the canonical flow from spec §9 (`auth login` → `project use` → `source add --name production --url "$DATABASE_URL"` → `source status production` → `source protect production` (then `--set …`/`--approve`) → `source copy production --wait` → `branch create --name dev --wait`); a new **Sources** table row per command (list/get/status/add/update/protect/copy with flags, exit-code notes); a **Secrets** paragraph: the production URL is never printed, never in `--json`, sent only in the request body; prefer the hidden prompt (`pgrun source add --name production`) over `--url` on a shared shell because argv is visible to other local processes and shell history; `--json` requires `--url`. An explicit sentence: **a Safe Copy is a one-time copy in this release — refresh/continuous sync (Phase 6) does not exist yet; `sync` reads `snapshot_only`.**
- AGENTS.md: a `## Sources (Safe Copy setup)` section with the same flow, the exit codes per command, the status vocabulary, and safety rules 8–9: *never paste a production connection URL into a chat or task transcript — have the human run `pgrun source add --name <n>` and type it at the hidden prompt*; *never pass `--approve` on a policy you have not shown the human (`pgrun source protect <n>` prints the review table — approval is the human's decision)*.
- SKILL.md: a short "Before the first branch" subsection: if `pgrun branch create` returns 404/`no Safe Copy` or `pgrun source status <project>` is not `Safe Copy ready`, stop and tell the human to complete `pgrun source status` → `protect` → `copy --wait` themselves (data-protection decisions are theirs, and the URL must not pass through the agent).
- Keep vocabulary: Production Database, Protect Data / data protection, Safe Copy, Branch.
- Ruling 8 (Task 3): on `source add`/`source update` the `--url` flag IS the connection URL, so the API-base override on those two commands is spelled `--api-url` (`--token` unchanged; every other command keeps `--url`). Document this exception next to the config-resolution sentence in README/AGENTS.md and in `usage`. Also document the accepted residual: assigning the URL to a boolean flag (`--wait=postgres://…`) makes Go's flag package echo the invalid value — never do that; the documented flows never hit it.

- [ ] **Step 1: Failing test** for the usage text, run it, see it fail (if Task 2 already made it pass, keep the test anyway).
- [ ] **Step 2: Write the docs.**
- [ ] **Step 3: Run** `go test ./... && go vet ./... && gofmt -l cmd internal` — green.
- [ ] **Step 4: Commit** `docs: pgrun source flow — README quickstart, AGENTS.md sources contract, skill hand-off rule`
