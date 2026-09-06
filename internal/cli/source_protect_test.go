// Tests for `pgrun source protect`: the review table, --set/--acknowledge
// decisions, explicit --approve, and --review. See
// .superpowers/sdd/2026-09-06-pgrun-source-cli/task-4-brief.md for the exact
// flow, layout, and exit codes these lock in.

package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// decodeBody unmarshals a recorded request body into a generic map so a test
// can assert arbitrary field shapes (maps, arrays, bools) — recorder.bodyField
// (source_connect_test.go) only extracts a single string field.
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("request body is not JSON (%v): %q", err, body)
	}
	return decoded
}

// protectServer wires GET /api/v1/sources/<name> to getBody and POST
// /api/v1/sources/<name>/protect to (protectStatus, protectBody), routing on
// method+path like the brief instructs. rec (optional) records the protect
// request. Any other route fails the test — the way to assert "no POST was
// made" is to pass a getBody whose status keeps the CLI from ever reaching
// the protect call.
func protectServer(t *testing.T, name string, rec *recorder, getBody string, protectStatus int, protectBody string) {
	t.Helper()
	getPath := "/api/v1/sources/" + name
	protectPath := getPath + "/protect"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == getPath:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(getBody))
		case r.Method == http.MethodPost && r.URL.Path == protectPath:
			if rec != nil {
				rec.record(r)
			}
			w.WriteHeader(protectStatus)
			w.Write([]byte(protectBody))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
}

// --- review table rendering ---

func TestSourceProtect_ReviewTable(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft","policy_version":null,"unresolved_count":2}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,
		"unresolved_count":2,
		"unresolved":[
			{"column":"public.events.metadata","sensitive":false,"valid":["copy","null"]},
			{"column":"public.users.ssn","sensitive":true,"valid":["fake","null"]}
		],
		"rules":[
			{"column":"public.users.email","disposition":"fake","recommended":true,"sensitive":true},
			{"column":"public.users.id","disposition":"copy","recommended":true,"sensitive":false},
			{"column":"public.users.bio","disposition":"null","recommended":false,"sensitive":false}
		],
		"table_rules":[{"table":"public.solid_queue_jobs","disposition":"schema_only"}]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, _ := run(t, "source", "protect", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
	}
	if !strings.Contains(out, "Data protection review — acme") {
		t.Fatalf("out missing header: %q", out)
	}
	if !strings.Contains(out, "COLUMN\tDISPOSITION\tSOURCE") && !strings.Contains(out, "COLUMN") {
		t.Fatalf("out missing table header: %q", out)
	}

	line := findLine(t, out, "public.users.email")
	if !strings.Contains(line, "(sensitive)") || !strings.Contains(line, "FAKE") || !strings.Contains(line, "recommended") {
		t.Fatalf("email row = %q", line)
	}
	line = findLine(t, out, "public.users.id")
	if !strings.Contains(line, "COPY") || !strings.Contains(line, "recommended") {
		t.Fatalf("id row = %q", line)
	}
	line = findLine(t, out, "public.users.bio")
	if !strings.Contains(line, "REMOVE") || !strings.Contains(line, "decided") {
		t.Fatalf("bio row = %q", line)
	}
	line = findLine(t, out, "public.events.metadata")
	if !strings.Contains(line, "UNRESOLVED") || !strings.Contains(line, "valid: copy, null") {
		t.Fatalf("metadata row = %q", line)
	}
	if strings.Contains(line, "sensitive") {
		t.Fatalf("metadata is not sensitive, should have no sensitive marker: %q", line)
	}
	line = findLine(t, out, "public.users.ssn")
	if !strings.Contains(line, "UNRESOLVED") || !strings.Contains(line, "valid: fake, null") || !strings.Contains(line, "(sensitive — copying needs --acknowledge)") {
		t.Fatalf("ssn row = %q", line)
	}
	line = findLine(t, out, "public.solid_queue_jobs")
	if !strings.Contains(line, "SCHEMA ONLY") {
		t.Fatalf("table rule row = %q", line)
	}
	if !strings.Contains(out, "TABLE") {
		t.Fatalf("out missing TABLE header: %q", out)
	}
}

