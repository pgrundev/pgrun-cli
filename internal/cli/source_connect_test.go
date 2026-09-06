package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pgrundev/pgrun-cli/internal/config"
)

// The one connection URL every test in this file feeds the CLI. Its password
// is deliberately a distinctive token: any assertion that greps output for
// it is really asking "did the secret escape?".
const (
	testConnURL  = "postgres://app:SECRETPW@db.example.com:5432/prod"
	testPassword = "SECRETPW"
)

// recorder captures what the fake API actually received, so the tests can
// assert the connection URL travelled in the JSON body (and nowhere else).
type recorder struct {
	mu     sync.Mutex
	method string
	path   string
	query  string
	body   []byte
}

func (rec *recorder) record(r *http.Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.method, rec.path, rec.query = r.Method, r.URL.Path, r.URL.RawQuery
	rec.body, _ = io.ReadAll(r.Body)
}

func (rec *recorder) get() (method, path, query string, body []byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.method, rec.path, rec.query, rec.body
}

// bodyField decodes one string field out of the recorded request body.
func (rec *recorder) bodyField(t *testing.T, key string) string {
	t.Helper()
	_, _, _, body := rec.get()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("request body is not JSON (%v): %q", err, body)
	}
	s, _ := decoded[key].(string)
	return s
}

// assertNoLeak is the whole point of this file: no output stream may ever
// carry the connection URL or its password.
func assertNoLeak(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, testPassword) || strings.Contains(s, testConnURL) {
		t.Fatalf("%s leaked the connection URL: %q", where, s)
	}
}

// runSourceAdd drives sourceAdd directly with a plain reader standing in for
// a terminal — the injection point that makes the hidden URL prompt testable
// without a TTY (same shape as runLogin in auth_login_test.go).
func runSourceAdd(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = sourceAdd(args, strings.NewReader(stdin), &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

// runSourceUpdate is runSourceAdd for `source update`.
func runSourceUpdate(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = sourceUpdate(args, strings.NewReader(stdin), &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

// --- source add ---

func TestSourceAdd_NoWait(t *testing.T) {
	var rec recorder
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","step":1,"protection":"none","last_check_error":null}`))
	})

	code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}

	method, path, query, body := rec.get()
	if method != http.MethodPost || path != "/api/v1/sources" {
		t.Fatalf("request = %s %s, want POST /api/v1/sources", method, path)
	}
	if query != "" {
		t.Fatalf("query string = %q — the connection URL must never ride in a URL", query)
	}
	if got := rec.bodyField(t, "connection_url"); got != testConnURL {
		t.Fatalf("body connection_url = %q, want the URL verbatim", got)
	}
	if got := rec.bodyField(t, "name"); got != "production" {
		t.Fatalf("body name = %q", got)
	}
	if !strings.Contains(string(body), testConnURL) {
		t.Fatalf("body should carry the URL: %q", body)
	}

	for _, want := range []string{
		"✓ Production database production added",
		"→ Checking connection (read-only, nothing is copied)",
		"Next:",
		"pgrun source status production",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceAdd_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","extra_field":"kept-verbatim"}`))
	})
	code, out, _ := run(t, "source", "add", "--name", "production", "--url", testConnURL, "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw body verbatim: %q", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v (%q)", err, out)
	}
	assertNoLeak(t, "stdout", out)
}

// TestSourceAdd_HiddenPrompt: with no --url, the URL is read from stdin
// behind the hidden-input prompt and never echoed back.
func TestSourceAdd_HiddenPrompt(t *testing.T) {
	var rec recorder
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking"}`))
	})

	code, out, stderr := runSourceAdd(t, testConnURL+"\n", "--name", "production")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "Connection URL (input hidden): ") {
		t.Fatalf("stdout missing the hidden prompt: %q", out)
	}
	if got := rec.bodyField(t, "connection_url"); got != testConnURL {
		t.Fatalf("body connection_url = %q, want the prompted URL", got)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

// TestSourceAdd_EmptyPromptIsUsage: pressing enter at the prompt is a usage
// error, not an empty API call.
func TestSourceAdd_EmptyPromptIsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := runSourceAdd(t, "\n", "--name", "production")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "a connection URL is required") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceAdd_JSONWithoutURLIsUsage: prompting would interleave a prompt
// with the JSON stream, so --json without --url is refused outright.
func TestSourceAdd_JSONWithoutURLIsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := runSourceAdd(t, testConnURL+"\n", "--name", "production", "--json")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if strings.Contains(out, "Connection URL") {
		t.Fatalf("--json must not prompt: %q", out)
	}
	if !strings.Contains(stderr, "--url is required with --json") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceAdd_BadSchemeIsUsage: the client-side scheme check reports the
