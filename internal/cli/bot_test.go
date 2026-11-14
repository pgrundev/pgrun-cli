package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakePgbot writes a shell-script pgbot that speaks just enough of pgbot's CLI
// for `pgrun bot` (version probe, schema snapshot/diff/compare, lint) and logs
// every invocation — argv and whether DATABASE_URL / an AI key reached it.
func fakePgbot(t *testing.T, diffSeverity string) (logPath, markPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake pgbot")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	markPath = filepath.Join(dir, "migrated")
	script := `#!/bin/sh
echo "ARGS $* | URL=${DATABASE_URL:-none} | AIKEY=${ANTHROPIC_API_KEY:-none}" >> "` + logPath + `"
case "$1 $2" in
  "--version "*) echo "pgbot version ${FAKE_PGBOT_VERSION:-0.10.0}"; exit 0;;
  "schema snapshot")
    if [ -n "$FAKE_PGBOT_NO_SCHEMA" ]; then echo "unknown command schema" >&2; exit 64; fi
    if [ "$3" = "--help" ]; then echo "Usage: pgbot schema snapshot [connection-string]"; exit 0; fi
    if [ -e "` + markPath + `" ]; then state=after; else state=before; fi
    echo '{"kind":"pgbot-schema-snapshot","version":"1.0.0","captured_at":"'$state'","database":"d","server_version":"18","relations":[],"columns":[],"indexes":[],"constraints":[],"extensions":[]}'
    exit 0;;
  "schema diff")
    if [ "` + diffSeverity + `" = "warning" ]; then
      echo '{"kind":"pgbot-schema-diff","dangerous":false,"summary":{"critical":0,"warning":1,"info":0},"findings":[{"id":"index_created_on_existing_table","severity":"warning","object":"public.orders","detail":"new index","advice":"CONCURRENTLY"}]}'
    elif [ "` + diffSeverity + `" = "critical" ]; then
      echo '{"kind":"pgbot-schema-diff","dangerous":true,"summary":{"critical":1,"warning":0,"info":0},"findings":[{"id":"table_dropped","severity":"critical","object":"public.legacy","detail":"dropped","advice":"keep it"}]}'
    else
      echo '{"kind":"pgbot-schema-diff","dangerous":false,"summary":{"critical":0,"warning":0,"info":1},"findings":[{"id":"table_created","severity":"info","object":"public.audit","detail":"created"}]}'
    fi
    exit 0;;
  "schema compare")
    if [ -n "$FAKE_COMPARE_DIFF" ]; then echo '{"kind":"pgbot-schema-compare","identical":false,"differences":[{"kind":"column_type","object":"public.t.x"}]}'; exit 0; fi
    echo '{"kind":"pgbot-schema-compare","identical":true,"differences":[]}'; exit 0;;
  "lint "*)
    if [ -n "$FAKE_LINT_FAIL" ]; then echo "lint: connection refused" >&2; exit 3; fi
    if [ -e "` + markPath + `" ]; then
      echo '{"findings":[{"id":"fk_without_index","severity":"warning","object":"public.orders","title":"FK without index"}]}'
    else
      echo '{"findings":[]}'
    fi
    exit 0;;
  "inspect "*) echo '{"findings":[]}'; exit 0;;
esac
echo "unexpected: $*" >&2; exit 64
`
	bin := filepath.Join(dir, "pgbot")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGRUN_PGBOT", bin)
	return logPath, markPath
}

type fakeBranches struct {
	mu      sync.Mutex
	created []map[string]any
	deleted []string
}

