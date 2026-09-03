package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBranchEnv_Ready(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	code, out, _ := run(t, "branch", "env", "proj1", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	want := `export DATABASE_URL="postgres://u:p@host/feature-x"` + "\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestBranchEnv_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	code, out, _ := run(t, "branch", "env", "proj1", "feature-x", "--format=json")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(decoded) != 1 || decoded["database_url"] != "postgres://u:p@host/feature-x" {
		t.Fatalf("decoded = %+v, want exactly {database_url: postgres://u:p@host/feature-x}", decoded)
	}
}

func TestBranchEnv_NotReady_ExitsFailure(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
	})
	code, out, stderr := run(t, "branch", "env", "proj1", "feature-x")
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

func TestBranchEnv_ReadyButNotCredentialed_ExitsFailure(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready"}`))
	})
	code, out, stderr := run(t, "branch", "env", "proj1", "feature-x")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(stderr, "not yet credentialed") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBranchEnv_BadFormatIsUsageError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "branch", "env", "proj1", "feature-x", "--format=yaml")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--format") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBranchEnv_MissingArgsIsUsageError(t *testing.T) {
	isolateHome(t)
	for _, args := range [][]string{
		{"branch", "env"},
		{"branch", "env", "proj1"},
	} {
		code, _, _ := run(t, args...)
		if code != exitUsage {
			t.Errorf("args=%v: code = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestShellDoubleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{`postgres://u:p@host/db`, `"postgres://u:p@host/db"`},
		{`has"quote`, `"has\"quote"`},
		{`has$dollar`, `"has\$dollar"`},
		{"has`backtick", "\"has\\`backtick\""},
		{`has\backslash`, `"has\\backslash"`},
	}
	for _, c := range cases {
		if got := shellDoubleQuote(c.in); got != c.want {
			t.Errorf("shellDoubleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