// findLine returns the first line of out containing needle, failing the test
// if none matches.
func findLine(t *testing.T, out, needle string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	t.Fatalf("no line containing %q in: %q", needle, out)
	return ""
}

// --- unresolved closing block: --set/--acknowledge hint lines ---

func TestSourceProtect_Unresolved_Hints(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,
		"unresolved_count":2,
		"unresolved":[
			{"column":"public.a.b","sensitive":false,"valid":["copy","null"]},
			{"column":"public.c.d","sensitive":true,"valid":["fake","null"]}
		],
		"rules":[],"table_rules":[]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, _ := run(t, "source", "protect", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
	}
	for _, want := range []string{
		"2 column(s) need a decision.",
		"Run:",
		"  pgrun source protect acme --set public.a.b=copy|fake|null",
		"  pgrun source protect acme --set public.c.d=copy|fake|null --acknowledge public.c.d",
		"or ask pgrun to review them:",
		"  pgrun source protect acme --review",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("out missing %q: %q", want, out)
		}
	}
	// The non-sensitive column's hint line must NOT carry --acknowledge.
	line := findLine(t, out, "public.a.b")
	if strings.Contains(line, "--acknowledge") {
		t.Fatalf("non-sensitive hint line should not carry --acknowledge: %q", line)
	}
}

// --- --set: remove aliases null, repeats build a multi-key map ---

func TestSourceProtect_SetRemoveAliasAndRepeats(t *testing.T) {
	var rec recorder
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", &rec, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme",
		"--set", "public.users.email=fake",
		"--set", "public.users.bio=remove",
	)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q, out=%q", code, stderr, out)
	}
	_, path, _, body := rec.get()
	if path != "/api/v1/sources/acme/protect" {
		t.Fatalf("path = %q", path)
	}
	decoded := decodeBody(t, body)
	decisions, ok := decoded["decisions"].(map[string]any)
	if !ok {
		t.Fatalf("decisions not a map: %v", decoded["decisions"])
	}
	if len(decisions) != 2 {
		t.Fatalf("decisions = %v, want 2 keys", decisions)
	}
	if decisions["public.users.email"] != "fake" {
		t.Fatalf("email decision = %v, want fake", decisions["public.users.email"])
	}
	if decisions["public.users.bio"] != "null" {
		t.Fatalf("bio decision = %v, want null (remove is an alias)", decisions["public.users.bio"])
	}
	if _, ok := decoded["approve"]; ok {
		t.Fatalf("approve must be absent without --approve: %v", decoded)
	}
}

// --- --acknowledge appears in the request body ---

func TestSourceProtect_AcknowledgeInBody(t *testing.T) {
	var rec recorder
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", &rec, getBody, http.StatusOK, protectBody)

	code, _, stderr := run(t, "source", "protect", "acme",
		"--set", "public.users.ssn=fake",
		"--acknowledge", "public.users.ssn",
	)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	_, _, _, body := rec.get()
	decoded := decodeBody(t, body)
	ack, ok := decoded["acknowledge"].([]any)
	if !ok || len(ack) != 1 || ack[0] != "public.users.ssn" {
		t.Fatalf("acknowledge = %v", decoded["acknowledge"])
	}
	if _, ok := decoded["approve"]; ok {
		t.Fatalf("approve must be absent without --approve: %v", decoded)
	}
}

// TestSourceProtect_FlaglessPostBodyIsEmptyObject: no --set/--acknowledge/
// --approve at all must produce an empty JSON object body — decisions,
// acknowledge, and approve are all "omitempty", and none of the three may
// leak through as a zero value (empty map/slice, or approve:false).
func TestSourceProtect_FlaglessPostBodyIsEmptyObject(t *testing.T) {
	var rec recorder
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", &rec, getBody, http.StatusOK, protectBody)

	code, _, stderr := run(t, "source", "protect", "acme")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	_, _, _, body := rec.get()
	if got := strings.TrimSpace(string(body)); got != "{}" {
		t.Fatalf("body = %q, want exactly {}", got)
	}
}