// verifyServer: every created branch becomes ready at once with an engine
// matching the request, except when the request asks for pgx and pgxOK is false
// (422 engine_unavailable, like the real API on an incompatible project).
func verifyServer(t *testing.T, fb *fakeBranches, pgxOK bool) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodPost:
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			if body["engine"] == "pgx" && !pgxOK {
				w.WriteHeader(http.StatusUnprocessableEntity)
				w.Write([]byte(`{"error":"engine_unavailable","engine":"pgx","reason":"incompatible","alternative":"postgres"}`))
				return
			}
			fb.mu.Lock()
			fb.created = append(fb.created, body)
			fb.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"` + body["name"].(string) + `","status":"creating"}`))
		case http.MethodGet:
			engine := "postgres"
			if strings.HasSuffix(name, "-pgx") {
				engine = "pgx"
			}
			w.Write([]byte(`{"id":"b1","name":"` + name + `","status":"ready","engine":"` + engine + `","connection_url":"postgres://u:sekretpassw0rd@` + name + `.example/db?sslmode=require"}`))
		case http.MethodDelete:
			fb.mu.Lock()
			fb.deleted = append(fb.deleted, name)
			fb.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		}
	})
}

func migrationCommand(mark string, exit string) []string {
	return []string{"/bin/sh", "-c", `echo "migrating $DATABASE_URL"; touch "` + mark + `"; exit ` + exit}
}

func TestBotVerify_PassCleansUpAndKeepsURLOutOfOutput(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	verifyServer(t, fb, true)
	logPath, mark := fakePgbot(t, "info")
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-reach-pgbot")

	args := append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "0")...)
	code, stdout, stderr := run(t, args...)
	if code != 0 {
		t.Fatalf("code %d, stderr %s, stdout %s", code, stderr, stdout)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, stdout)
	}
	// A new lint finding is informational: verdict warning, not blocking, exit 0.
	if rep.Kind != VerifyKind || rep.Verdict != "warning" || rep.Blocking || rep.Summary.NewLint != 1 ||
		strings.Join(rep.Categories, ",") != "warning" {
		t.Fatalf("report %+v", rep)
	}
	if len(fb.created) != 1 || fb.created[0]["engine"] != "postgres" || fb.created[0]["ttl"] != "1h" {
		t.Fatalf("created %+v, want one postgres branch with a 1h TTL", fb.created)
	}
	if len(fb.deleted) != 1 || fb.deleted[0] != fb.created[0]["name"] {
		t.Fatalf("deleted %v, want the verification branch", fb.deleted)
	}
	if len(rep.NewLintFindings) != 1 || rep.NewLintFindings[0].ID != "fk_without_index" {
		t.Fatalf("new lint findings %+v", rep.NewLintFindings)
	}
	if strings.Contains(stdout+stderr, "sekretpassw0rd") {
		t.Fatalf("branch password leaked into output:\n%s\n%s", stdout, stderr)
	}
	calls, _ := os.ReadFile(logPath)
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		args := strings.SplitN(line, " | ", 2)[0]
		if strings.Contains(args, "sekretpassw0rd") {
			t.Fatalf("connection URL passed on pgbot's argv: %s", line)
		}
		if !strings.Contains(line, "AIKEY=none") {
			t.Fatalf("an AI key reached pgbot: %s", line)
		}
	}
	if !strings.Contains(string(calls), "ARGS schema diff") {
		t.Fatalf("schema diff never ran:\n%s", calls)
	}
}

func TestBotVerify_CriticalReviewIsDangerousAndStillDeletes(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	verifyServer(t, fb, true)
	_, mark := fakePgbot(t, "critical")
	args := append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "0")...)
	code, stdout, _ := run(t, args...)
	var rep verifyReport
	_ = json.Unmarshal([]byte(stdout), &rep)
	if code != exitFailure || rep.Verdict != "dangerous" {
		t.Fatalf("code %d verdict %q, want 1/dangerous", code, rep.Verdict)
	}
	if len(fb.deleted) != 1 {
		t.Fatalf("deleted %v", fb.deleted)
	}
	// --fail-on none reports the same finding but passes.
	fb2 := &fakeBranches{}
	verifyServer(t, fb2, true)
	os.Remove(mark)
	args = append([]string{"bot", "verify", "proj1", "--json", "--fail-on", "none", "--"}, migrationCommand(mark, "0")...)
	if code, stdout, _ := run(t, args...); code != 0 || !strings.Contains(stdout, `"table_dropped"`) {
		t.Fatalf("--fail-on none: code %d\n%s", code, stdout)
	}
}

func TestBotVerify_FailedMigrationFailsAndDeletes(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	verifyServer(t, fb, true)
	_, mark := fakePgbot(t, "info")
	args := append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "3")...)
	code, stdout, _ := run(t, args...)
	var rep verifyReport
	_ = json.Unmarshal([]byte(stdout), &rep)
	if code != exitFailure || rep.Verdict != "fail" || rep.Postgres.ExitCode == nil || *rep.Postgres.ExitCode != 3 {
		t.Fatalf("code %d report %+v", code, rep)
	}
	if len(fb.deleted) != 1 {
		t.Fatalf("deleted %v", fb.deleted)
	}
}

func TestBotVerify_ComparePGX(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	verifyServer(t, fb, true)
	_, mark := fakePgbot(t, "info")
	args := append([]string{"bot", "verify", "proj1", "--json", "--compare-pgx", "--"}, migrationCommand(mark, "0")...)
	code, stdout, stderr := run(t, args...)
	var rep verifyReport
	_ = json.Unmarshal([]byte(stdout), &rep)
	if code != 0 || rep.PGXCompatible == nil || !*rep.PGXCompatible || rep.PGX == nil || rep.PGX.Engine != "pgx" {
		t.Fatalf("code %d report %+v stderr %s", code, rep, stderr)
	}
	if len(fb.created) != 2 || len(fb.deleted) != 2 {
		t.Fatalf("created %d deleted %d, want 2/2", len(fb.created), len(fb.deleted))
	}
}

func TestBotVerify_ComparePGXUnavailableIsReportedNotHidden(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	verifyServer(t, fb, false)
	_, mark := fakePgbot(t, "info")
	args := append([]string{"bot", "verify", "proj1", "--json", "--compare-pgx", "--"}, migrationCommand(mark, "0")...)
	code, stdout, _ := run(t, args...)
	var rep verifyReport
	_ = json.Unmarshal([]byte(stdout), &rep)
	if rep.PGXCompatible == nil || *rep.PGXCompatible || !strings.Contains(string(rep.PGX.Unavailable), "engine_unavailable") {
		t.Fatalf("report %+v", rep)
	}
	// PostgreSQL verification itself was fine: PGX being unavailable is
	// "unsupported", reported but not blocking under the default policy.
	if rep.Verdict != "unsupported" || rep.Blocking || code != 0 || len(fb.deleted) != 1 {
		t.Fatalf("verdict %q blocking %v code %d deleted %v", rep.Verdict, rep.Blocking, code, fb.deleted)
	}
}

func verifyJSON(t *testing.T, extra ...string) (int, verifyReport, string) {
	t.Helper()
	code, stdout, stderr := run(t, extra...)
	var rep verifyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("not JSON (code %d): %v\n%s\n%s", code, err, stdout, stderr)
	}
	return code, rep, stderr
}

// The brief's rule: a skipped verification must never read as a pass.
func TestBotVerify_OldPgbotWithoutSchemaIsIncompleteAndBlocks(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	verifyServer(t, fb, true)
	_, mark := fakePgbot(t, "info")
	t.Setenv("FAKE_PGBOT_VERSION", "0.9.0")
	t.Setenv("FAKE_PGBOT_NO_SCHEMA", "1")
	code, rep, _ := verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--compare-pgx", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "incomplete" || !rep.Blocking || code != exitFailure {
		t.Fatalf("code %d report %+v", code, rep)
	}
	if !strings.Contains(rep.SchemaReviewSkipped, "0.10.0") {
		t.Fatalf("skip reason should name the needed version: %q", rep.SchemaReviewSkipped)
	}
	if rep.PGXCompatible != nil {
		t.Fatalf("pgx_compatible must be absent when no comparison ran, got %v", *rep.PGXCompatible)
	}
	if len(fb.deleted) != 2 {
		t.Fatalf("deleted %v, want both branches", fb.deleted)
	}
	// An explicit policy may accept it — then it is reported, not hidden, and does not block.
	os.Remove(mark)
	code, rep, _ = verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--block-on", "fail,dangerous,error", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "incomplete" || rep.Blocking || code != 0 {
		t.Fatalf("accepted incomplete: code %d report %+v", code, rep)
	}
}

func TestBotVerify_WarningBelowThresholdInformsOnly(t *testing.T) {
	withFastPoll(t)
	verifyServer(t, &fakeBranches{}, true)
	_, mark := fakePgbot(t, "warning")
	code, rep, _ := verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "warning" || rep.Blocking || code != 0 || rep.Summary.Warning != 1 {
		t.Fatalf("code %d report %+v", code, rep)
	}
	os.Remove(mark)
	code, rep, _ = verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--fail-on", "warning", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "dangerous" || !rep.Blocking || code != exitFailure {
		t.Fatalf("--fail-on warning: code %d report %+v", code, rep)
	}
	os.Remove(mark)
	code, rep, _ = verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--block-on", "warning", "--"}, migrationCommand(mark, "0")...)...)
	if !rep.Blocking || code != exitFailure {
		t.Fatalf("--block-on warning: code %d report %+v", code, rep)
	}
}

func TestBotVerify_LintFailureIsIncomplete(t *testing.T) {
	withFastPoll(t)
	verifyServer(t, &fakeBranches{}, true)
	_, mark := fakePgbot(t, "info")
	t.Setenv("FAKE_LINT_FAIL", "1")
	code, rep, _ := verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "incomplete" || !rep.Blocking || code != exitFailure {
		t.Fatalf("code %d report %+v", code, rep)
	}
}

func TestBotVerify_PGXSchemaDifferenceIsUnsupported(t *testing.T) {
	withFastPoll(t)
	verifyServer(t, &fakeBranches{}, true)
	_, mark := fakePgbot(t, "info")
	t.Setenv("FAKE_COMPARE_DIFF", "1")
	code, rep, _ := verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--compare-pgx", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "unsupported" || rep.PGXCompatible == nil || *rep.PGXCompatible || rep.Blocking || code != 0 {
		t.Fatalf("code %d report %+v", code, rep)
	}
	os.Remove(mark)
	code, rep, _ = verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--compare-pgx", "--block-on", "fail,dangerous,unsupported", "--"}, migrationCommand(mark, "0")...)...)
	if !rep.Blocking || code != exitFailure {
		t.Fatalf("blocking on unsupported: code %d report %+v", code, rep)
	}
}

// A branch that never becomes ready is an infrastructure error: exit 3, so CI
// can tell "the platform failed" from "your migration is unsafe".
func TestBotVerify_InfrastructureErrorExitsThree(t *testing.T) {
	withFastPoll(t)
	fb := &fakeBranches{}
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"x","status":"creating"}`))
		case http.MethodGet:
			w.Write([]byte(`{"id":"b1","name":"` + name + `","status":"failed","engine":"postgres"}`))
		case http.MethodDelete:
			fb.mu.Lock()
			fb.deleted = append(fb.deleted, name)
			fb.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		}
	})
	_, mark := fakePgbot(t, "info")
	code, rep, _ := verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Verdict != "error" || !rep.Blocking || code != exitInfra {
		t.Fatalf("code %d report %+v", code, rep)
	}
	if len(fb.deleted) != 1 {
		t.Fatalf("a failed branch is still deleted, got %v", fb.deleted)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the migration must not run without a ready branch")
	}
}

