package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/pgrundev/pgrun-cli/internal/config"
)

// --- source list ---

func TestSourceList_Table(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sources" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"sources":[` +
			`{"name":"production","safe_copy_status":"ready","protection":"active","policy_version":2,"branches":3},` +
			`{"name":"staging","safe_copy_status":"action_required","protection":"active","policy_version":1,"branches":0},` +
			`{"name":"devbox","safe_copy_status":"protect","protection":"draft","policy_version":null,"branches":0}` +
			`]}`))
	})
	code, out, _ := run(t, "source", "list")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"NAME", "STATUS", "PROTECTION", "SAFE COPY", "BRANCHES",
		"production", "ready", "active v2", "3",
		"staging", "action_required", "active v1", "action required", "0",
		"devbox", "protect", "draft",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("table leaked a connection URL: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected header + 3 rows, got %d lines: %q", len(lines), out)
	}
	// devbox's protection is "draft" with no active policy version —
	// protectionCell's passthrough branch must render the bare value, with
	// no " vN" suffix (that's reserved for an *active* policy).
	var devboxLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "devbox") {
			devboxLine = l
		}
	}
	if devboxLine == "" {
		t.Fatalf("no devbox row found: %q", out)
	}
	fields := strings.Fields(devboxLine)
	if len(fields) < 3 || fields[2] != "draft" {
		t.Fatalf("devbox PROTECTION column = %v, want exactly %q: %q", fields, "draft", devboxLine)
	}
}

func TestSourceList_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"sources":[{"name":"production","safe_copy_status":"ready","branches":1}],"extra_field":"kept-verbatim"}`))
	})
	code, out, _ := run(t, "source", "list", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw body verbatim: %q", out)
	}
}

// TestSourceList_JSON_Empty locks in that the --json path never substitutes
// the human "no production databases yet" message — an empty sources array
// prints the raw body verbatim, same as any other --json response.
func TestSourceList_JSON_Empty(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"sources":[]}`))
	})
	code, out, stderr := run(t, "source", "list", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(out) != `{"sources":[]}` {
		t.Fatalf("out = %q, want the raw body verbatim", out)
	}
	if strings.Contains(stderr, "no production databases yet") {
		t.Fatalf("--json path must not print the human empty-list message: stderr=%q", stderr)
	}
}

func TestSourceList_Empty(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"sources":[]}`))
	})
	code, out, stderr := run(t, "source", "list")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(stderr, "no production databases yet") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, `pgrun source add --name <n> --url "$DATABASE_URL"`) {
		t.Fatalf("stderr missing next command: %q", stderr)
	}
}

func TestSourcesAlias_List(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"sources":[]}`))
	})
	code, _, stderr := run(t, "sources", "list")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr, "no production databases yet") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- source get ---

func TestSourceGet_Line(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sources/production" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"ready_to_copy","step":3,"protection":"active","policy_version":2,"unresolved_count":0,"branches":1,"last_check_error":null}`))
	})
	code, out, _ := run(t, "source", "get", "production")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"name=production", "status=ready_to_copy", "step=3", "protection=active",
		"policy=v2", "unresolved=0", "branches=1", "check_error=-",
		"next: pgrun source copy production --wait",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("get line missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("get leaked a connection URL: %q", out)
	}
}

// TestSourceGet_Line_PolicyDashWhenNoVersion covers sourceLine's
// PolicyVersion == 0 branch (the API sends null when no policy has ever
// been approved) — the get line must show "policy=-", not "policy=v0".
func TestSourceGet_Line_PolicyDashWhenNoVersion(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"connect","step":1,"protection":"none","policy_version":null,"unresolved_count":0,"branches":0,"last_check_error":null}`))
	})
	code, out, _ := run(t, "source", "get", "production")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "policy=-") {
		t.Fatalf("expected policy=- when policy_version is null: %q", out)
	}
}

func TestSourceGet_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"ready","extra_field":"kept-verbatim"}`))
	})
	code, out, _ := run(t, "source", "get", "production", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "extra_field") {
		t.Fatalf("--json must print raw body verbatim: %q", out)
	}
}

