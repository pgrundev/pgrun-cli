<!-- Supplied verbatim by the operator 2026-09-06 as the "pgrun source CLI" slice brief.
     Transmission garbles preserved as-is: "Use the exiserminology" (existing terminology),
     "supplying a neL" (new URL), "respect the e-closed policy model" (fail-closed),
     "Do not crete source-project selection behavior" (create),
     "deploy only if the change requires server-side changes action safety checks pass"
     (…server-side changes and production safety checks pass). The implementation plan is
     docs/superpowers/plans/2026-09-06-pgrun-source-cli.md. -->

Continue from the current deployed PGRun main branch.

Current state:

* Safe Copy UX Slice A is merged, deployed, production-verified, and pushed.
* Customer flow is:
  Connect Postgres → Protect Data → Create Safe Copy → Create Branch.
* `/api/v1/sources` already exists and is the API foundation for CLI source operations.
* Connection URLs must never be returned by the API or exposed in logs.
* Existing Safe Copy/privacy/fail-closed behavior must remain unchanged.
* Full suite was green after the previous slice.
* Do NOT start Phase 6 continuous sync/re-copy in this slice.

## Goal

Implement the first complete customer-facing `pgrun source` CLI workflow using the existing `/api/v1/sources` API.

The CLI should let a developer complete or inspect the source/Safe Copy onboarding flow without needing the web UI where the API already supports it.

Target mental model:

```text
pgrun source add
pgrun source status
pgrun source protect
pgrun source copy

then:

pgrun branch create
```

Use the exiserminology:

* Production Database / Source
* Protect Data
* Safe Copy
* Branch

Do not expose internal names such as Golden, BranchSource, SourceDatabase, etc. unless required internally by existing code.

---

# 1. Inspect before changing

First inspect:

* current `pgrun` Go CLI architecture
* auth/token handling
* project selection behavior
* API client conventions
* JSON output conventions
* error formatting
* current `/api/v1/sources` controller/routes/response schemas
* existing source status values and Safe Copy state derivation
* current CLI tests

Reuse existing CLI patterns rather than inventing a parallel framework.

Do not rename deployed backend models or database fields for cosmetic reasons.

---

# 2. Commands

Implement:

## `pgrun source list`

List sources for the current project.

Human output should include useful fields such as:

```text
NAME            STATUS            SAFE COPY
production      ready             ready
staging         action_required   unavailable
```

Use actual API fields/statuses rather than inventing state.

Support:

```bash
pgrun source list --json
```

JSON should be stable and machine-readable.

---

## `pgrun source get`

Example:

```bash
pgrun source get production
```

or use the established CLI resource-ID convention if names are not unique.

Show:

* source name
* connection/check status
* protection status
* Safe Copy status
* relevant schema/policy state
* actionable next step where appropriate

Never print credentials or connection URLs.

Support `--json`.

---

## `pgrun source add`

Primary form:

```bash
pgrun source add \
  --name production \
  --url "$DATABASE_URL"
```

If current API requires different fields, map cleanly to the existing API contract.

Important:

* accept the connection URL as a CLI input
* never echo it back
* never include it in normal errors
* never log it
* avoid command/debug output that could expose it
* use the backend's existing `connection_url` field naming

If the CLI currently has a secure stdin/input convention, support that where appropriate.

If useful and consistent with the CLI architecture, also support:

```bash
pgrun source add --name production
```

with secure interactive input for the URL.

Do not overbuild interactive UX if the current CLI is intentionally non-interactive.

After creation, display the source and current check/status state.

If the backend check is asynchronous, follow the existing CLI wait/poll conventions.

Do not poll forever.

Failed connection checks must terminate with a clear recoverable message.

Example conceptual result:

```text
✓ Production database added
→ Checking connection...

Connection failed.

Update the connection URL and try again:
  pgrun source update production --url "$DATABASE_URL"
```

Use actual supported API actions.

---

## `pgrun source update`

If the API already supports updating the connection URL, expose:

```bash
pgrun source update production --url "$DATABASE_URL"
```

This is important because failed connection checks are recoverable by supplying a neL.

Same secret-handling requirements as `source add`.

If the API contract does not safely support this yet, do not fake it. Document the blocker.

---

## `pgrun source status`

Example:

```bash
pgrun source status production
```

This should answer:

* Is Postgres connected?
* Has schema analysis completed?
* Is the protection policy complete?
* Is a Safe Copy ready?
* Is there action required?
* What should the customer do next?

Prefer a compact status view such as:

```text
Production Database: production

✓ Connected
✓ Schema analyzed
✓ Data protection active
✓ Safe Copy ready

Next:
  pgrun branch create
```

Or:

```text
Production Database: production

✓ Connected
! Data protection needs review
○ Safe Copy not created

Next:
  pgrun source protect production
```

The CLI must derive this from API state and must not create a competing state machine.

Support `--json`.

---

# 3. Protection workflow

Implement:

```bash
pgrun source protect production
```

The command must respect the e-closed policy model.

Important invariants:

* recommended rules may be shown/applied
* unresolved columns must remain unresolved until explicitly resolved
* policy activation must fail while unresolved columns exist
* sensitive columns copied unmasked require the existing explicit acknowledgment
* no `copy everything`
* no automatic weakening of protections
* unsupported/unsafe cases remain review/support cases
* use the existing backend policy transition/domain methods only

