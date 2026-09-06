// Tests for `pgrun source copy`: the server-enforced pre-flight checklist,
// the four refusal messages, the POST, and --wait to a settled Safe Copy.
// See .superpowers/sdd/2026-09-06-pgrun-source-cli/task-5-brief.md for the
// exact flow, messages, and exit codes these lock in.

package cli

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// --- pre-flight refusals: no POST is ever made ---

func TestSourceCopy_DriftRefusesWithoutPost(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == getPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"action_required","step":4,"protection":"active","policy_version":2}`))
			return
		}
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := run(t, "source", "copy", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (out=%q)", code, exitFailure, out)
	}
	for _, want := range []string{
		"pgrun: Safe Copy cannot be created.",
		"Production schema changed after the protection policy was approved.",
		"Review changes:",
		"  pgrun source protect acme",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q: %q", want, stderr)
		}
	}
}

// TestSourceCopy_DriftJSON: --json on the pre-flight refusal dumps the GET's
// raw body before the stderr message (per the task's clarified rule).
func TestSourceCopy_DriftJSON(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == getPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"action_required","extra_field":"kept-verbatim"}`))
			return
		}
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, out, stderr := run(t, "source", "copy", "acme", "--json")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json should dump the GET's raw body: %q", out)
	}
	if !strings.Contains(stderr, "Safe Copy cannot be created.") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSourceCopy_AlreadyReady(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == getPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"ready"}`))
			return
		}
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "copy", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "pgrun: acme already has a Safe Copy") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSourceCopy_PriorFailure(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == getPath {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"failed"}`))
			return
		}
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	code, _, stderr := run(t, "source", "copy", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "pgrun: the Safe Copy for acme failed — write to support@postgresrun.com") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceCopy_NotProtectedYetRefusesWithoutPost covers connect/checking/
// protect: all three refuse before any POST, with sourceNext's own hint.
func TestSourceCopy_NotProtectedYetRefusesWithoutPost(t *testing.T) {
	cases := []struct {
		status string
		next   string
	}{
		{"connect", `pgrun source update acme --url "$DATABASE_URL"`},
		{"checking", "pgrun source status acme"},
		{"protect", "pgrun source protect acme"},
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
			code, _, stderr := run(t, "source", "copy", "acme")
			if code != exitFailure {
				t.Fatalf("code = %d, want %d", code, exitFailure)
			}
			if !strings.Contains(stderr, "pgrun: finish data protection first — "+tc.next) {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

// --- ready_to_copy: checklist, POST, no --wait ---

func TestSourceCopy_ReadyToCopy_NoWait(t *testing.T) {
	var rec recorder
	getPath := "/api/v1/sources/acme"
	postPath := getPath + "/copy"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == getPath:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":2}`))
		case r.Method == http.MethodPost && r.URL.Path == postPath:
			rec.record(r)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"copying","policy_version":2}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	code, out, stderr := run(t, "source", "copy", "acme")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	method, path, _, _ := rec.get()
	if method != http.MethodPost || path != postPath {
		t.Fatalf("copy request = %s %s, want POST %s", method, path, postPath)
	}
	for _, want := range []string{
		"✓ Connection verified",
		"✓ Data protection active (policy v2)",
		"✓ Schema unchanged",
		"→ Creating Safe Copy",
		"Next:",
		"pgrun source status acme",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
}

// TestSourceCopy_ReadyToCopy_JSON: --json prints the POST's raw body
// verbatim, with none of the human headlines mixed in.
func TestSourceCopy_ReadyToCopy_JSON(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	postPath := getPath + "/copy"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == getPath:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":2}`))
		case r.Method == http.MethodPost && r.URL.Path == postPath:
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"copying","extra_field":"kept-verbatim"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	code, out, stderr := run(t, "source", "copy", "acme", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw POST body verbatim: %q", out)
	}
	if strings.Contains(out, "Creating Safe Copy") || strings.Contains(out, "Connection verified") {
		t.Fatalf("--json must not also print human headlines: %q", out)
	}
}

// --- server refusal on the POST itself ---

func TestSourceCopy_POST422(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	postPath := getPath + "/copy"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == getPath:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":2}`))
		case r.Method == http.MethodPost && r.URL.Path == postPath:
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"error":"production schema changed — review protect before creating or re-creating the Safe Copy"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	code, _, stderr := run(t, "source", "copy", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "production schema changed — review protect before creating or re-creating the Safe Copy") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSourceCopy_POST409(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	postPath := getPath + "/copy"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == getPath:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"ready_to_copy","policy_version":2}`))
		case r.Method == http.MethodPost && r.URL.Path == postPath:
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"this database already has a Safe Copy"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	code, _, stderr := run(t, "source", "copy", "acme")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "this database already has a Safe Copy") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- copying: informational, no --wait ---

func TestSourceCopy_CopyingNoWait(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"copying"}`))
	})
	code, out, stderr := run(t, "source", "copy", "acme")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"→ Safe Copy for acme is being created",
		"Next:",
		"pgrun source status acme",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
}