func TestSourceGet_ProjectFallback(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sources/jobsgpt" {
			t.Fatalf("expected fallback project jobsgpt in path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"jobsgpt","safe_copy_status":"connect"}`))
	})
	dir := t.TempDir()
	chdir(t, dir)
	if _, err := config.SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	code, out, stderr := run(t, "source", "get")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(out, "name=jobsgpt") {
		t.Fatalf("out = %q", out)
	}
}

func TestSourceGet_NoNameNoProject_ExitsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	dir := t.TempDir()
	chdir(t, dir)

	code, _, stderr := run(t, "source", "get")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "no .pgrun/project found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceGet_TwoPositionals_PrintsGetUsage covers sourceNameArgs's "too
// many positionals" path for the "get" command specifically — the usage
// text shown must be get's own ("[--json]"), not some other command's.
func TestSourceGet_TwoPositionals_PrintsGetUsage(t *testing.T) {
	isolateHome(t)
	code, _, stderr := run(t, "source", "get", "a", "b")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "pgrun source get [<name>] [--json]") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceNameArgs_UsageFlagsPerCommand exercises sourceUsageFlags'
// per-command map directly through sourceNameArgs: "copy" must report its
// own flags ([--wait] [--timeout 30m] [--json]), not the generic
// [--json] every other command used to get regardless of which one it was.
func TestSourceNameArgs_UsageFlagsPerCommand(t *testing.T) {
	var stderr bytes.Buffer
	name, rest, code, ok := sourceNameArgs("copy", []string{"a", "b"}, &stderr)
	if ok {
		t.Fatalf("expected ok=false for two positionals, got name=%q rest=%v", name, rest)
	}
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "[--wait] [--timeout 30m] [--json]") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSourceGet_404ExitsFailureWithMessage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"source not found"}`))
	})
	code, _, stderr := run(t, "source", "get", "bogus")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "source not found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- source status ---

func TestSourceStatus_Views(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		lines []string
		next  string
		code  int
	}{
		{
			name: "connect",
			body: `{"name":"acme","safe_copy_status":"connect","step":1,"protection":"none"}`,
			lines: []string{
				"○ Not connected — pgrun needs a PostgreSQL connection URL",
				"○ Schema not analyzed yet",
				"○ Data protection not started",
				"○ Safe Copy not created",
			},
			next: `pgrun source update acme --url "$DATABASE_URL"`,
			code: exitSuccess,
		},
		{
			name: "checking",
			body: `{"name":"acme","safe_copy_status":"checking","step":1,"protection":"none","last_check_error":null}`,
			lines: []string{
				"→ Checking connection (read-only, nothing is copied)",
				"○ Schema not analyzed yet",
				"○ Data protection not started",
				"○ Safe Copy not created",
			},
			next: "pgrun source status acme",
			code: exitSuccess,
		},
		{
			name: "protect",
			body: `{"name":"acme","safe_copy_status":"protect","step":2,"postgres_version":"16.4","protection":"draft","unresolved_count":3,"tables":10,"sensitive_columns":5}`,
			lines: []string{
				"✓ Connected, Postgres 16.4",
				"✓ Schema analyzed (10 tables, 5 potentially sensitive columns)",
				"! Data protection needs review (3 columns need a decision)",
				"○ Safe Copy not created",
			},
			next: "pgrun source protect acme",
			code: exitSuccess,
		},
		{
			// protectionLine's "draft" branch has three distinct outcomes
			// (plural/singular unresolved count, and zero-unresolved); the
			// "protect" case above only exercises unresolved_count:3
			// (plural). This covers the singular grammar.
			name: "protect_singular_unresolved",
			body: `{"name":"acme","safe_copy_status":"protect","step":2,"postgres_version":"16.4","protection":"draft","unresolved_count":1,"tables":10,"sensitive_columns":5}`,
			lines: []string{
				"! Data protection needs review (1 column needs a decision)",
			},
			next: "pgrun source protect acme",
			code: exitSuccess,
		},
		{
			// ...and this covers the zero-unresolved ("reviewed but not
			// approved") outcome.
			name: "protect_reviewed_not_approved",
			body: `{"name":"acme","safe_copy_status":"protect","step":2,"postgres_version":"16.4","protection":"draft","unresolved_count":0,"tables":10,"sensitive_columns":5}`,
			lines: []string{
				"! Data protection reviewed but not approved",
			},
			next: "pgrun source protect acme",
			code: exitSuccess,
		},
		{
			name: "ready_to_copy",
			body: `{"name":"acme","safe_copy_status":"ready_to_copy","step":3,"postgres_version":"16.4","protection":"active","policy_version":2,"tables":10,"sensitive_columns":5,"schema_changed":false}`,
			lines: []string{
				"✓ Connected, Postgres 16.4",
				"✓ Schema analyzed (10 tables, 5 potentially sensitive columns)",
				"✓ Data protection active (policy v2)",
				"○ Safe Copy not created",
			},
			next: "pgrun source copy acme --wait",
			code: exitSuccess,
		},
		{
			name: "copying",
			body: `{"name":"acme","safe_copy_status":"copying","step":4,"postgres_version":"16.4","protection":"active","policy_version":2,"tables":10,"sensitive_columns":5,"schema_changed":false}`,
			lines: []string{
				"✓ Connected, Postgres 16.4",
				"✓ Schema analyzed (10 tables, 5 potentially sensitive columns)",
				"✓ Data protection active (policy v2)",
				"→ Creating Safe Copy",
			},
			next: "pgrun source status acme",
			code: exitSuccess,
		},
		{
			name: "ready",
			body: `{"name":"acme","safe_copy_status":"ready","step":4,"postgres_version":"16.4","protection":"active","policy_version":2,"tables":10,"sensitive_columns":5,"schema_changed":false,"branches":1}`,
			lines: []string{
				"✓ Connected, Postgres 16.4",
				"✓ Schema analyzed (10 tables, 5 potentially sensitive columns)",
				"✓ Data protection active (policy v2)",
				"✓ Safe Copy ready",
			},
			next: "pgrun branch create acme --name dev --wait",
			code: exitSuccess,
		},
		{
			name: "action_required",
			body: `{"name":"acme","safe_copy_status":"action_required","step":4,"postgres_version":"16.4","protection":"active","policy_version":2,"tables":10,"sensitive_columns":5,"schema_changed":true}`,
			lines: []string{
				"✓ Connected, Postgres 16.4",
				"✓ Schema analyzed (10 tables, 5 potentially sensitive columns)",
				"! Data protection policy v2 was approved against an older schema",
				"! Safe Copy ready, but production schema changed since the policy was approved",
			},
			next: "pgrun source protect acme",
			code: exitFailure,
		},
		{
			name: "action_required_no_copy_yet",
			body: `{"name":"acme","safe_copy_status":"action_required","step":2,"postgres_version":"16.4","protection":"active","policy_version":2,"tables":10,"sensitive_columns":5,"schema_changed":true}`,
			lines: []string{
				"○ Safe Copy not created — production schema changed, review protection first",
			},
			next: "pgrun source protect acme",
			code: exitFailure,
		},
		{
			name: "failed",
			body: `{"name":"acme","safe_copy_status":"failed","step":4,"postgres_version":"16.4","protection":"active","policy_version":2,"tables":10,"sensitive_columns":5,"schema_changed":false}`,
			lines: []string{
				"✓ Connected, Postgres 16.4",
				"✓ Schema analyzed (10 tables, 5 potentially sensitive columns)",
				"✓ Data protection active (policy v2)",
				"✗ Safe Copy failed",
			},
			next: "write to support@postgresrun.com (a failed Safe Copy is a support case)",
			code: exitFailure,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			withServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(body))
			})
			code, out, _ := run(t, "source", "status", "acme")
			if code != tc.code {
				t.Fatalf("code = %d, want %d (out=%q)", code, tc.code, out)
			}
			for _, want := range tc.lines {
				if !strings.Contains(out, want) {
					t.Fatalf("status view missing %q: %q", want, out)
				}
			}
			if !strings.Contains(out, "Next:") || !strings.Contains(out, tc.next) {
				t.Fatalf("status view missing next command %q: %q", tc.next, out)
			}
			if !strings.Contains(out, "Production Database: acme") {
				t.Fatalf("status view missing header: %q", out)
			}
			if strings.Contains(out, "postgres://") {
				t.Fatalf("status view leaked a connection URL: %q", out)
			}
		})
	}
}

// TestSourceStatus_CheckFailed covers the "checking" status with a non-empty
// last_check_error — a distinct rendering (and exit code) from the plain
// "checking, no error yet" case above.
func TestSourceStatus_CheckFailed(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"checking","step":1,"protection":"none","last_check_error":"PG::ConnectionBad"}`))
	})
	code, out, _ := run(t, "source", "status", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out, "✗ Connection failed (PG::ConnectionBad)") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(out, "Next:") || !strings.Contains(out, `pgrun source update acme --url "$DATABASE_URL"`) {
		t.Fatalf("out = %q", out)
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("status view leaked a connection URL: %q", out)
	}
}