If the API currently supports separate review/update/activate operations, model the CLI around those actual API operations.

Do not bypass backend validations from the CLI.

A reasonable interaction could be:

```text
pgrun source protect production

Data protection review

users.email        FAKE
users.name         FAKE
users.password     REMOVE
events.metadata    UNRESOLVED

1 column requires a decision.

Run:
  pgrun source protect production --set events.metadata=copy
```

But align the exact UX with what the existing API supports.

Allowed customer-facing decisions remain conceptually:

```text
COPY
FAKE
NULL / REMOVE
```

Use the terminology already established by the deployed API/UI.

Do not invent a new policy representation.

Support non-interactive operation suitable for CI/agents.

---

# 4. Create Safe Copy

Implement:

```bash
pgrun source copy production
```

This should call the existing Safe Copy/import operation.

Preconditions must remain enforced server-side:

* source connection valid
* approved active protection policy
* no unresolved columns
* production schema fingerprint matches approved policy/schema state

If schema drift exists:

```text
Safe Copy cannot be created.

Production schema changed after the protection policy was approved.

Review changes:
  pgrun source protect production
```

Do not override or auto-approve schema drift.

If a Safe Copy creation job is asynchronous, support:

```bash
pgrun source copy production --wait
```

if consistent with existing `branch create --wait` behavior.

Show useful progress without leaking sensitive information.

Conceptual:

```text
✓ Connection verified
✓ Protection policy active
✓ Schema unchanged
→ Creating Safe Copy...

✓ Safe Copy ready
```

A failed Safe Copy remains a support/recovery case in this slice. Do not implement Phase 6 refresh/re-copy behavior under live branches.

---

# 5. Agent/machine usability

All major commands should support `--json`.

JSON must:

* be stable
* avoid terminal decoration
* never contain source credentials
* never contain the original connection URL
* use explicit statuses
* be suitable for agents/CI

Ensure non-zero exit codes for actionable failures.

Examples:

* authentication failure
* project missing
* source not found
* connection failed
* unresolved protection policy
* schema drift/action_required
* Safe Copy failed

Do not encode every failure as exit 0 plus prose.

---

# 6. Project handling

Use the same project semantics as existing commands:

```bash
pgrun projects use
pgrun branch create
```

Do not crete source-project selection behavior.

If current CLI supports project flags, preserve them.

---

# 7. Security requirements

Treat source Postgres credentials as high-sensitivity secrets.

Verify specifically:

* connection URL is not printed
* connection URL is not included in errors
* connection URL is not included in JSON output
* connection URL is not included in HTTP debug logs
* passwords are not accidentally preserved in test snapshots
* query parameters are not used in a way that Rails or proxies may log
* API errors are sanitized

Add tests specifically for secret redaction.

Do not weaken any existing Rails parameter filtering.

---

# 8. Compatibility

Do not break existing:

```text
pgrun auth
pgrun projects
pgrun branch
MCP
agent workflows
```

The new source commands should fit the current CLI architecture.

Do not rename current public commands without migration/compatibility handling.

---

# 9. Documentation

Update relevant CLI docs / README / agents documentation.

The canonical new first-run CLI flow should become approximately:

```bash
pgrun auth login

pgrun projects use <project>

pgrun source add \
  --name production \
  --url "$DATABASE_URL"

pgrun source status production

pgrun source protect production

pgrun source copy production --wait

pgrun branch create --name dev --wait
```

Do not claim source continuous sync exists yet.

Be explicit that Phase 6 refresh/continuous sync is not part of this slice.

---

# 10. Tests

Add focused tests for:

* source list
* source get
* source add
* failed connection state
* URL update/retry
* source status
* protection workflow
* unresolved protection rules
* protection activation refusal
* sensitive COPY acknowledgment where applicable
* Safe Copy creation
* schema drift/action_required
* Safe Copy failure
* JSON output
* exit codes
* auth/project errors
* connection URL/password redaction

Run:

1. focused CLI tests
2. Go CLI full test suite
3. Rails/API tests if backend changes are required
4. full relevant repository suite

Do not deploy with failing tests.

---

# 11. Scope boundaries

Do NOT implement in this slice:

* continuous CDC
* Phase 6 re-copy/refresh
* automatic Safe Copy refresh
* pgstream integration
* multi-host scheduling
* gateway
* scale-to-zero
* billing changes
* per-branch pricing
* Stripe changes
* another UI redesign
* new internal model renames

Do not make unrelated refactors.

---

# 12. Completion criteria

The slice is complete when a fresh authenticated customer can reasonably perform:

```text
Connect Postgres
      ↓
Inspect status
      ↓
Protect Data
      ↓
Create Safe Copy
      ↓
Create Branch
```

from the CLI/API path without weakening any existing safety guarantees.

After implementation:

1. run the complete test suite
2. perform a security-focused review
3. specifically review secret handling
4. specifically review fail-closed schema/policy behavior
5. fix Critical/Important findings
6. re-run tests
7. merge only from a clean tree
8. deploy only if the change requires server-side changes action safety checks pass
9. push main
10. provide a concise factual report with:

* commits
* commands shipped
* tests/results
* production verification if applicable
* anything intentionally deferred
* blockers discovered

Do not begin Phase 6 automatically after completing this slice.

Stop after the `pgrun source` CLI slice and report results.
