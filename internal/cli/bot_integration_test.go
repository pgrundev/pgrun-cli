package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// TestBotVerify_RealPostgresAndPgbot runs `pgrun bot verify` end to end with a
// real pgbot binary against real PostgreSQL databases. Only the PGRun API is
// stood in for: creating a "branch" clones a golden database
// (CREATE DATABASE … TEMPLATE golden), deleting one drops it — the same
// copy-of-the-golden semantics a PGRun branch has. Needs:
//
//	PGRUN_TEST_PG_DSN   superuser URL of a scratch server (…/postgres?sslmode=disable)
//	PGRUN_TEST_PGBOT    path to a pgbot with the `schema` command
//	PGRUN_TEST_PSQL     path to psql
func TestBotVerify_RealPostgresAndPgbot(t *testing.T) {
	dsn, bot, psql := os.Getenv("PGRUN_TEST_PG_DSN"), os.Getenv("PGRUN_TEST_PGBOT"), os.Getenv("PGRUN_TEST_PSQL")
	if dsn == "" || bot == "" || psql == "" {
		t.Skip("PGRUN_TEST_PG_DSN / PGRUN_TEST_PGBOT / PGRUN_TEST_PSQL not set")
	}
	withFastPoll(t)
	sql := func(db, stmt string) {
		t.Helper()
		u, _ := url.Parse(dsn)
		u.Path = "/" + db
		out, err := exec.Command(psql, u.String(), "-v", "ON_ERROR_STOP=1", "-qAtc", stmt).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", stmt, err, out)
		}
	}
	golden := fmt.Sprintf("golden_it_%d", os.Getpid())
	sql("postgres", "DROP DATABASE IF EXISTS "+golden)
	sql("postgres", "CREATE DATABASE "+golden)
	t.Cleanup(func() { sql("postgres", "DROP DATABASE IF EXISTS "+golden) })
	sql(golden, `CREATE TABLE users (id bigserial PRIMARY KEY, email text, legacy text);
		INSERT INTO users (email, legacy) SELECT 'u'||g||'@x', 'l' FROM generate_series(1, 20000) g;
		CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint, total int);
		INSERT INTO orders (user_id, total) SELECT (g % 20000) + 1, g FROM generate_series(1, 20000) g`)

	var mu sync.Mutex
	live := map[string]bool{}
	dbName := func(branch string) string { return "br_" + strings.ReplaceAll(branch, "-", "_") }
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodPost:
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			name = body["name"].(string)
			sql("postgres", "CREATE DATABASE "+dbName(name)+" TEMPLATE "+golden)
			mu.Lock()
			live[name] = true
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"b","name":%q,"status":"creating"}`, name)
		case http.MethodGet:
			u, _ := url.Parse(dsn)
			u.Path = "/" + dbName(name)
			fmt.Fprintf(w, `{"id":"b","name":%q,"status":"ready","engine":"postgres","database":%q,"connection_url":%q}`, name, dbName(name), u.String())
		case http.MethodDelete:
			sql("postgres", "DROP DATABASE IF EXISTS "+dbName(name)+" WITH (FORCE)")
			mu.Lock()
			delete(live, name)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		}
	})
	t.Setenv("PGRUN_PGBOT", bot)

	// A migration with one safe step, one lock hazard and one data loss.
	migration := []string{psql, "-v", "ON_ERROR_STOP=1", "-qc",
		`ALTER TABLE users ADD COLUMN plan text NOT NULL DEFAULT 'free';
		 ALTER TABLE orders ALTER COLUMN total TYPE bigint;
		 CREATE INDEX orders_user_idx ON orders (user_id);
		 ALTER TABLE users DROP COLUMN legacy`}
	// psql takes the URL from $DATABASE_URL only via an explicit argument; wrap it.
	cmd := []string{"/bin/sh", "-c", `exec "$0" "$DATABASE_URL" "$@"`}
	cmd = append(cmd, migration...)
	args := append([]string{"bot", "verify", "proj", "--json", "--"}, cmd...)
	code, stdout, stderr := run(t, args...)

	var rep verifyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("not JSON (code %d): %v\nstdout %s\nstderr %s", code, err, stdout, stderr)
	}
	if rep.Verdict != "dangerous" || code != exitFailure {
		t.Fatalf("verdict %q code %d, want dangerous/1\n%s\n%s", rep.Verdict, code, stdout, stderr)
	}
	var review struct {
		Findings []struct{ ID, Severity, Object string } `json:"findings"`
	}
	if err := json.Unmarshal(rep.SchemaReview, &review); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range review.Findings {
		got[f.ID+" "+f.Object] = f.Severity
	}
	for k, sev := range map[string]string{
		"table_rewritten public.orders":                          "warning",
		"index_created_on_existing_table public.orders_user_idx": "warning",
		"column_dropped public.users.legacy":                     "critical",
		"column_added public.users.plan":                         "info",
	} {
		if got[k] != sev {
			t.Errorf("finding %q = %q, want %q (all: %v)", k, got[k], sev, got)
		}
	}
	if _, rewritten := got["table_rewritten public.users"]; rewritten {
		t.Error("constant-default ADD COLUMN reported as a rewrite")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(live) != 0 {
		t.Fatalf("verification databases left behind: %v", live)
	}
	if len(rep.Cleanup) != 1 || !rep.Cleanup[0].Deleted {
		t.Fatalf("cleanup %+v", rep.Cleanup)
	}
}
