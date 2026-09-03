package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgrundev/pgrun/internal/api"
)

// run is the test harness: it drives cli.Run and returns exit code, stdout,
// stderr as strings.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = Run(args, &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

// isolateHome points $HOME (and clears the env-level config vars) at a fresh
// temp dir for the duration of one test, so ~/.config/pgrun/config.json
// never touches the real filesystem and tests don't see each other's state.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PGRUN_API_URL", "")
	t.Setenv("PGRUN_API_TOKEN", "")
	return dir
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.HasPrefix(out, "pgrun ") {
		t.Fatalf("out = %q", out)
	}
}

func TestNoArgsIsUsage(t *testing.T) {
	code, _, stderr := run(t)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestUnknownCommandIsUsage(t *testing.T) {
	code, _, _ := run(t, "bogus")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

// --- usage error cases (exit 64) ---

func TestUsageErrors(t *testing.T) {
	isolateHome(t)
	cases := [][]string{
		{"branch"},                   // missing subcommand
		{"branch", "bogus"},          // unknown subcommand
		{"branch", "create"},         // missing project
		{"branch", "create", "proj"}, // missing --name
		{"branch", "create", "proj", "--name", "x", "--ttl", "3h"}, // bad ttl
		{"branch", "get", "proj"},                                  // missing name
		{"branch", "url", "proj"},                                  // missing name
		{"branch", "delete", "proj"},                               // missing name
		{"auth"},                                                   // missing subcommand
		{"auth", "set"},                                            // missing --token
		{"auth", "bogus"},                                          // unknown subcommand
		{"mcp"},                                                    // missing "serve"
		{"mcp", "bogus"},                                           // not "serve"
	}
	for _, args := range cases {
		code, _, stderr := run(t, args...)
		if code != exitUsage {
			t.Errorf("args=%v: code = %d, want %d (stderr=%q)", args, code, exitUsage, stderr)
		}
	}
}

func TestUnexpectedTrailingArgIsUsage(t *testing.T) {
	isolateHome(t)
	code, _, _ := run(t, "branch", "list", "proj", "extra")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

// --- auth set / status ---

func TestAuthSetAndStatus(t *testing.T) {
	dir := isolateHome(t)

	code, out, _ := run(t, "auth", "set", "--token", "abcdef123456", "--url", "https://api.example.com")
	if code != exitSuccess {
		t.Fatalf("auth set code = %d", code)
	}
	if strings.Contains(out, "abcdef123456") {
		t.Fatalf("auth set stdout leaked the token: %q", out)
	}

	path := filepath.Join(dir, ".config", "pgrun", "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file mode = %o, want 0600", perm)
	}

	code, out, stderr := run(t, "auth", "status")
	if code != exitSuccess {
		t.Fatalf("auth status code = %d, stderr=%q", code, stderr)
	}
	if strings.Contains(out, "abcdef123456") {
		t.Fatalf("auth status leaked the full token: %q", out)
	}
	if !strings.Contains(out, "abcdef…") {
		t.Fatalf("auth status missing fingerprint: %q", out)
	}
	if !strings.Contains(out, "https://api.example.com") {
		t.Fatalf("auth status missing url: %q", out)
	}
}

func TestAuthStatusNotConfigured(t *testing.T) {
	isolateHome(t)
	code, out, stderr := run(t, "auth", "status")
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr missing hint: %q", stderr)
	}
}

func TestAuthSetPreservesURLWhenOmitted(t *testing.T) {
	isolateHome(t)
	if code, _, _ := run(t, "auth", "set", "--token", "tok1", "--url", "https://first.example.com"); code != exitSuccess {
		t.Fatalf("first auth set failed")
	}
	if code, _, _ := run(t, "auth", "set", "--token", "tok2"); code != exitSuccess {
		t.Fatalf("second auth set failed")
	}
	_, out, _ := run(t, "auth", "status")
	if !strings.Contains(out, "https://first.example.com") {
		t.Fatalf("url was not preserved: %q", out)
	}
}

// --- config precedence, exercised through the CLI ---

func TestConfigPrecedenceThroughCLI(t *testing.T) {
	isolateHome(t)
	run(t, "auth", "set", "--token", "file-token", "--url", "https://file.example.com")

	// File layer.
	_, out, _ := run(t, "auth", "status")
	if !strings.Contains(out, "https://file.example.com") {
		t.Fatalf("file layer: %q", out)
	}

	// Env overrides file.
	t.Setenv("PGRUN_API_URL", "https://env.example.com")
	_, out, _ = run(t, "auth", "status")
	if !strings.Contains(out, "https://env.example.com") {
		t.Fatalf("env layer: %q", out)
	}

	// Flag overrides env and file.
	_, out, _ = run(t, "auth", "status", "--url", "https://flag.example.com")
	if !strings.Contains(out, "https://flag.example.com") {
		t.Fatalf("flag layer: %q", out)
	}
}

func TestMissingConfigExitsAuth(t *testing.T) {
	isolateHome(t)
	code, _, stderr := run(t, "branch", "list", "proj1")
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr missing hint: %q", stderr)
	}
}

