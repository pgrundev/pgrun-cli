package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- ListSources ---

func TestListSources_Success(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "GET /api/v1/sources" {
			t.Fatalf("unexpected request: %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q", got)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("unexpected query string: %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"sources":[{"name":"production","safe_copy_status":"ready","branches":2},{"name":"staging","safe_copy_status":"connect","branches":0}]}`))
	})

	raw, sources, err := client.ListSources(context.Background())
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 2 || sources[0].Name != "production" || sources[1].Name != "staging" {
		t.Fatalf("unexpected sources: %+v", sources)
	}
	if !strings.Contains(string(raw), `"sources"`) {
		t.Fatalf("raw missing sources envelope: %s", raw)
	}
}

func TestListSources_401(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	_, _, err := client.ListSources(context.Background())
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

// --- GetSource ---

func TestGetSource_FullShape(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "GET /api/v1/sources/production" {
			t.Fatalf("unexpected request: %s", got)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("unexpected query string: %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"ready","step":4,"postgres_version":"16.4",` +
			`"provider":"rds","size_bytes":123456789,"tables":42,"sensitive_columns":7,"unresolved_count":0,` +
			`"protection":"active","policy_version":3,"sync":"snapshot_only","safe_copy_updated_at":"2026-09-05T00:00:00Z",` +
			`"branches":2,"last_check_error":null,"schema_changed":false,"schema_changes":null}`))
	})

	_, src, err := client.GetSource(context.Background(), "production")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	want := Source{
		Name: "production", SafeCopyStatus: "ready", Step: 4, PostgresVersion: "16.4",
		Provider: "rds", SizeBytes: 123456789, Tables: 42, SensitiveColumns: 7, UnresolvedCount: 0,
		Protection: "active", PolicyVersion: 3, Sync: "snapshot_only", SafeCopyUpdatedAt: "2026-09-05T00:00:00Z",
		Branches: 2, LastCheckError: "", SchemaChanged: false, SchemaChanges: nil,
	}
	if src != want {
		t.Fatalf("src = %+v, want %+v", src, want)
	}
}

func TestGetSource_NullPolicyVersionAndCheckError(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","step":1,"postgres_version":"",` +
			`"provider":"","size_bytes":0,"tables":0,"sensitive_columns":0,"unresolved_count":0,` +
			`"protection":"none","policy_version":null,"sync":"","safe_copy_updated_at":"",` +
			`"branches":0,"last_check_error":"PG::ConnectionBad","schema_changed":true,` +
			`"schema_changes":{"added":["t.c"],"removed":[],"retyped":["t2.c2"]}}`))
	})

	_, src, err := client.GetSource(context.Background(), "production")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if src.PolicyVersion != 0 {
		t.Fatalf("PolicyVersion = %d, want 0 (null decode)", src.PolicyVersion)
	}
	if src.LastCheckError != "PG::ConnectionBad" {
		t.Fatalf("LastCheckError = %q", src.LastCheckError)
	}
	if !src.SchemaChanged {
		t.Fatalf("SchemaChanged = false, want true")
	}
	if src.SchemaChanges == nil {
		t.Fatal("SchemaChanges = nil, want populated")
	}
	if len(src.SchemaChanges.Added) != 1 || src.SchemaChanges.Added[0] != "t.c" {
		t.Fatalf("SchemaChanges.Added = %+v", src.SchemaChanges.Added)
	}
	if len(src.SchemaChanges.Removed) != 0 {
		t.Fatalf("SchemaChanges.Removed = %+v, want empty", src.SchemaChanges.Removed)
	}
	if len(src.SchemaChanges.Retyped) != 1 || src.SchemaChanges.Retyped[0] != "t2.c2" {
		t.Fatalf("SchemaChanges.Retyped = %+v", src.SchemaChanges.Retyped)
	}
}

