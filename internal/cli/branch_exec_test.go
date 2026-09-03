package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMain lets this test binary re-exec itself as a tiny helper process —
// the standard Go idiom (see os/exec's own tests) for driving `branch exec`
// against a *real* child process without depending on any external command
// (no /bin/sh, no coreutils) so this stays portable and dependency-free.
// PGRUN_TEST_HELPER selects the helper behavior; unset, this is a normal
// test run.
func TestMain(m *testing.M) {
	switch os.Getenv("PGRUN_TEST_HELPER") {
	case "":
		os.Exit(m.Run())
	case "echo_database_url":
		// Proves branch exec set DATABASE_URL in the child's *environment*
		// (not argv, not a file) — this helper only ever looks at its env.
		fmt.Fprint(os.Stdout, os.Getenv("DATABASE_URL"))
		os.Exit(0)
	case "fail":
		// A command that always exits nonzero, to exercise --delete-after's
		// "even on command failure" cleanup guarantee and exit-code passthrough.
		os.Exit(7)
	default:
		os.Exit(1)
	}
}

// helperCommand returns a `branch exec ... -- <self>` command line: the
// test binary re-invoked with PGRUN_TEST_HELPER set, so it runs one of the
// TestMain branches above instead of the real test suite.
func helperCommand(t *testing.T, helper string) []string {
	t.Helper()
	t.Setenv("PGRUN_TEST_HELPER", helper)
	return []string{os.Args[0]}
}

func TestBranchExec_ExistingBranch_SetsDatabaseURLInChildEnv(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	self := helperCommand(t, "echo_database_url")

	args := append([]string{"branch", "exec", "proj1", "feature-x", "--"}, self...)
	code, out, stderr := run(t, args...)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if out != "postgres://u:p@host/feature-x" {
		t.Fatalf("child did not see DATABASE_URL via its environment: out = %q", out)
	}
}

func TestBranchExec_ExistingBranch_PropagatesChildExitCode(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	self := helperCommand(t, "fail")

	args := append([]string{"branch", "exec", "proj1", "feature-x", "--"}, self...)
	code, _, _ := run(t, args...)
	if code != 7 {
		t.Fatalf("code = %d, want 7 (the child's own exit code)", code)
	}
}

func TestBranchExec_NotReady_ExitsFailureWithoutRunningCommand(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
	})
	// A command that would fail the test if it ran (it's not a real binary),
	// proving branch exec never gets as far as exec'ing it.
	code, out, stderr := run(t, "branch", "exec", "proj1", "feature-x", "--", "/does/not/exist/pgrun-test-sentinel")
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

func TestBranchExec_Create_DeleteAfter_DeletesEvenOnCommandFailure(t *testing.T) {
	withFastPoll(t)
	var deleteCalls int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"agent-x","status":"creating"}`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"agent-x","status":"ready","connection_url":"postgres://u:p@host/agent-x"}`))
		case r.Method == http.MethodDelete:
			atomic.AddInt32(&deleteCalls, 1)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		}
	})
	self := helperCommand(t, "fail")

	args := append([]string{
		"branch", "exec", "proj1", "--create", "agent-x", "--ttl", "1h", "--delete-after", "--timeout", "5s", "--",
	}, self...)
	code, _, stderr := run(t, args...)
	if code != 7 {
		t.Fatalf("code = %d, want 7 (the failing child's own exit code), stderr=%q", code, stderr)
	}
	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("DELETE calls = %d, want 1 — --delete-after must clean up even when the command fails", got)
	}
}

func TestBranchExec_Create_DeleteAfter_DeletesOnSuccessToo(t *testing.T) {
	withFastPoll(t)
	var deleteCalls int32
	var gotDeleteName string
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"agent-x","status":"creating"}`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"agent-x","status":"ready","connection_url":"postgres://u:p@host/agent-x"}`))
		case r.Method == http.MethodDelete:
			atomic.AddInt32(&deleteCalls, 1)
			gotDeleteName = strings.TrimPrefix(r.URL.Path, "/api/v1/projects/proj1/branches/")
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		}
	})
	self := helperCommand(t, "echo_database_url")

	args := append([]string{
		"branch", "exec", "proj1", "--create", "agent-x", "--delete-after", "--timeout", "5s", "--",
	}, self...)
	code, out, stderr := run(t, args...)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if out != "postgres://u:p@host/agent-x" {
		t.Fatalf("out = %q", out)
	}
	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("DELETE calls = %d, want 1", got)
	}
	if gotDeleteName != "agent-x" {
		t.Fatalf("DELETE was for %q, want %q", gotDeleteName, "agent-x")
	}
}

func TestBranchExec_UsageErrors(t *testing.T) {
	isolateHome(t)
	cases := [][]string{
		{"branch", "exec"},          // missing project
		{"branch", "exec", "proj1"}, // no name, no --create
		{"branch", "exec", "proj1", "name", "--create", "other"},                 // both name and --create
		{"branch", "exec", "proj1", "name", "--"},                                // missing command
		{"branch", "exec", "proj1", "--create", "n", "--"},                       // missing command
		{"branch", "exec", "proj1", "name", "--ttl", "1h", "--", "cmd"},          // --ttl without --create
		{"branch", "exec", "proj1", "--create", "n", "--ttl", "3h", "--", "cmd"}, // bad ttl
	}
	for _, args := range cases {
		code, _, stderr := run(t, args...)
		if code != exitUsage {
			t.Errorf("args=%v: code = %d, want %d (stderr=%q)", args, code, exitUsage, stderr)
		}
	}
}