func TestBotVerify_CleanupFailureIsAWarning(t *testing.T) {
	withFastPoll(t)
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"b1","name":"x","status":"creating"}`))
		case http.MethodGet:
			w.Write([]byte(`{"id":"b1","name":"` + name + `","status":"ready","engine":"postgres","connection_url":"postgres://u:p@h/db"}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
		}
	})
	_, mark := fakePgbot(t, "info")
	code, rep, _ := verifyJSON(t, append([]string{"bot", "verify", "proj1", "--json", "--"}, migrationCommand(mark, "0")...)...)
	if rep.Blocking || code != 0 || len(rep.Cleanup) != 1 || rep.Cleanup[0].Deleted {
		t.Fatalf("code %d report %+v", code, rep)
	}
	found := false
	for _, c := range rep.Categories {
		found = found || c == "warning"
	}
	if !found {
		t.Fatalf("categories %v, want warning for the undeleted branch", rep.Categories)
	}
}

func TestBotVerify_BlockOnValidation(t *testing.T) {
	isolateHome(t)
	fakePgbot(t, "info")
	code, _, stderr := run(t, "bot", "verify", "proj1", "--block-on", "fail,maybe", "--", "true")
	if code != exitUsage || !strings.Contains(stderr, "maybe") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
}

func TestBotBranchCommands_RefuseConnectionStringsAndAI(t *testing.T) {
	isolateHome(t)
	fakePgbot(t, "info")
	cases := [][]string{
		{"bot", "run", "proj", "br", "--", "inspect", "postgres://prod:pw@db.prod/app"},
		{"bot", "run", "proj", "br", "--", "inspect", "service=prod"},
		{"bot", "run", "proj", "br", "--", "ask", "why is it slow"},
		{"bot", "run", "proj", "br", "--", "sql", "explain", "--ai", "select 1"},
		{"bot", "explain", "proj", "br", "postgres://prod:pw@db.prod/app"},
	}
	for _, c := range cases {
		code, _, stderr := run(t, c...)
		if code != exitUsage {
			t.Errorf("%v: code %d, want usage refusal; stderr %s", c, code, stderr)
		}
		if strings.Contains(stderr, "pw@db.prod") {
			t.Errorf("%v: echoed the connection string: %s", c, stderr)
		}
	}
}