// rule, never the value the user typed.
func TestSourceAdd_BadSchemeIsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	bad := "mysql://app:SECRETPW@db.example.com/prod"
	code, _, stderr := run(t, "source", "add", "--name", "production", "--url", bad)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "must start with postgres:// or postgresql://") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stderr, bad) || strings.Contains(stderr, testPassword) {
		t.Fatalf("usage error echoed the value: %q", stderr)
	}
}

// TestSourceAdd_PostgresqlSchemeAccepted: both spellings are legal.
func TestSourceAdd_PostgresqlSchemeAccepted(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking"}`))
	})
	code, _, stderr := run(t, "source", "add", "--name", "production", "--url", "postgresql://app:pw@h/db")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
}

func TestSourceAdd_MissingNameIsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "add", "--url", testConnURL)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--name is required") {
		t.Fatalf("stderr = %q", stderr)
	}
	assertNoLeak(t, "stderr", stderr)
}

// TestSourceAdd_BareURLPositionalIsRedacted: a URL typed without --url is a
// usage error whose message must not repeat what was typed.
func TestSourceAdd_BareURLPositionalIsRedacted(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "add", "--name", "production", testConnURL)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	assertNoLeak(t, "stderr", stderr)
}

// TestSourceAdd_URLAsNameIsRefused: a URL handed to --name would travel as
// a name — into the request body and the server's logs — outside the
// redactor this command builds from --url. Refused before any request.
func TestSourceAdd_URLAsNameIsRefused(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "add", "--name", testConnURL, "--url", testConnURL)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "not a connection URL") {
		t.Fatalf("stderr = %q", stderr)
	}
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceAdd_Wait_ConnectedSequence(t *testing.T) {
	withFastPoll(t)
	var gets int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","step":1,"protection":"none","last_check_error":null}`))
		case http.MethodGet:
			if atomic.AddInt32(&gets, 1) < 2 {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"name":"production","safe_copy_status":"checking","step":1,"protection":"none","last_check_error":null}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"production","safe_copy_status":"protect","step":2,"postgres_version":"17","protection":"draft","unresolved_count":2,"tables":12,"sensitive_columns":4}`))
		}
	})

	code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL, "--wait", "--timeout", "5s")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"✓ Production database production added",
		"Production Database: production",
		"✓ Connected, Postgres 17",
		"✓ Schema analyzed (12 tables, 4 potentially sensitive columns)",
		"Next:",
		"pgrun source protect production",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceAdd_Wait_ConnectionFailed(t *testing.T) {
	withFastPoll(t)
	var gets int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
		case http.MethodGet:
			if atomic.AddInt32(&gets, 1) < 2 {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":"PG::ConnectionBad"}`))
		}
	})

	code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL, "--wait", "--timeout", "5s")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "connection failed (PG::ConnectionBad)") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, `pgrun source update production --url "$DATABASE_URL"`) {
		t.Fatalf("stderr missing the retry hint: %q", stderr)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceAdd_Wait_Timeout(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
		}
	})
	code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL, "--wait", "--timeout", "10ms")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "still checking production after 10ms") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "pgrun source status production") {
		t.Fatalf("stderr missing the follow-up command: %q", stderr)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

// TestSourceAdd_HostileServer_Redacts is the structural-redaction case: an
// API that echoes the connection URL back in its error body must not be
// able to print it through the CLI — in either output mode.
func TestSourceAdd_HostileServer_Redacts(t *testing.T) {
	hostile := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"rejected ` + testConnURL + `"}`))
	}

	t.Run("human", func(t *testing.T) {
		withServer(t, hostile)
		code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL)
		if code != exitFailure {
			t.Fatalf("code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr, "[redacted]") {
			t.Fatalf("stderr should show the redaction marker: %q", stderr)
		}
		assertNoLeak(t, "stdout", out)
		assertNoLeak(t, "stderr", stderr)
	})

	t.Run("json", func(t *testing.T) {
		withServer(t, hostile)
		code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL, "--json")
		if code != exitFailure {
			t.Fatalf("code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(out, "[redacted]") {
			t.Fatalf("--json stdout should show the redaction marker: %q", out)
		}
		assertNoLeak(t, "stdout", out)
		assertNoLeak(t, "stderr", stderr)
	})
}

func TestSourceAdd_401ExitsAuth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	code, out, stderr := run(t, "source", "add", "--name", "production", "--url", testConnURL)
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr missing hint: %q", stderr)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

// --- source update ---

func TestSourceUpdate_NoWait(t *testing.T) {
	var rec recorder
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
	})

	code, out, stderr := run(t, "source", "update", "production", "--url", testConnURL)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	method, path, query, _ := rec.get()
	if method != http.MethodPatch || path != "/api/v1/sources/production" {
		t.Fatalf("request = %s %s, want PATCH /api/v1/sources/production", method, path)
	}
	if query != "" {
		t.Fatalf("query string = %q — the connection URL must never ride in a URL", query)
	}
	if got := rec.bodyField(t, "connection_url"); got != testConnURL {
		t.Fatalf("body connection_url = %q", got)
	}
	if !strings.Contains(out, "✓ Connection URL updated for production") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(out, "→ Checking connection (read-only, nothing is copied)") {
		t.Fatalf("stdout = %q", out)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceUpdate_HiddenPrompt(t *testing.T) {
	var rec recorder
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking"}`))
	})
	code, out, stderr := runSourceUpdate(t, testConnURL+"\n", "production")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "Connection URL (input hidden): ") {
		t.Fatalf("stdout missing the hidden prompt: %q", out)
	}
	if got := rec.bodyField(t, "connection_url"); got != testConnURL {
		t.Fatalf("body connection_url = %q", got)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceUpdate_409ExitsFailure(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"this database is already connected"}`))
	})
	code, out, stderr := run(t, "source", "update", "production", "--url", testConnURL)
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "this database is already connected") {
		t.Fatalf("stderr = %q", stderr)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceUpdate_ProjectFallback(t *testing.T) {
	var rec recorder
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"jobsgpt","safe_copy_status":"checking"}`))
	})
	dir := t.TempDir()
	chdir(t, dir)
	if _, err := config.SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	code, out, stderr := run(t, "source", "update", "--url", testConnURL)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if _, path, _, _ := rec.get(); path != "/api/v1/sources/jobsgpt" {
		t.Fatalf("path = %q, want the .pgrun/project fallback name", path)
	}
	if !strings.Contains(out, "✓ Connection URL updated for jobsgpt") {
		t.Fatalf("stdout = %q", out)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

func TestSourceUpdate_Wait_ConnectionFailed(t *testing.T) {
	withFastPoll(t)
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":"PG::ConnectionBad"}`))
		}
	})
	code, out, stderr := run(t, "source", "update", "production", "--url", testConnURL, "--wait", "--timeout", "5s")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "connection failed (PG::ConnectionBad)") {
		t.Fatalf("stderr = %q", stderr)
	}
	assertNoLeak(t, "stdout", out)
	assertNoLeak(t, "stderr", stderr)
}

