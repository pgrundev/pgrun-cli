package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer returns a client wired to a fresh httptest server and the
// server itself, so tests can inspect requests via a handler.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token"), srv
}

func TestCreateBranch_Success(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/proj1/branches" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q", got)
		}
		var body CreateBranchRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Name != "feature-x" || body.TTL != "1h" {
			t.Fatalf("unexpected request body: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
	})

	raw, branch, err := client.CreateBranch(context.Background(), "proj1", CreateBranchRequest{Name: "feature-x", TTL: "1h"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if branch.ID != "b1" || branch.Name != "feature-x" || branch.Status != "creating" {
		t.Fatalf("unexpected branch: %+v", branch)
	}
	if !json.Valid(raw) {
		t.Fatalf("raw body is not valid JSON: %s", raw)
	}
}

func TestCreateBranch_422(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"name has already been taken"}`))
	})

	raw, _, err := client.CreateBranch(context.Background(), "proj1", CreateBranchRequest{Name: "dup"})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 422 || apiErr.Message != "name has already been taken" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
	if raw == nil {
		t.Fatal("expected raw body even on error")
	}
}

func TestCreateBranch_401IsAuthError(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})

	_, _, err := client.CreateBranch(context.Background(), "proj1", CreateBranchRequest{Name: "x"})
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
	if authErr.StatusCode != 401 {
		t.Fatalf("StatusCode = %d", authErr.StatusCode)
	}
	// A plain *APIError check must also succeed via Unwrap.
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("AuthError should unwrap to *APIError")
	}
}

func TestListBranches(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/proj1/branches" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"branches":[{"id":"b1","name":"main","status":"ready"},{"id":"b2","name":"feature-x","status":"creating"}]}`))
	})

	raw, branches, err := client.ListBranches(context.Background(), "proj1")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 2 || branches[0].Name != "main" || branches[1].Name != "feature-x" {
		t.Fatalf("unexpected branches: %+v", branches)
	}
	if !strings.Contains(string(raw), `"branches"`) {
		t.Fatalf("raw missing branches key: %s", raw)
	}
}

func TestGetBranch_ReadyIncludesConnectionURL(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/proj1/branches/feature-x" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/db"}`))
	})

	_, branch, err := client.GetBranch(context.Background(), "proj1", "feature-x")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if branch.Status != "ready" || branch.ConnectionURL != "postgres://u:p@host/db" {
		t.Fatalf("unexpected branch: %+v", branch)
	}
}

// TestGetBranch_RealFieldShape locks in the exact JSON keys the live API
// uses: is_base (bool, not a base_branch name string), parent_branch_id
// ("branch_<id>" or null), postgres_version.
func TestGetBranch_RealFieldShape(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"ready","is_base":false,` +
			`"parent_branch_id":"branch_1","postgres_version":"17","created_at":"2026-08-29T00:00:00Z",` +
			`"expires_at":"2026-08-30T00:00:00Z","connection_url":"postgres://u:p@host/db"}`))
	})

	_, branch, err := client.GetBranch(context.Background(), "proj1", "feature-x")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	want := Branch{
		ID: "branch_2", Name: "feature-x", Status: "ready", IsBase: false,
		ParentBranchID: "branch_1", PostgresVersion: "17",
		CreatedAt: "2026-08-29T00:00:00Z", ExpiresAt: "2026-08-30T00:00:00Z",
		ConnectionURL: "postgres://u:p@host/db",
	}
	if branch != want {
		t.Fatalf("branch = %+v, want %+v", branch, want)
	}
}

// TestGetBranch_BaseBranchNullParent covers the base row: is_base:true,
// parent_branch_id:null (decodes to "").
func TestGetBranch_BaseBranchNullParent(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"branch_1","name":"main","status":"ready","is_base":true,` +
			`"parent_branch_id":null,"postgres_version":"17","created_at":"2026-08-01T00:00:00Z","expires_at":null}`))
	})

	_, branch, err := client.GetBranch(context.Background(), "proj1", "main")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if !branch.IsBase {
		t.Fatalf("IsBase = false, want true")
	}
	if branch.ParentBranchID != "" {
		t.Fatalf("ParentBranchID = %q, want empty (null parent)", branch.ParentBranchID)
	}
	if branch.ExpiresAt != "" {
		t.Fatalf("ExpiresAt = %q, want empty (no TTL)", branch.ExpiresAt)
	}
}

func TestGetBranch_404(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"branch not found"}`))
	})

	_, _, err := client.GetBranch(context.Background(), "proj1", "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("expected 404 APIError, got %T: %v", err, err)
	}
}