func TestGetSource_404(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"source not found"}`))
	})
	_, _, err := client.GetSource(context.Background(), "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("expected 404 APIError, got %T: %v", err, err)
	}
}

// --- CreateSource ---

func TestCreateSource_Success(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "POST /api/v1/sources" {
			t.Fatalf("unexpected request: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("connection URL leaked into query string: %q", r.URL.RawQuery)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["name"] != "production" {
			t.Fatalf("body[name] = %v", body["name"])
		}
		if body["connection_url"] != "postgres://u:p@host/db" {
			t.Fatalf("body[connection_url] = %v", body["connection_url"])
		}
		if _, hasURL := body["url"]; hasURL {
			t.Fatalf(`body must not have a bare "url" key: %v`, body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","branches":0}`))
	})

	raw, src, err := client.CreateSource(context.Background(), "production", "postgres://u:p@host/db")
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if src.Name != "production" || src.SafeCopyStatus != SourceChecking {
		t.Fatalf("unexpected source: %+v", src)
	}
	if !json.Valid(raw) {
		t.Fatalf("raw not valid JSON: %s", raw)
	}
}

func TestCreateSource_422(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"name has already been taken"}`))
	})
	_, _, err := client.CreateSource(context.Background(), "production", "postgres://u:p@host/db")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 {
		t.Fatalf("expected 422 APIError, got %T: %v", err, err)
	}
	if apiErr.Message != "name has already been taken" {
		t.Fatalf("Message = %q", apiErr.Message)
	}
}

func TestCreateSource_ErrorNeverLeaksConnectionURL(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"could not connect to the given database"}`))
	})
	secretURL := "postgres://sensitive-user:sensitive-pass@evil-host/db"
	_, _, err := client.CreateSource(context.Background(), "production", secretURL)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "sensitive-pass") {
		t.Fatalf("error message leaked the connection URL: %s", err.Error())
	}
}

// --- UpdateSourceURL ---

func TestUpdateSourceURL_Success(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "PATCH /api/v1/sources/production" {
			t.Fatalf("unexpected request: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("connection URL leaked into query string: %q", r.URL.RawQuery)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["connection_url"] != "postgres://u:p@host/db2" {
			t.Fatalf("body[connection_url] = %v", body["connection_url"])
		}
		if _, hasURL := body["url"]; hasURL {
			t.Fatalf(`body must not have a bare "url" key: %v`, body)
		}
		if _, hasName := body["name"]; hasName {
			t.Fatalf("update body must not carry name: %v", body)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","branches":0}`))
	})

	_, src, err := client.UpdateSourceURL(context.Background(), "production", "postgres://u:p@host/db2")
	if err != nil {
		t.Fatalf("UpdateSourceURL: %v", err)
	}
	if src.SafeCopyStatus != SourceChecking {
		t.Fatalf("unexpected source: %+v", src)
	}
}

func TestUpdateSourceURL_409AlreadyConnected(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"this database is already connected"}`))
	})
	_, _, err := client.UpdateSourceURL(context.Background(), "production", "postgres://u:p@host/db2")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 {
		t.Fatalf("expected 409 APIError, got %T: %v", err, err)
	}
	if apiErr.Message != "this database is already connected" {
		t.Fatalf("Message = %q", apiErr.Message)
	}
}

// --- ProtectSource ---

func TestProtectSource_Success(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "POST /api/v1/sources/production/protect" {
			t.Fatalf("unexpected request: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		var body ProtectRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Decisions["users.email"] != "fake" || !body.Approve {
			t.Fatalf("unexpected request body: %+v", body)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"ready_to_copy","policy_version":1,"activated":true,` +
			`"unresolved_count":0,"unresolved":[],"rules":[{"column":"users.email","disposition":"fake","recommended":true,"sensitive":true}],` +
			`"table_rules":[{"table":"users","disposition":"copy_data"}]}`))
	})

	req := ProtectRequest{Decisions: map[string]string{"users.email": "fake"}, Approve: true}
	_, res, err := client.ProtectSource(context.Background(), "production", req)
	if err != nil {
		t.Fatalf("ProtectSource: %v", err)
	}
	if !res.Activated || res.PolicyVersion != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Rules) != 1 || res.Rules[0].Column != "users.email" || res.Rules[0].Disposition != "fake" {
		t.Fatalf("unexpected rules: %+v", res.Rules)
	}
	if len(res.TableRules) != 1 || res.TableRules[0].Table != "users" {
		t.Fatalf("unexpected table rules: %+v", res.TableRules)
	}
}