// --- --approve sends "approve":true ---

func TestSourceProtect_ApproveSendsTrue(t *testing.T) {
	var rec recorder
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":1,"activated":true,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", &rec, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme", "--approve")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q, out=%q", code, stderr, out)
	}
	_, _, _, body := rec.get()
	decoded := decodeBody(t, body)
	if decoded["approve"] != true {
		t.Fatalf("approve = %v, want true", decoded["approve"])
	}
}

// --- server 422 refusals surface as exit 1 with the server's own message ---

func TestSourceProtect_Approve422Unresolved(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"activated":false,"unresolved_count":1,"unresolved":[{"column":"public.a.b","sensitive":false,"valid":["copy","null"]}],"rules":[],"table_rules":[],"error":"1 column still needs a decision before the policy can be approved"}`
	protectServer(t, "acme", nil, getBody, http.StatusUnprocessableEntity, protectBody)

	code, _, stderr := run(t, "source", "protect", "acme", "--approve")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "1 column still needs a decision before the policy can be approved") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSourceProtect_SensitiveCopyWithoutAcknowledgment422(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"error":"public.users.ssn is sensitive — acknowledge it before copying"}`
	protectServer(t, "acme", nil, getBody, http.StatusUnprocessableEntity, protectBody)

	code, _, stderr := run(t, "source", "protect", "acme", "--set", "public.users.ssn=copy")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "acknowledge it before copying") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- closing variants: activated / complete-but-not-approved ---

func TestSourceProtect_Activated(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":1,"activated":true,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme", "--approve")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "✓ Data protection active — policy v1") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(out, "Next:") || !strings.Contains(out, "pgrun source copy acme --wait") {
		t.Fatalf("out = %q", out)
	}
}

func TestSourceProtect_CompleteNotApproved(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[{"column":"public.users.id","disposition":"copy","recommended":true,"sensitive":false}],"table_rules":[]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "Every column is resolved. Approve to activate the policy:") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(out, "  pgrun source protect acme --approve") {
		t.Fatalf("out = %q", out)
	}
}

// TestSourceProtect_ApproveDidNotActivate: --approve was passed, the server
// did not refuse (no 422) and nothing is unresolved — yet the policy is
// still not active. Printing "Approve to activate the policy: … --approve"
// here would send the caller back to the command they just ran, and exit 0
// would let a CI script walk on to `copy`. Say what happened, exit 1.
func TestSourceProtect_ApproveDidNotActivate(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[{"column":"public.users.id","disposition":"copy","recommended":true,"sensitive":false}],"table_rules":[],"extra_field":"kept-verbatim"}`
	const wantErr = "pgrun: the policy was not activated — run `pgrun source status acme`"

	t.Run("human", func(t *testing.T) {
		protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)
		code, out, stderr := run(t, "source", "protect", "acme", "--approve")
		if code != exitFailure {
			t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
		}
		if !strings.Contains(stderr, wantErr) {
			t.Fatalf("stderr = %q, want %q", stderr, wantErr)
		}
		if strings.Contains(out, "--approve") {
			t.Fatalf("stdout must not send the caller back to --approve: %q", out)
		}
	})

	t.Run("json", func(t *testing.T) {
		protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)
		code, out, stderr := run(t, "source", "protect", "acme", "--approve", "--json")
		if code != exitFailure {
			t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
		}
		if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
			t.Fatalf("--json must still dump the raw body first: %q", out)
		}
		if !strings.Contains(stderr, wantErr) {
			t.Fatalf("stderr = %q, want %q", stderr, wantErr)
		}
	})
}

// TestSourceProtect_ExitCodeFollowsUnresolvedList_NotJustCount covers the
// case where unresolved_count and the Unresolved list disagree: the exit
// code (and the closing variant) must follow whatever the table actually
// rendered (len(Unresolved)), never a possibly-stale unresolved_count.
func TestSourceProtect_ExitCodeFollowsUnresolvedList_NotJustCount(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[{"column":"public.a.b","sensitive":false,"valid":["copy","null"]}],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (out=%q, stderr=%q)", code, exitFailure, out, stderr)
	}
	if !strings.Contains(out, "1 column(s) need a decision.") {
		t.Fatalf("out missing the unresolved closing: %q", out)
	}
	if strings.Contains(out, "Every column is resolved") {
		t.Fatalf("out should not show the complete-but-not-approved closing: %q", out)
	}
}

// --- schema-change block ---

func TestSourceProtect_SchemaChangeBlock(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"action_required","step":4,"protection":"active","policy_version":3,` +
		`"schema_changed":true,"schema_changes":{"added":["public.users.new_col"],"removed":[],"retyped":["public.orders.amount"]}}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":3,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"Production schema changed since policy v3 was approved:",
		"added:   public.users.new_col",
		"removed: -",
		"retyped: public.orders.amount",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("out missing %q: %q", want, out)
		}
	}
	// No rules and no unresolved columns in this fixture — the
	// COLUMN/DISPOSITION/SOURCE table must be skipped entirely rather than
	// rendering a bare, row-less header.
	if strings.Contains(out, "DISPOSITION") {
		t.Fatalf("out should not render an empty review table: %q", out)
	}
}

// TestSourceProtect_NoSchemaChangeBlockWhenNotChanged locks in that the block
// only renders when SchemaChanged is true AND SchemaChanges is non-nil.
func TestSourceProtect_NoSchemaChangeBlockWhenNotChanged(t *testing.T) {
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft","schema_changed":false,"schema_changes":null}`
	protectBody := `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[]}`
	protectServer(t, "acme", nil, getBody, http.StatusOK, protectBody)

	code, out, stderr := run(t, "source", "protect", "acme")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if strings.Contains(out, "schema changed") {
		t.Fatalf("out should not contain the schema-change block: %q", out)
	}
}