func TestSourceStatus_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"ready","extra_field":"kept-verbatim"}`))
	})
	code, out, _ := run(t, "source", "status", "acme", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "extra_field") {
		t.Fatalf("--json must print raw body verbatim: %q", out)
	}
}

// TestSourceStatus_JSON_StillReflectsActionableExitCode locks in that --json
// changes how the outcome is reported, never whether it's actionable.
func TestSourceStatus_JSON_StillReflectsActionableExitCode(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"failed"}`))
	})
	code, out, _ := run(t, "source", "status", "acme", "--json")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out, `"safe_copy_status":"failed"`) {
		t.Fatalf("--json should still dump the raw body: %q", out)
	}
}

func TestSourceStatus_401ExitsAuth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	code, _, stderr := run(t, "source", "status", "acme")
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSourceStatus_404ExitsFailureWithMessage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"source not found"}`))
	})
	code, _, stderr := run(t, "source", "status", "bogus")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "source not found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- dispatch ---
//
// add/update are implemented in source_connect.go (see
// source_connect_test.go); protect is implemented in source_protect.go (see
// source_protect_test.go); copy is implemented in source_copy.go (see
// TestSourceCopy_IsDispatched in source_copy_test.go).

func TestSource_UnknownSubcommandIsUsage(t *testing.T) {
	isolateHome(t)
	code, _, _ := run(t, "source", "bogus")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestSource_NoSubcommandIsUsage(t *testing.T) {
	isolateHome(t)
	code, _, _ := run(t, "source")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

// TestSourceUsageErrors_NeverEchoAURL is the cross-cutting property behind
// redactedArg: a usage error is printed before any redactor exists, so any
// usage message that echoes its argument is a print site for a secret. Each
// case below puts a credentialed URL in the one position that reaches a
// different usageErrf site — the subcommand name, the trailing positional of
// each command that never takes a URL, and a malformed --set item whose
// *value* is a URL (the argument is not URL-shaped as a whole, so the shape
// test has to look inside it). None of them may repeat what was typed.
func TestSourceUsageErrors_NeverEchoAURL(t *testing.T) {
	cases := []struct {
		what string
		args []string
	}{
		{"unknown subcommand", []string{"source", testConnURL}},
		{"list trailing positional", []string{"source", "list", "--json", testConnURL}},
		{"get trailing positional", []string{"source", "get", "acme", "--json", testConnURL}},
		{"status trailing positional", []string{"source", "status", "acme", "--json", testConnURL}},
		{"protect trailing positional", []string{"source", "protect", "acme", "--json", testConnURL}},
		{"protect malformed --set", []string{"source", "protect", "acme", "--set", "public.users.email=" + testConnURL}},
		{"copy trailing positional", []string{"source", "copy", "acme", "--json", testConnURL}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			withServer(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
			})
			code, out, stderr := run(t, tc.args...)
			if code != exitUsage {
				t.Fatalf("code = %d, want %d (stdout=%q stderr=%q)", code, exitUsage, out, stderr)
			}
			for _, stream := range []struct{ where, s string }{{"stdout", out}, {"stderr", stderr}} {
				assertNoLeak(t, stream.where, stream.s)
				if strings.Contains(stream.s, "postgres://") {
					t.Fatalf("%s echoed a connection URL: %q", stream.where, stream.s)
				}
			}
		})
	}
}

// --- dispositionLabel (used by Tasks 3-5, exercised here since it's added
// in this task's source_output.go) ---

func TestDispositionLabel(t *testing.T) {
	cases := map[string]string{
		"copy":        "COPY",
		"fake":        "FAKE",
		"null":        "REMOVE",
		"copy_data":   "COPY",
		"schema_only": "SCHEMA ONLY",
		"exclude":     "EXCLUDE",
		"weird":       "WEIRD",
	}
	for in, want := range cases {
		if got := dispositionLabel(in); got != want {
			t.Errorf("dispositionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