func TestProtectSource_EmptyRequestStillSendsJSONBody(t *testing.T) {
	var gotContentType string
	var gotBody string
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","safe_copy_status":"protect","policy_version":0,"activated":false,` +
			`"unresolved_count":2,"unresolved":[{"column":"users.ssn","sensitive":true,"valid":["fake","null"]}],"rules":[],"table_rules":[]}`))
	})
	_, res, err := client.ProtectSource(context.Background(), "production", ProtectRequest{})
	if err != nil {
		t.Fatalf("ProtectSource: %v", err)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json even for an empty request", gotContentType)
	}
	if !strings.Contains(gotBody, "{") {
		t.Fatalf("expected a JSON object body, got %q", gotBody)
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0].Column != "users.ssn" || !res.Unresolved[0].Sensitive {
		t.Fatalf("unexpected unresolved: %+v", res.Unresolved)
	}
	if len(res.Unresolved[0].Valid) != 2 {
		t.Fatalf("unexpected valid options: %+v", res.Unresolved[0].Valid)
	}
}

func TestProtectSource_422UnresolvedColumns(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"2 columns still need a decision before the policy can be approved"}`))
	})
	_, _, err := client.ProtectSource(context.Background(), "production", ProtectRequest{Approve: true})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 {
		t.Fatalf("expected 422 APIError, got %T: %v", err, err)
	}
}

// --- ReviewSource ---

func TestReviewSource_Success(t *testing.T) {
	var gotContentType string
	var bodyLen int
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "POST /api/v1/sources/production/review" {
			t.Fatalf("unexpected request: %s", got)
		}
		gotContentType = r.Header.Get("Content-Type")
		b := make([]byte, 16)
		n, _ := r.Body.Read(b)
		bodyLen = n
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"production","unresolved":["users.ssn","users.dob"],"requested":true}`))
	})

	_, res, err := client.ReviewSource(context.Background(), "production")
	if err != nil {
		t.Fatalf("ReviewSource: %v", err)
	}
	if gotContentType != "" {
		t.Fatalf("Content-Type = %q, want empty (no request body)", gotContentType)
	}
	if bodyLen != 0 {
		t.Fatalf("expected an empty request body, read %d bytes", bodyLen)
	}
	if !res.Requested || len(res.Unresolved) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// --- CopySource ---

func TestCopySource_Success(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method + " " + r.URL.Path; got != "POST /api/v1/sources/production/copy" {
			t.Fatalf("unexpected request: %s", got)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"name":"production","safe_copy_status":"copying","branches":0}`))
	})
	_, src, err := client.CopySource(context.Background(), "production")
	if err != nil {
		t.Fatalf("CopySource: %v", err)
	}
	if src.SafeCopyStatus != SourceCopying {
		t.Fatalf("unexpected source: %+v", src)
	}
}

func TestCopySource_409AlreadyHasSafeCopy(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"this database already has a Safe Copy"}`))
	})
	_, _, err := client.CopySource(context.Background(), "production")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 {
		t.Fatalf("expected 409 APIError, got %T: %v", err, err)
	}
}