func TestBotLint_PassesBranchURLInEnvOnly(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"b1","name":"br","status":"ready","connection_url":"postgres://u:sekretpassw0rd@br.example/db"}`))
	})
	logPath, _ := fakePgbot(t, "info")
	code, stdout, stderr := run(t, "bot", "lint", "proj", "br", "--json")
	if code != 0 || !strings.Contains(stdout, `"findings"`) {
		t.Fatalf("code %d stdout %s stderr %s", code, stdout, stderr)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "ARGS lint --fail-on none --format json | URL=postgres://u:sekretpassw0rd@br.example/db") {
		t.Fatalf("pgbot did not get the branch URL via the environment:\n%s", calls)
	}
}

func TestBotInspect_SuppressesDisposableBranchSettings(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"b1","name":"br","status":"ready","connection_url":"postgres://u:p@br.example/db"}`))
	})
	logPath, _ := fakePgbot(t, "info")
	code, _, stderr := run(t, "bot", "inspect", "proj", "br", "--json")
	calls, _ := os.ReadFile(logPath)
	if code != 0 || !strings.Contains(string(calls), "ARGS inspect --no-store --ignore fsync_off --ignore full_page_writes_off --ignore checkpoints_forced --json") ||
		!strings.Contains(stderr, "suppressed for disposable branches") {
		t.Fatalf("code %d stderr %s calls %s", code, stderr, calls)
	}
}