// --- pre-flight gate: connect/checking sources refuse without posting ---

func TestSourceProtect_NotConnectedRefusesWithoutPost(t *testing.T) {
	cases := []struct {
		status string
		next   string
	}{
		// "connect": sourceNext points at the update command.
		{"connect", `pgrun source update acme --url "$DATABASE_URL"`},
		// "checking" with no check error yet: sourceNext just says to poll
		// status — there's nothing to update while the check is still
		// running. Both must still refuse the protect POST outright.
		{"checking", "pgrun source status acme"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			getPath := "/api/v1/sources/acme"
			withServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == getPath {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{"name":"acme","safe_copy_status":"` + tc.status + `","step":1,"protection":"none","last_check_error":null}`))
					return
				}
				t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
			})
			code, out, stderr := run(t, "source", "protect", "acme")
			if code != exitFailure {
				t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
			}
			if !strings.Contains(stderr, "acme is not connected yet") {
				t.Fatalf("stderr = %q", stderr)
			}
			if !strings.Contains(stderr, tc.next) {
				t.Fatalf("stderr missing next-command hint %q: %q", tc.next, stderr)
			}
		})
	}
}

// TestSourceProtect_CheckFailedRefusesWithUpdateHint covers "checking" with
// a failed check (last_check_error set) — sourceNext then does point at the
// update command, same as the plain "connect" case.
func TestSourceProtect_CheckFailedRefusesWithUpdateHint(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == getPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"checking","step":1,"protection":"none","last_check_error":"PG::ConnectionBad"}`))
			return
		}
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := run(t, "source", "protect", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
	}
	if !strings.Contains(stderr, "acme is not connected yet") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, `pgrun source update acme --url "$DATABASE_URL"`) {
		t.Fatalf("stderr missing update hint: %q", stderr)
	}
}

// --- --review ---

func TestSourceProtect_Review_Unresolved(t *testing.T) {
	var rec recorder
	reviewPath := "/api/v1/sources/acme/review"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == reviewPath {
			rec.record(r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","unresolved":["public.users.a","public.users.b"],"requested":true}`))
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := run(t, "source", "protect", "acme", "--review")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "review requested for 2 columns: public.users.a, public.users.b") {
		t.Fatalf("out = %q", out)
	}
	if method, _, _, _ := rec.get(); method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
}

func TestSourceProtect_Review_NothingUnresolved(t *testing.T) {
	reviewPath := "/api/v1/sources/acme/review"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == reviewPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","unresolved":[],"requested":false}`))
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := run(t, "source", "protect", "acme", "--review")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "nothing is unresolved — no review needed") {
		t.Fatalf("out = %q", out)
	}
}