func TestDeleteBranch_Accepted(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"deleting"}`))
	})

	_, status, err := client.DeleteBranch(context.Background(), "proj1", "feature-x")
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if status != "deleting" {
		t.Fatalf("status = %q", status)
	}
}

func TestDeleteBranch_409OnBase(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"cannot delete a branch with children"}`))
	})

	_, _, err := client.DeleteBranch(context.Background(), "proj1", "main")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 {
		t.Fatalf("expected 409 APIError, got %T: %v", err, err)
	}
	if apiErr.Message != "cannot delete a branch with children" {
		t.Fatalf("Message = %q", apiErr.Message)
	}
}

func TestErrorMessageNeverLeaksToken(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	client.Token = "super-secret-token-value"

	_, _, err := client.CreateBranch(context.Background(), "proj1", CreateBranchRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), client.Token) {
		t.Fatalf("error message leaked the token: %s", err.Error())
	}
}

func TestJoinURL_NoDoubleSlash(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://api.example.com", "/api/v1/x", "https://api.example.com/api/v1/x"},
		{"https://api.example.com/", "/api/v1/x", "https://api.example.com/api/v1/x"},
		{"https://api.example.com", "api/v1/x", "https://api.example.com/api/v1/x"},
		{"https://api.example.com/", "api/v1/x", "https://api.example.com/api/v1/x"},
	}
	for _, c := range cases {
		if got := joinURL(c.base, c.path); got != c.want {
			t.Errorf("joinURL(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestPathEscaping(t *testing.T) {
	var gotPath string
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature/x y","status":"ready"}`))
	})
	// Rewire client base URL after we know the server (newTestServer already
	// sets it); this test only needs to confirm escaping round-trips.
	_, _, err := client.GetBranch(context.Background(), "proj 1", "feature/x y")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if !strings.Contains(gotPath, "proj%201") || !strings.Contains(gotPath, "feature%2Fx%20y") {
		t.Fatalf("path not escaped: %s", gotPath)
	}
}

// TestInvalidPathSegment_NeverHitsTheNetwork covers F3: "..", ".", and ""
// must be rejected before any request is built — the handler calling
// t.Fatal proves the client short-circuits rather than sending a
// dot-segment path an HTTP stack might normalize into an unintended route.
func TestInvalidPathSegment_NeverHitsTheNetwork(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})

	for _, bad := range []string{"", ".", ".."} {
		t.Run("project="+bad, func(t *testing.T) {
			_, _, err := client.ListBranches(context.Background(), bad)
			assertInvalidNameError(t, err, "project", bad)

			_, _, err = client.CreateBranch(context.Background(), bad, CreateBranchRequest{Name: "x"})
			assertInvalidNameError(t, err, "project", bad)

			_, _, err = client.GetBranch(context.Background(), bad, "ok-name")
			assertInvalidNameError(t, err, "project", bad)

			_, _, err = client.DeleteBranch(context.Background(), bad, "ok-name")
			assertInvalidNameError(t, err, "project", bad)
		})
		t.Run("name="+bad, func(t *testing.T) {
			_, _, err := client.GetBranch(context.Background(), "ok-project", bad)
			assertInvalidNameError(t, err, "name", bad)

			_, _, err = client.DeleteBranch(context.Background(), "ok-project", bad)
			assertInvalidNameError(t, err, "name", bad)
		})
	}
}

func assertInvalidNameError(t *testing.T, err error, wantField, wantValue string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var nameErr *InvalidNameError
	if !errors.As(err, &nameErr) {
		t.Fatalf("expected *InvalidNameError, got %T: %v", err, err)
	}
	if nameErr.Field != wantField || nameErr.Value != wantValue {
		t.Fatalf("got Field=%q Value=%q, want Field=%q Value=%q", nameErr.Field, nameErr.Value, wantField, wantValue)
	}
}

// TestValidTTL locks in the four accepted values (and that "" is NOT one of
// them — callers check emptiness separately) shared by the CLI and MCP.
func TestValidTTL(t *testing.T) {
	valid := map[string]bool{"1h": true, "6h": true, "24h": true, "7d": true}
	for _, ttl := range []string{"1h", "6h", "24h", "7d", "", "2h", "1H", "1hour", "-1h"} {
		if got, want := ValidTTL(ttl), valid[ttl]; got != want {
			t.Errorf("ValidTTL(%q) = %v, want %v", ttl, got, want)
		}
	}
}