func TestCopySource_422SchemaChanged(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"production schema changed — review protect before creating or re-creating the Safe Copy"}`))
	})
	_, _, err := client.CopySource(context.Background(), "production")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 {
		t.Fatalf("expected 422 APIError, got %T: %v", err, err)
	}
}

// --- Invalid name / auth mapping shared across every source method ---

func TestSourceMethods_InvalidNameNeverHitsNetwork(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})

	for _, bad := range []string{"", ".", ".."} {
		t.Run("name="+bad, func(t *testing.T) {
			_, _, err := client.GetSource(context.Background(), bad)
			assertInvalidNameError(t, err, "name", bad)

			_, _, err = client.CreateSource(context.Background(), bad, "postgres://u:p@host/db")
			assertInvalidNameError(t, err, "name", bad)

			_, _, err = client.UpdateSourceURL(context.Background(), bad, "postgres://u:p@host/db")
			assertInvalidNameError(t, err, "name", bad)

			_, _, err = client.ProtectSource(context.Background(), bad, ProtectRequest{})
			assertInvalidNameError(t, err, "name", bad)

			_, _, err = client.ReviewSource(context.Background(), bad)
			assertInvalidNameError(t, err, "name", bad)

			_, _, err = client.CopySource(context.Background(), bad)
			assertInvalidNameError(t, err, "name", bad)
		})
	}
}

func TestGetSource_401IsAuthError(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	_, _, err := client.GetSource(context.Background(), "production")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

// --- WaitForSource ---

// withFastPoll shrinks PollInterval for the duration of one test, so a
// scripted multi-call sequence doesn't wait on the real production cadence.
// Mirrors internal/cli's withFastPoll (cli_test.go).
func withFastPoll(t *testing.T) {
	t.Helper()
	old := PollInterval
	PollInterval = time.Millisecond
	t.Cleanup(func() { PollInterval = old })
}

// TestWaitForSource_StopsWhenCheckSettled drives checking (no error) →
// checking (with last_check_error) across two GetSource polls and asserts
// done=CheckSettled stops exactly on the second one, not before and not by
// running out a generous ctx timeout.
func TestWaitForSource_StopsWhenCheckSettled(t *testing.T) {
	withFastPoll(t)

	var calls int32
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		if n < 2 {
			w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":null}`))
			return
		}
		w.Write([]byte(`{"name":"production","safe_copy_status":"checking","last_check_error":"PG::ConnectionBad"}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	initial := Source{Name: "production", SafeCopyStatus: SourceChecking}
	_, src, err := client.WaitForSource(ctx, "production", nil, initial, CheckSettled)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("WaitForSource returned an error: %v", err)
	}
	if !CheckSettled(src) {
		t.Fatalf("returned source is not settled: %+v", src)
	}
	if src.LastCheckError != "PG::ConnectionBad" {
		t.Fatalf("LastCheckError = %q, want PG::ConnectionBad", src.LastCheckError)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("GetSource called %d times, want exactly 2", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("took %s — looks like it ran out the 2s timeout instead of stopping once settled", elapsed)
	}
}

func TestWaitForSource_CtxDeadlineExceeded(t *testing.T) {
	withFastPoll(t)
	PollInterval = 50 * time.Millisecond // longer than the ctx timeout below

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("GetSource should never be called before the context deadline fires")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	initial := Source{Name: "production", SafeCopyStatus: SourceChecking}
	_, _, err := client.WaitForSource(ctx, "production", []byte(`{"safe_copy_status":"checking"}`), initial, CheckSettled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// --- CheckFailed / CheckSettled / CopySettled ---

func TestCheckFailed(t *testing.T) {
	cases := []struct {
		name string
		s    Source
		want bool
	}{
		{"checking no error", Source{SafeCopyStatus: SourceChecking, LastCheckError: ""}, false},
		{"checking with error", Source{SafeCopyStatus: SourceChecking, LastCheckError: "PG::ConnectionBad"}, true},
		{"ready", Source{SafeCopyStatus: SourceReady, LastCheckError: ""}, false},
		{"connect", Source{SafeCopyStatus: SourceConnect}, false},
	}
	for _, c := range cases {
		if got := CheckFailed(c.s); got != c.want {
			t.Errorf("%s: CheckFailed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCheckSettled(t *testing.T) {
	cases := []struct {
		name string
		s    Source
		want bool
	}{
		{"checking no error", Source{SafeCopyStatus: SourceChecking, LastCheckError: ""}, false},
		{"checking with error", Source{SafeCopyStatus: SourceChecking, LastCheckError: "PG::ConnectionBad"}, true},
		{"protect", Source{SafeCopyStatus: SourceProtect}, true},
		{"ready", Source{SafeCopyStatus: SourceReady}, true},
		{"connect", Source{SafeCopyStatus: SourceConnect}, true},
	}
	for _, c := range cases {
		if got := CheckSettled(c.s); got != c.want {
			t.Errorf("%s: CheckSettled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCopySettled(t *testing.T) {
	cases := []struct {
		name string
		s    Source
		want bool
	}{
		{"copying", Source{SafeCopyStatus: SourceCopying}, false},
		{"ready", Source{SafeCopyStatus: SourceReady}, true},
		{"failed", Source{SafeCopyStatus: SourceFailed}, true},
		{"ready_to_copy", Source{SafeCopyStatus: SourceReadyToCopy}, true},
		{"action_required", Source{SafeCopyStatus: SourceActionRequired}, true},
	}
	for _, c := range cases {
		if got := CopySettled(c.s); got != c.want {
			t.Errorf("%s: CopySettled = %v, want %v", c.name, got, c.want)
		}
	}
}
