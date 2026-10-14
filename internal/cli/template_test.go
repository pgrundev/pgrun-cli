package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// templateServer fakes the templates + branches API. getStatuses is the
// sequence of statuses GET .../templates/<name> answers with (the last one
// repeats); every request is recorded as "METHOD path".
type templateServer struct {
	mu          sync.Mutex
	calls       []string
	putBody     map[string]any
	branchBody  map[string]any
	putStatus   string
	getStatuses []string
	buildStatus string
}

func (s *templateServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		name := filepath.Base(strings.TrimSuffix(r.URL.Path, "/build"))
		tpl := func(status string) string {
			return `{"name":"` + name + `","mode":"schema_only","status":"` + status + `","version":3}`
		}
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/templates/"):
			json.Unmarshal(body, &s.putBody)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(tpl(s.putStatus)))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/build"):
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(tpl(s.buildStatus)))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/templates/"):
			st := s.getStatuses[0]
			if len(s.getStatuses) > 1 {
				s.getStatuses = s.getStatuses[1:]
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(tpl(st)))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/branches"):
			json.Unmarshal(body, &s.branchBody)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"ci-1","status":"creating"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"b1","name":"ci-1","status":"ready","engine":"pgx","database":"b_9","connection_url":"postgres://u:p@host/b_9"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

func (s *templateServer) called(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestTemplateCreate_SchemaOnlyWithSeed_PutsDefinitionAndWaits(t *testing.T) {
	withFastPoll(t)
	srv := &templateServer{putStatus: "building", getStatuses: []string{"building", "ready"}}
	withServer(t, srv.handler(t))
	seed := filepath.Join(t.TempDir(), "ci.sql")
	os.WriteFile(seed, []byte("INSERT INTO roles (name) VALUES ('admin');"), 0o600)

	code, out, stderr := run(t, "template", "create", "proj1", "ci", "--schema-only", "--seed", seed, "--wait", "--timeout", "5s")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if srv.putBody["schema_only"] != true || srv.putBody["seed_sql"] != "INSERT INTO roles (name) VALUES ('admin');" {
		t.Fatalf("PUT body = %v", srv.putBody)
	}
	if !strings.Contains(out, "template ci: ready") {
		t.Fatalf("out = %q", out)
	}
}

func TestTemplateCreate_RequiresSchemaOnly(t *testing.T) {
	isolateHome(t)
	if code, _, _ := run(t, "template", "create", "proj1", "ci"); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestTemplateCreate_FailedBuildExitsFailure(t *testing.T) {
	withFastPoll(t)
	srv := &templateServer{putStatus: "building", getStatuses: []string{"failed"}}
	withServer(t, srv.handler(t))
	code, _, stderr := run(t, "template", "create", "proj1", "ci", "--schema-only", "--wait", "--timeout", "5s")
	if code != exitFailure || !strings.Contains(stderr, "template ci failed") {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
}

func TestBranchExec_SchemaOnly_DefinesTemplateThenMintsFromIt(t *testing.T) {
	withFastPoll(t)
	srv := &templateServer{putStatus: "building", getStatuses: []string{"ready"}}
	withServer(t, srv.handler(t))
	self := helperCommand(t, "echo_database_url")

	code, out, stderr := run(t, append([]string{
		"branch", "exec", "proj1", "--create", "ci-1", "--schema-only", "--delete-after", "--timings", "--timeout", "5s", "--",
	}, self...)...)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if out != "postgres://u:p@host/b_9" {
		t.Fatalf("out = %q", out)
	}
	if !srv.called("PUT /api/v1/projects/proj1/templates/schema-only") {
		t.Fatalf("schema-only template was not defined: %v", srv.calls)
	}
	if srv.branchBody["template"] != "schema-only" {
		t.Fatalf("branch POST body = %v, want template schema-only", srv.branchBody)
	}
	if !strings.Contains(stderr, "template=schema-only") || !strings.Contains(stderr, "template_wait_ms=") {
		t.Fatalf("timings line missing template fields: %q", stderr)
	}
}

func TestBranchCreate_StaleTemplate_RebuildsBeforeCreating(t *testing.T) {
	withFastPoll(t)
	srv := &templateServer{buildStatus: "building", getStatuses: []string{"stale", "ready"}}
	withServer(t, srv.handler(t))

	code, _, stderr := run(t, "branch", "create", "proj1", "--name", "ci-1", "--template", "ci")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !srv.called("POST /api/v1/projects/proj1/templates/ci/build") {
		t.Fatalf("stale template was not rebuilt: %v", srv.calls)
	}
	if srv.branchBody["template"] != "ci" {
		t.Fatalf("branch POST body = %v", srv.branchBody)
	}
}

func TestBranchExec_FailedTemplate_NeverCreatesBranch(t *testing.T) {
	withFastPoll(t)
	srv := &templateServer{getStatuses: []string{"failed"}}
	withServer(t, srv.handler(t))
	self := helperCommand(t, "echo_database_url")

	code, _, _ := run(t, append([]string{
		"branch", "exec", "proj1", "--create", "ci-1", "--template", "ci", "--delete-after", "--",
	}, self...)...)
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if srv.called("POST /api/v1/projects/proj1/branches") {
		t.Fatalf("a branch was created from a failed template: %v", srv.calls)
	}
}

func TestTemplateFlags_UsageErrors(t *testing.T) {
	isolateHome(t)
	cases := [][]string{
		{"branch", "create", "proj1", "--name", "x", "--schema-only", "--template", "ci"},
		{"branch", "create", "proj1", "--name", "x", "--template", "ci", "--parent", "main"},
		{"branch", "exec", "proj1", "--create", "x", "--schema-only", "--from", "main", "--", "cmd"},
		{"branch", "exec", "proj1", "name", "--template", "ci", "--", "cmd"},
	}
	for _, args := range cases {
		if code, _, stderr := run(t, args...); code != exitUsage {
			t.Errorf("args=%v: code = %d, want %d (stderr=%q)", args, code, exitUsage, stderr)
		}
	}
}