func TestBotStatus_NotInstalled(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("PATH", home) // no pgbot anywhere
	t.Setenv("PGRUN_PGBOT", "")
	t.Setenv("PGRUN_HOME", filepath.Join(home, "pgrun"))
	code, stdout, _ := run(t, "bot", "status", "--json")
	var doc botStatusDoc
	_ = json.Unmarshal([]byte(stdout), &doc)
	if code != exitFailure || doc.Installed || !strings.Contains(doc.Hint, "pgrun bot install") {
		t.Fatalf("code %d doc %+v", code, doc)
	}
}

func TestUpsertManagedBlock(t *testing.T) {
	user := "# My project\n\nKeep this.\n"
	once, err := upsertManagedBlock(user, "hello")
	if err != nil || !strings.HasPrefix(once, user) || !strings.Contains(once, agentsBlockBegin+"\nhello\n"+agentsBlockEnd) {
		t.Fatalf("first insert: %q %v", once, err)
	}
	twice, _ := upsertManagedBlock(once, "hello")
	if twice != once {
		t.Fatalf("not idempotent:\n%q\n%q", once, twice)
	}
	withTail := once + "\n## After\nuser text\n"
	replaced, _ := upsertManagedBlock(withTail, "new body")
	if !strings.HasPrefix(replaced, user) || !strings.HasSuffix(replaced, "\n## After\nuser text\n") || strings.Contains(replaced, "hello") {
		t.Fatalf("replace touched user text: %q", replaced)
	}
	if _, err := upsertManagedBlock(user+agentsBlockBegin+"\nunterminated", "x"); err == nil {
		t.Fatal("an unterminated block must be refused, not guessed")
	}
}