func TestSourceProtect_Review_JSON(t *testing.T) {
	reviewPath := "/api/v1/sources/acme/review"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == reviewPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","unresolved":["public.users.a"],"requested":true,"extra_field":"kept-verbatim"}`))
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := run(t, "source", "protect", "acme", "--review", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw body verbatim: %q", out)
	}
}

// TestSourceProtect_ReviewCombinedWithOtherFlagsIsUsage covers ruling 4's
// usage-exit case: --review is exclusive of --set/--acknowledge/--approve.
func TestSourceProtect_ReviewCombinedWithOtherFlagsIsUsage(t *testing.T) {
	cases := [][]string{
		{"source", "protect", "acme", "--review", "--set", "public.a.b=copy"},
		{"source", "protect", "acme", "--review", "--acknowledge", "public.a.b"},
		{"source", "protect", "acme", "--review", "--approve"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			withServer(t, func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
			})
			code, _, stderr := run(t, args...)
			if code != exitUsage {
				t.Fatalf("code = %d, want %d (stderr=%q)", code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "--review") {
				t.Fatalf("stderr should name --review: %q", stderr)
			}
		})
	}
}

// --- malformed --set ---

func TestSourceProtect_MalformedSetIsUsage(t *testing.T) {
	cases := []string{
		"noequalssign",
		"public.a.b=bogus",
		"=copy",
		"public.a.b=",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			withServer(t, func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
			})
			code, _, stderr := run(t, "source", "protect", "acme", "--set", bad)
			if code != exitUsage {
				t.Fatalf("code = %d, want %d (stderr=%q)", code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "--set") {
				t.Fatalf("stderr should name --set: %q", stderr)
			}
		})
	}
}

// TestSourceProtect_DuplicateSetKeyIsUsage: a repeated --set for the same
// column is refused before any request — silently letting the last one win
// would hide a likely mistake.
func TestSourceProtect_DuplicateSetKeyIsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "protect", "acme",
		"--set", "public.users.bio=copy",
		"--set", "public.users.bio=fake",
	)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, `--set names "public.users.bio" twice`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- --json verbatim, exit code follows unresolved_count ---

func TestSourceProtect_JSON_ExitFollowsUnresolvedCount(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"unresolved", `{"name":"acme","safe_copy_status":"protect","policy_version":null,"activated":false,"unresolved_count":1,"unresolved":[{"column":"public.a.b","sensitive":false,"valid":["copy","null"]}],"rules":[],"table_rules":[],"extra_field":"kept-verbatim"}`, exitFailure},
		{"complete", `{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":1,"activated":true,"unresolved_count":0,"unresolved":[],"rules":[],"table_rules":[],"extra_field":"kept-verbatim"}`, exitSuccess},
	}
	getBody := `{"name":"acme","safe_copy_status":"protect","step":2,"protection":"draft"}`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			protectServer(t, "acme", nil, getBody, http.StatusOK, tc.body)
			code, out, stderr := run(t, "source", "protect", "acme", "--json")
			if code != tc.code {
				t.Fatalf("code = %d, want %d (stderr=%q)", code, tc.code, stderr)
			}
			if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
				t.Fatalf("--json must print the raw body verbatim: %q", out)
			}
			if strings.Contains(out, "DISPOSITION") {
				t.Fatalf("--json must not also render the human table: %q", out)
			}
		})
	}
}

// --- unexpected trailing positional / dispatch ---

func TestSourceProtect_IsDispatched(t *testing.T) {
	isolateHome(t)
	code, _, stderr := run(t, "source", "protect")
	if code == exitUsage && strings.Contains(stderr, "not implemented yet") {
		t.Fatalf("protect is still the Task 2 placeholder: %q", stderr)
	}
}