func TestSourceCopy_CopyingNoWait_JSON(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"copying","extra_field":"kept-verbatim"}`))
	})
	code, out, stderr := run(t, "source", "copy", "acme", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw GET body verbatim: %q", out)
	}
}

// --- --wait sequences ---

func TestSourceCopy_Wait_ReadySequence(t *testing.T) {
	withFastPoll(t)
	var gets int32
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request (no POST expected when already copying): %s %s", r.Method, r.URL.Path)
		}
		n := atomic.AddInt32(&gets, 1)
		if n < 3 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"copying"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"ready"}`))
	})
	code, out, stderr := run(t, "source", "copy", "acme", "--wait", "--timeout", "5s")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"✓ Safe Copy ready",
		"Next:",
		"pgrun branch create acme --name dev --wait",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
}

// TestSourceCopy_Wait_ReadySequence_JSON: --wait --json prints only the
// final raw body once the Safe Copy is ready.
func TestSourceCopy_Wait_ReadySequence_JSON(t *testing.T) {
	withFastPoll(t)
	var gets int32
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		n := atomic.AddInt32(&gets, 1)
		if n < 2 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"copying"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"ready","extra_field":"kept-verbatim"}`))
	})
	code, out, stderr := run(t, "source", "copy", "acme", "--wait", "--timeout", "5s", "--json")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json must print the raw body verbatim: %q", out)
	}
	if strings.Contains(out, "Safe Copy ready") {
		t.Fatalf("--json must not also print the human headline: %q", out)
	}
}

func TestSourceCopy_Wait_Failed(t *testing.T) {
	withFastPoll(t)
	var gets int32
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		n := atomic.AddInt32(&gets, 1)
		if n < 2 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"copying"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"failed"}`))
	})
	code, _, stderr := run(t, "source", "copy", "acme", "--wait", "--timeout", "5s")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "pgrun: ✗ Safe Copy failed — write to support@postgresrun.com") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceCopy_Wait_EndsInActionRequired covers the "anything else
// terminal" branch — a schema change lands mid-copy.
func TestSourceCopy_Wait_EndsInActionRequired(t *testing.T) {
	withFastPoll(t)
	var gets int32
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		n := atomic.AddInt32(&gets, 1)
		if n < 2 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"acme","safe_copy_status":"copying"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"action_required"}`))
	})
	code, _, stderr := run(t, "source", "copy", "acme", "--wait", "--timeout", "5s")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "pgrun: Safe Copy ended in action_required — run `pgrun source status acme`") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSourceCopy_Wait_Timeout(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"copying"}`))
	})
	code, _, stderr := run(t, "source", "copy", "acme", "--wait", "--timeout", "10ms")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "pgrun: still creating the Safe Copy for acme after 10ms — run `pgrun source status acme`") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestSourceCopy_Wait_Timeout_JSON: --json on the deadline path dumps the
// last raw body before the stderr message, same as branch create --wait.
func TestSourceCopy_Wait_Timeout_JSON(t *testing.T) {
	getPath := "/api/v1/sources/acme"
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != getPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"acme","safe_copy_status":"copying","extra_field":"kept-verbatim"}`))
	})
	code, out, stderr := run(t, "source", "copy", "acme", "--wait", "--timeout", "10ms", "--json")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out, "extra_field") || !strings.Contains(out, "kept-verbatim") {
		t.Fatalf("--json should dump the last raw body: %q", out)
	}
	if !strings.Contains(stderr, "still creating the Safe Copy for acme") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- auth ---

func TestSourceCopy_401ExitsAuth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	code, _, stderr := run(t, "source", "copy", "acme")
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr missing hint: %q", stderr)
	}
}

func TestSourceCopy_404ExitsFailureWithMessage(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"source not found"}`))
	})
	code, _, stderr := run(t, "source", "copy", "bogus")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "source not found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --- dispatch ---

// TestSourceCopy_IsDispatched guards against the Task 2 placeholder being
// left in place — replaces the dropped canary entry in
// TestSourcePlaceholders_NotImplemented (source_test.go).
func TestSourceCopy_IsDispatched(t *testing.T) {
	isolateHome(t)
	code, _, stderr := run(t, "source", "copy")
	if code == exitUsage && strings.Contains(stderr, "not implemented yet") {
		t.Fatalf("copy is still the Task 2 placeholder: %q", stderr)
	}
}
