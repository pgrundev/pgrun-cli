package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgrundev/pgrun/internal/config"
)

// chdir switches the process's working directory to dir for the duration of
// one test and restores it afterward (the same technique
// TestSkillInstall_DirAndProjectTargets uses in skill_test.go) — needed here
// because project use/show and the branch-command fallback all key off
// os.Getwd().
func chdir(t *testing.T, dir string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
}

// --- projects list ---

func TestProjectsList_Table(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"projects":[` +
			`{"name":"jobsgpt","status":"ready","base_branch":"main","branches":2,"postgres_version":"18","sanitized":true},` +
			`{"name":"other","status":"preparing","base_branch":null,"branches":0,"postgres_version":"17","sanitized":false}` +
			`]}`))
	})
	dir := t.TempDir()
	chdir(t, dir)
	if _, err := config.SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	code, out, _ := run(t, "projects", "list")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"PROJECT", "STATUS", "BASE", "BRANCHES", "PG", "jobsgpt", "other", "ready", "preparing", "main"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q: %q", want, out)
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 rows, got %d lines: %q", len(lines), out)
	}
	jobsgptLine := lines[1]
	otherLine := lines[2]
	if !strings.Contains(jobsgptLine, "jobsgpt") {
		jobsgptLine, otherLine = otherLine, jobsgptLine
	}
	if !strings.HasPrefix(jobsgptLine, "*") {
		t.Fatalf("current project row should be marked with *: %q", jobsgptLine)
	}
	if strings.HasPrefix(otherLine, "*") {
		t.Fatalf("non-current project row should not be marked: %q", otherLine)
	}
}

func TestProjectsList_JSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"projects":[{"name":"jobsgpt","status":"ready","base_branch":"main","branches":2,"postgres_version":"18","sanitized":true}],"extra_field":"kept-verbatim"}`))
	})
	code, out, _ := run(t, "projects", "list", "--json")
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

func TestProjectsList_Empty(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"projects":[]}`))
	})
	code, out, stderr := run(t, "projects", "list")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if stderr == "" {
		t.Fatal("expected a friendly stderr line for an empty project list")
	}
}

func TestProjectsList_401ExitsAuth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})
	code, _, stderr := run(t, "projects", "list")
	if code != exitAuth {
		t.Fatalf("code = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(stderr, "pgrun auth set") {
		t.Fatalf("stderr missing hint: %q", stderr)
	}
}

// --- project use ---

func TestProjectUse_WritesFile(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"projects":[{"name":"jobsgpt","status":"ready"}]}`))
	})
	dir := t.TempDir()
	chdir(t, dir)

	code, out, stderr := run(t, "project", "use", "jobsgpt")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	wantPath := filepath.Join(dir, ".pgrun", "project")
	if !strings.Contains(out, `using project "jobsgpt"`) || !strings.Contains(out, wantPath) {
		t.Fatalf("out = %q", out)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("project file not written: %v", err)
	}
	if string(data) != "jobsgpt\n" {
		t.Fatalf("file contents = %q, want %q", data, "jobsgpt\n")
	}
}

func TestProjectUse_UnknownSlugExitsFailure(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"projects":[{"name":"jobsgpt","status":"ready"},{"name":"other","status":"ready"}]}`))
	})
	dir := t.TempDir()
	chdir(t, dir)

	code, _, stderr := run(t, "project", "use", "bogus")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "jobsgpt") || !strings.Contains(stderr, "other") {
		t.Fatalf("stderr should name the available slugs: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".pgrun", "project")); err == nil {
		t.Fatal("project file should not have been written for an unknown slug")
	}
}

func TestProjectUse_NoVerifySkipsAPI(t *testing.T) {
	// No server at all — isolateHome clears PGRUN_API_URL/TOKEN and the
	// config file, so if --no-verify called the API it would have nothing
	// to call and fail with exitAuth (resolveOrHint). Success here proves
	// the API was never touched.
	isolateHome(t)
	dir := t.TempDir()
	chdir(t, dir)

	code, out, stderr := run(t, "project", "use", "jobsgpt", "--no-verify")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(out, `using project "jobsgpt"`) {
		t.Fatalf("out = %q", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".pgrun", "project"))
	if err != nil {
		t.Fatalf("project file not written: %v", err)
	}
	if string(data) != "jobsgpt\n" {
		t.Fatalf("file contents = %q", data)
	}
}

// --- project show ---

func TestProjectShow_ReportsSelected(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if _, err := config.SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	code, out, _ := run(t, "project", "show")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "jobsgpt") {
		t.Fatalf("out = %q", out)
	}
}

func TestProjectShow_ReportsNone(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	code, out, _ := run(t, "project", "show")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out, "no project selected") {
		t.Fatalf("out = %q", out)
	}
}

// --- .pgrun/project fallback for branch commands ---

func TestBranchCreate_ProjectFallback_FromPgrunProject(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/projects/jobsgpt/branches") {
			t.Fatalf("expected project jobsgpt in the request path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
	})
	dir := t.TempDir()
	chdir(t, dir)
	if _, err := config.SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	code, out, stderr := run(t, "branch", "create", "--name", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(out, "feature-x") {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchCreate_ProjectFallback_MissingBoth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	dir := t.TempDir()
	chdir(t, dir)

	code, _, stderr := run(t, "branch", "create", "--name", "feature-x")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "no .pgrun/project found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBranchURL_ProjectFallback_FromPgrunProject(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/projects/jobsgpt/branches/feature-x") {
			t.Fatalf("expected project jobsgpt in the request path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	dir := t.TempDir()
	chdir(t, dir)
	if _, err := config.SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	code, out, _ := run(t, "branch", "url", "feature-x")
	if code != exitSuccess {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(out) != "DATABASE_URL=postgres://u:p@host/feature-x" {
		t.Fatalf("out = %q", out)
	}
}

func TestBranchURL_ProjectFallback_MissingBoth(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	dir := t.TempDir()
	chdir(t, dir)

	code, _, stderr := run(t, "branch", "url", "feature-x")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "no .pgrun/project found") {
		t.Fatalf("stderr = %q", stderr)
	}
}