// --- branch commands against an httptest API ---

// withServer starts an httptest server and points PGRUN_API_URL/TOKEN at it
// so branch commands resolve without needing a config file.
func withServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	isolateHome(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("PGRUN_API_URL", srv.URL)
	t.Setenv("PGRUN_API_TOKEN", "test-token")
}

func TestBranchCreate_NoWait(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
	})
	code, out, _ := run(t, "branch", "create", "proj1", "--name", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "feature-x") || !strings.Contains(out, "creating") {
		t.Fatalf("out = %q", out)
	}
	if strings.Contains(out, "DATABASE_URL") {
		t.Fatalf("no --wait must never print DATABASE_URL: %q", out)
	}
}

func TestBranchCreate_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating","extra_field":"kept-verbatim"}`))
	})
	code, out, _ := run(t, "branch", "create", "proj1", "--name", "feature-x", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw body verbatim: %q", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

// withFastPoll shrinks the create --wait poll interval for the duration of
// one test, so scripted multi-call sequences don't wait on the real 2s
// production cadence.
func withFastPoll(t *testing.T) {
	t.Helper()
	old := api.PollInterval
	api.PollInterval = time.Millisecond
	t.Cleanup(func() { api.PollInterval = old })
}

func TestBranchCreate_Wait_ReadySequence(t *testing.T) {
	withFastPoll(t)
	var calls int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		case r.Method == http.MethodGet:
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
		}
	})
	code, out, _ := run(t, "branch", "create", "proj1", "--name", "feature-x", "--wait", "--timeout", "5s")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(out) != "DATABASE_URL=postgres://u:p@host/feature-x" {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchCreate_Wait_FailedSequence(t *testing.T) {
	withFastPoll(t)
	var calls int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		case r.Method == http.MethodGet:
			n := atomic.AddInt32(&calls, 1)
			if n < 2 {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"failed"}`))
		}
	})
	code, out, stderr := run(t, "branch", "create", "proj1", "--name", "feature-x", "--wait", "--timeout", "5s")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if strings.Contains(out, "DATABASE_URL") {
		t.Fatalf("failed branch must never print DATABASE_URL: %q", out)
	}
	if !strings.Contains(stderr, "failed") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestBranchCreate_Wait_DeletedSequence is F1's scripted-poll case: a branch
// observed as "deleted" mid-wait must stop the loop immediately (not run
// out the --timeout), exit 1, and name the reason — never DATABASE_URL.
func TestBranchCreate_Wait_DeletedSequence(t *testing.T) {
	withFastPoll(t)
	var calls int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		case r.Method == http.MethodGet:
			n := atomic.AddInt32(&calls, 1)
			if n < 2 {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"deleted"}`))
		}
	})
	start := time.Now()
	// A generous 10s timeout deliberately — if this regresses to the
	// timeout path instead of stopping on "deleted", the test goes slow
	// rather than silently passing.
	code, out, stderr := run(t, "branch", "create", "proj1", "--name", "feature-x", "--wait", "--timeout", "10s")
	elapsed := time.Since(start)

	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if strings.Contains(out, "DATABASE_URL") {
		t.Fatalf("deleted branch must never print DATABASE_URL: %q", out)
	}
	if !strings.Contains(stderr, "deleted") {
		t.Fatalf("stderr should name the deletion: %q", stderr)
	}
	if strings.Contains(stderr, "timed out") {
		t.Fatalf("must stop on the terminal status, not the timeout: %q", stderr)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s — looks like it waited out the 10s timeout instead of stopping on \"deleted\"", elapsed)
	}
}