// TestSourceUpdate_BareURLPositionalIsRedacted: `pgrun source update <url>`
// (URL where the name goes) must not echo the URL back either — the shared
// [<name>] parser is a print site too.
func TestSourceUpdate_BareURLPositionalIsRedacted(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "update", "production", testConnURL)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	assertNoLeak(t, "stderr", stderr)
}

// TestSourceUpdate_URLAsNameIsRefused: `pgrun source update <url>` (the URL
// where the name goes) must never become a request path — that would put
// the secret in the API's access log — and the refusal must not echo it.
func TestSourceUpdate_URLAsNameIsRefused(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "update", testConnURL)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "not a connection URL") {
		t.Fatalf("stderr = %q", stderr)
	}
	assertNoLeak(t, "stderr", stderr)
}

// --- dispatch ---

// TestSourceAddUpdate_AreDispatched guards against the Task 2 placeholders
// being left in place for these two subcommands.
func TestSourceAddUpdate_AreDispatched(t *testing.T) {
	isolateHome(t)
	for _, args := range [][]string{
		{"source", "add"},
		{"source", "update", "production", "--url", "bogus://x"},
	} {
		code, _, stderr := run(t, args...)
		if code != exitUsage {
			t.Errorf("%v: code = %d, want %d", args, code, exitUsage)
		}
		if strings.Contains(stderr, "not implemented yet") {
			t.Errorf("%v: still the placeholder: %q", args, stderr)
		}
	}
}