func TestSetup_InstallsSkillAndReportsBot(t *testing.T) {
	home := isolateHome(t)
	fakePgbot(t, "info")
	agents := filepath.Join(home, "repo", "AGENTS.md")
	os.MkdirAll(filepath.Dir(agents), 0o755)
	os.WriteFile(agents, []byte("# Rules\n\nDo not touch.\n"), 0o644)
	t.Setenv("PGRUN_API_URL", "https://pgrun.example")
	t.Setenv("PGRUN_API_TOKEN", "tok")

	code, stdout, stderr := run(t, "setup", "--json", "--agent", "claude,codex", "--agents-md", agents)
	if code != 0 {
		t.Fatalf("code %d stderr %s stdout %s", code, stderr, stdout)
	}
	var doc setupDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Ready || doc.Pgbot.Action != "found" {
		t.Fatalf("doc %+v", doc)
	}
	for _, p := range []string{".claude/skills/pgrun-branching/SKILL.md", ".codex/skills/pgrun-branching/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, p)); err != nil {
			t.Errorf("skill missing at %s", p)
		}
	}
	want := map[string]bool{"bot.verify": false, "bot.schema_review": false, "engine.pgx": false}
	for _, c := range doc.Capabilities {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for c, ok := range want {
		if !ok {
			t.Errorf("capability %s not reported", c)
		}
	}
	md, _ := os.ReadFile(agents)
	if !strings.HasPrefix(string(md), "# Rules\n\nDo not touch.\n") || !strings.Contains(string(md), "pgrun bot verify") {
		t.Fatalf("AGENTS.md: %s", md)
	}
	// Second run: everything current, file unchanged.
	code, stdout, _ = run(t, "setup", "--json", "--agent", "claude,codex", "--agents-md", agents)
	md2, _ := os.ReadFile(agents)
	if code != 0 || string(md2) != string(md) || !strings.Contains(stdout, `"status": "current"`) {
		t.Fatalf("second run not idempotent: code %d\n%s", code, stdout)
	}
}

func TestSetup_WithoutAgentsMDFlagNeverWritesInstructions(t *testing.T) {
	home := isolateHome(t)
	fakePgbot(t, "info")
	wd, _ := os.Getwd()
	repo := filepath.Join(home, "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("mine\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("mine too\n"), 0o644)
	os.Chdir(repo)
	t.Cleanup(func() { os.Chdir(wd) })
	run(t, "setup", "--json")
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		b, _ := os.ReadFile(filepath.Join(repo, f))
		if !strings.HasPrefix(string(b), "mine") || strings.Contains(string(b), "pgrun") {
			t.Fatalf("%s changed without consent: %q", f, b)
		}
	}
}