// TestBranchCreate_Wait_JSON_FailedExitsFailure locks in that --json still
// reflects the real outcome in its exit code — --json changes how the
// result is reported, never whether it's a failure.
func TestBranchCreate_Wait_JSON_FailedExitsFailure(t *testing.T) {
	withFastPoll(t)
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"failed"}`))
		}
	})
	code, out, _ := run(t, "branch", "create", "proj1", "--name", "feature-x", "--wait", "--timeout", "5s", "--json")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d — --json must not mask a failed outcome", code, exitFailure)
	}
	if !strings.Contains(out, `"status":"failed"`) {
		t.Fatalf("--json should still dump the raw failed body: %q", out)
	}
}

func TestBranchCreate_Wait_Timeout(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		}
	})
	code, out, stderr := run(t, "branch", "create", "proj1", "--name", "feature-x", "--wait", "--timeout", "10ms")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if strings.Contains(out, "DATABASE_URL") {
		t.Fatalf("timeout must never print DATABASE_URL: %q", out)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBranchCreate_BadTTLIsUsageError(t *testing.T) {
	isolateHome(t)
	code, _, stderr := run(t, "branch", "create", "proj1", "--name", "x", "--ttl", "bogus")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--ttl") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestBranchCreate_InvalidProjectName_ExitsUsage is F3's CLI-level case: a
// project name of ".." must never reach the network, and the CLI reports it
// as a usage error (64), not an operation failure.
func TestBranchCreate_InvalidProjectName_ExitsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "branch", "create", "..", "--name", "x")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, `".."`) {
		t.Fatalf("stderr should name the bad value: %q", stderr)
	}
}

// TestBranchGet_InvalidBranchName_ExitsUsage covers the "name" path segment
// (distinct from "project"), which only GetBranch/DeleteBranch have.
func TestBranchGet_InvalidBranchName_ExitsUsage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "branch", "get", "proj1", "..")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, `".."`) {
		t.Fatalf("stderr should name the bad value: %q", stderr)
	}
}

func TestBranchCreate_401ExitsAuth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	code, _, stderr := run(t, "branch", "create", "proj1", "--name", "x")
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if strings.Contains(stderr, "test-token") {
		t.Fatalf("stderr leaked token: %q", stderr)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr missing hint: %q", stderr)
	}
}

func TestBranchCreate_409ExitsFailure(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"branch already exists"}`))
	})
	code, _, stderr := run(t, "branch", "create", "proj1", "--name", "x")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "branch already exists") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestBranchCreate_Wait_JSON_IncludesDatabaseURL is item 4's CLI half: a
// --wait --json create must carry a "database_url" field once ready, in
// addition to whatever the server already sent (including connection_url),
// so an agent scripting against --json never needs a second call.
func TestBranchCreate_Wait_JSON_IncludesDatabaseURL(t *testing.T) {
	withFastPoll(t)
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
		}
	})
	code, out, _ := run(t, "branch", "create", "proj1", "--name", "feature-x", "--wait", "--timeout", "5s", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if decoded["database_url"] != "postgres://u:p@host/feature-x" {
		t.Fatalf("database_url = %v, want the connection_url value: %s", decoded["database_url"], out)
	}
	if decoded["id"] != "b1" || decoded["name"] != "feature-x" || decoded["status"] != "ready" {
		t.Fatalf("--json should still carry the original fields: %s", out)
	}
	// "ready": true rides along with database_url — one boolean an agent can
	// check instead of comparing status strings.
	if decoded["ready"] != true {
		t.Fatalf("ready = %v, want true: %s", decoded["ready"], out)
	}
}

func TestBranchList_Table(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"branches":[` +
			`{"id":"branch_1","name":"main","status":"ready","is_base":true,"parent_branch_id":null,"postgres_version":"17","created_at":"2026-08-01T00:00:00Z","expires_at":null},` +
			`{"id":"branch_2","name":"feature-x","status":"creating","is_base":false,"parent_branch_id":"branch_1","postgres_version":"17","created_at":"2026-08-29T00:00:00Z","expires_at":"2026-08-30T00:00:00Z"}` +
			`]}`))
	})
	code, out, _ := run(t, "branch", "list", "proj1")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"NAME", "STATUS", "BASE", "PARENT", "VERSION", "CREATED", "EXPIRES",
		"main", "feature-x", "yes", "branch_1", "17",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q: %q", want, out)
		}
	}
	// main is_base:true -> BASE column "yes"; feature-x is_base:false -> "-".
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 rows, got %d lines: %q", len(lines), out)
	}
	if !strings.Contains(lines[2], "-") {
		t.Fatalf("expected a dash for feature-x's non-base BASE column: %q", lines[2])
	}
}

func TestBranchGet_NeverPrintsConnectionURL(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://secret@host/db"}`))
	})
	code, out, _ := run(t, "branch", "get", "proj1", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("branch get leaked connection_url in human mode: %q", out)
	}
	if !strings.Contains(out, "feature-x") || !strings.Contains(out, "ready") {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchGet_JSONIncludesConnectionURL(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://secret@host/db"}`))
	})
	code, out, _ := run(t, "branch", "get", "proj1", "feature-x", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "postgres://secret@host/db") {
		t.Fatalf("--json should pass connection_url through verbatim: %q", out)
	}
}

func TestBranchStatusIsAliasForGet(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready"}`))
	})
	code, out, _ := run(t, "branch", "status", "proj1", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "feature-x") {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchURL_Ready(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	code, out, _ := run(t, "branch", "url", "proj1", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(out) != "DATABASE_URL=postgres://u:p@host/feature-x" {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchURL_NotReady(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
	})
	code, out, stderr := run(t, "branch", "url", "proj1", "feature-x")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(stderr, "not ready") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBranchDelete_Accepted(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"deleting"}`))
	})
	code, out, _ := run(t, "branch", "delete", "proj1", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "deleting") {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchDelete_409OnBase(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"cannot delete a branch with children"}`))
	})
	code, _, stderr := run(t, "branch", "delete", "proj1", "main")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "cannot delete a branch with children") {
		t.Fatalf("stderr = %q", stderr)
	}
}
