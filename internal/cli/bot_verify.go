package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
	"github.com/pgrundev/pgrun-cli/internal/pgbot"
)

// `pgrun bot verify -- <migration command>` is the PostgreSQL verification
// workflow. On a DISPOSABLE branch of the project's Safe Copy (PostgreSQL by
// default, so the canonical engine) it:
//
//  1. snapshots the schema and runs pgbot lint (the "before"),
//  2. runs the migration command with DATABASE_URL in its environment only,
//  3. snapshots and lints again, and has pgbot classify what the migration did
//     (rewrites, blocking index builds, validating scans, drops, renames),
//  4. with --compare-pgx, runs the same migration on a PGX branch and compares
//     the resulting schema with PostgreSQL's,
//  5. deletes every branch it created (unless --keep), even on failure.
//
// It never touches the project's base branch or Safe Copy, and pgbot only
// reads. The migration command is the caller's own, run exactly like
// `pgrun branch exec` runs it.

const (
	VerifyKind    = "pgrun-verify"
	VerifyVersion = "1.1.0"
)

type verifyEngineRun struct {
	Engine          string          `json:"engine"`
	RequestedEngine string          `json:"requested_engine"`
	Branch          string          `json:"branch"`
	Database        string          `json:"database,omitempty"`
	ReadyMs         int64           `json:"ready_ms"`
	MigrateMs       int64           `json:"migrate_ms"`
	ExitCode        *int            `json:"migration_exit_code,omitempty"`
	Error           string          `json:"error,omitempty"`
	Unavailable     json.RawMessage `json:"unavailable,omitempty"` // the API's engine_unavailable body
}

type lintFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Object   string `json:"object,omitempty"`
	Title    string `json:"title"`
}

type verifyCleanup struct {
	Branch  string `json:"branch"`
	Deleted bool   `json:"deleted"`
	Error   string `json:"error,omitempty"`
	Kept    bool   `json:"kept,omitempty"`
}

type verifyReport struct {
	Kind                string           `json:"kind"`
	Version             string           `json:"version"`
	Project             string           `json:"project"`
	Verdict             string           `json:"verdict"` // see verdictOrder
	Categories          []string         `json:"categories"`
	Blocking            bool             `json:"blocking"`
	Policy              verifyPolicy     `json:"policy"`
	Summary             verifySummary    `json:"summary"`
	Reasons             []string         `json:"reasons"`
	FailOn              string           `json:"fail_on"` // = policy.fail_on (1.0.0 field, kept)
	Postgres            *verifyEngineRun `json:"verification"`
	SchemaReview        json.RawMessage  `json:"schema_review,omitempty"`
	SchemaReviewSkipped string           `json:"schema_review_skipped,omitempty"`
	NewLintFindings     []lintFinding    `json:"new_lint_findings"`
	PGX                 *verifyEngineRun `json:"pgx,omitempty"`
	PGXComparison       json.RawMessage  `json:"pgx_comparison,omitempty"`
	PGXCompatible       *bool            `json:"pgx_compatible,omitempty"`
	Cleanup             []verifyCleanup  `json:"cleanup"`
	Pgbot               string           `json:"pgbot_version"`
	DurationMs          int64            `json:"duration_ms"`
}

// Verdicts, highest precedence first. A run collects every category that applies;
// the verdict is the highest one, and the run BLOCKS when any collected category
// is in the policy's block_on list. A review that could not run is "incomplete",
// never "pass".
//
//	fail        the migration exited non-zero on canonical PostgreSQL (confirmed failure)
//	dangerous   the schema review found a finding at/above --fail-on (rewrite, blocking
//	            index build, SET NOT NULL, drop, rename, type change, …)
//	unsupported --compare-pgx: PGX was unavailable, failed where PostgreSQL succeeded,
//	            or produced a different schema (PostgreSQL itself was fine)
//	incomplete  the schema review or lint could not run (e.g. a pgbot without `schema`)
//	error       infrastructure or tool error (branch never ready, API, command not startable)
//	warning     informational: findings below --fail-on, new lint findings, a branch
//	            that could not be deleted (its TTL removes it)
//	pass        everything ran and nothing was found
var verdictOrder = []string{"fail", "dangerous", "unsupported", "incomplete", "error", "warning", "pass"}

// DefaultBlockOn: warnings and PGX-only differences inform; they do not block.
var DefaultBlockOn = []string{"fail", "dangerous", "incomplete", "error"}

const exitInfra = 3 // blocking, and every blocking category is "error"

type verifyPolicy struct {
	FailOn  string   `json:"fail_on"`
	BlockOn []string `json:"block_on"`
}

type verifySummary struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
	NewLint  int `json:"new_lint"`
}

func parseBlockOn(s string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" || c == "none" {
			continue
		}
		ok := false
		for _, v := range verdictOrder {
			ok = ok || (v == c && v != "pass")
		}
		if !ok {
			return nil, fmt.Errorf("unknown category %q (want fail, dangerous, unsupported, incomplete, error, warning or none)", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func (rep *verifyReport) add(category, reason string) {
	for _, c := range rep.Categories {
		if c == category {
			goto reason
		}
	}
	rep.Categories = append(rep.Categories, category)
reason:
	if reason != "" {
		rep.Reasons = append(rep.Reasons, reason)
	}
}

// finish derives verdict and blocking from the collected categories.
func (rep *verifyReport) finish() {
	rank := map[string]int{}
	for i, v := range verdictOrder {
		rank[v] = i
	}
	if len(rep.Categories) == 0 {
		rep.Categories = []string{"pass"}
	}
	sortByRank := func(cs []string) {
		for i := 1; i < len(cs); i++ {
			for j := i; j > 0 && rank[cs[j]] < rank[cs[j-1]]; j-- {
				cs[j], cs[j-1] = cs[j-1], cs[j]
			}
		}
	}
	sortByRank(rep.Categories)
	rep.Verdict = rep.Categories[0]
	rep.Blocking = false
	for _, c := range rep.Categories {
		for _, b := range rep.Policy.BlockOn {
			if c == b {
				rep.Blocking = true
			}
		}
	}
}

// exitCode: 0 not blocking; 3 when only infrastructure errors block; else 1.
func (rep *verifyReport) exitCode() int {
	if !rep.Blocking {
		return exitSuccess
	}
	for _, c := range rep.Categories {
		for _, b := range rep.Policy.BlockOn {
			if c == b && c != "error" {
				return exitFailure
			}
		}
	}
	return exitInfra
}

func botVerify(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) > 1 {
		return usageErrf(stderr, "bot verify: unexpected argument %s", redactedArg(pos[1]))
	}
	fs := flag.NewFlagSet("bot verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "parent branch to verify on (default: the project's base — its Safe Copy)")
	engine := fs.String("engine", "postgres", "engine of the verification branch: postgres (canonical) or pgx")
	comparePGX := fs.Bool("compare-pgx", false, "also run the migration on a PGX branch and compare its schema with PostgreSQL's")
	ttl := fs.String("ttl", "1h", "TTL of the verification branches (the backstop if pgrun is killed)")
	keep := fs.Bool("keep", false, "keep the verification branches instead of deleting them")
	failOn := fs.String("fail-on", "critical", "a schema-review finding at/above this is `dangerous`: critical|warning|none")
	blockOn := fs.String("block-on", strings.Join(DefaultBlockOn, ","), "categories that make the run blocking (exit non-zero): fail,dangerous,unsupported,incomplete,error,warning, or none")
	timeout := fs.Duration("timeout", 300*time.Second, "max wait for each branch to become ready")
	jsonOut := fs.Bool("json", false, "print the verification report as JSON (pgrun-verify/1.0.0) on stdout")
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	command := fs.Args()
	if len(command) == 0 {
		return usageErrf(stderr, "bot verify: missing the migration command after --, e.g. pgrun bot verify -- bin/rails db:migrate")
	}
	if *engine != "postgres" && *engine != "pgx" {
		return usageErrf(stderr, "bot verify: --engine must be postgres or pgx")
	}
	if *comparePGX && *engine == "pgx" {
		return usageErrf(stderr, "bot verify: --compare-pgx compares PGX against a postgres verification branch; leave --engine at postgres")
	}
	switch *failOn {
	case "critical", "warning", "none":
	default:
		return usageErrf(stderr, "bot verify: --fail-on must be critical, warning or none")
	}
	blocking, err := parseBlockOn(*blockOn)
	if err != nil {
		return usageErrf(stderr, "bot verify: --block-on: %v", err)
	}
	if !api.ValidTTL(*ttl) {
		return usageErrf(stderr, "bot verify: --ttl must be one of 1h, 6h, 24h, 7d")
	}
	explicit := ""
	if len(pos) == 1 {
		explicit = pos[0]
	}
	project, code, ok := resolveProject(explicit, rest, stderr)
	if !ok {
		return code
	}
	bin, ok := requireBot(context.Background(), stderr)
	if !ok {
		return exitFailure
	}
	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)
	client.OnCapacityWait = func(d time.Duration) {
		fmt.Fprintf(stderr, "pgrun: the host is busy, retrying in %ds\n", int(d.Seconds()))
	}
	v := &verifier{client: client, bin: bin, project: project, from: *from, ttl: *ttl, timeout: *timeout,
		command: command, stderr: stderr, keep: *keep}
	rep := v.run(*engine, *comparePGX, verifyPolicy{FailOn: *failOn, BlockOn: blocking})
	// Every branch URL is scrubbed from the report too (an error string that
	// echoed one must not print it).
	for _, u := range v.redactorURLs {
		stdout = redactWriter(stdout, newRedactor(u))
	}
	if *jsonOut {
		writeJSONDoc(stdout, rep)
	} else {
		printVerify(stdout, rep)
	}
	return rep.exitCode()
}

type verifier struct {
	client       *api.Client
	bin          pgbot.Binary
	project      string
	from         string
	ttl          string
	timeout      time.Duration
	command      []string
	stderr       io.Writer
	keep         bool
	mu           sync.Mutex
	created      []string
	redactorURLs []string
}

func randSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (v *verifier) run(engine string, comparePGX bool, policy verifyPolicy) (rep verifyReport) {
	began := time.Now()
	failOn := policy.FailOn
	rep = verifyReport{Kind: VerifyKind, Version: VerifyVersion, Project: v.project, FailOn: failOn, Policy: policy,
		Pgbot: v.bin.Version, Categories: []string{}, Reasons: []string{}, NewLintFindings: []lintFinding{}, Cleanup: []verifyCleanup{}}
	defer func() {
		rep.Cleanup = v.cleanup()
		for _, c := range rep.Cleanup {
			if !c.Deleted && !c.Kept {
				rep.add("warning", "verification branch "+c.Branch+" could not be deleted (its TTL removes it): "+c.Error)
			}
		}
		rep.Summary.NewLint = len(rep.NewLintFindings)
		rep.DurationMs = time.Since(began).Milliseconds()
		rep.finish()
	}()
	work, err := os.MkdirTemp("", "pgrun-verify-*")
	if err != nil {
		rep.add("error", err.Error())
		return rep
	}
	defer os.RemoveAll(work)

	stamp := time.Now().UTC().Format("0102-1504") + "-" + randSuffix()
	pgRun := &verifyEngineRun{RequestedEngine: engine, Branch: "verify-" + stamp + "-" + engineTag(engine)}
	rep.Postgres = pgRun
	var pgxRun *verifyEngineRun
	if comparePGX {
		pgxRun = &verifyEngineRun{RequestedEngine: "pgx", Branch: "verify-" + stamp + "-pgx"}
		rep.PGX = pgxRun
	}

	// Create every branch first, then wait: provisioning runs in parallel.
	pgBranch, pgRaw := v.create(pgRun)
	var pgxBranch api.Branch
	var pgxRaw []byte
	if pgxRun != nil {
		pgxBranch, pgxRaw = v.create(pgxRun)
	}
	pgURL := v.wait(pgRun, pgBranch, pgRaw)
	pgxURL := ""
	if pgxRun != nil {
		pgxURL = v.wait(pgxRun, pgxBranch, pgxRaw)
	}
	if pgURL == "" {
		rep.add("error", "verification branch not ready: "+pgRun.Error)
		return rep
	}

	canReview := v.bin.Capabilities.SchemaReview
	before := filepath.Join(work, "before.json")
	after := filepath.Join(work, "after.json")
	if canReview {
		if err := v.pgbotToFile(pgURL, before, "schema", "snapshot"); err != nil {
			canReview = false
			rep.SchemaReviewSkipped = "schema snapshot before the migration failed: " + err.Error()
		}
	} else {
		rep.SchemaReviewSkipped = fmt.Sprintf("pgbot %s has no `schema` command (needs pgbot %s or newer)", v.bin.Version, pgbot.SchemaMinVersion)
	}
	lintBefore, lintErr := v.lint(pgURL)
	if lintErr != nil {
		rep.add("incomplete", "lint before the migration failed: "+lintErr.Error())
	}

	code := v.migrate(pgRun, pgURL)
	switch {
	case pgRun.Error != "":
		rep.add("error", "the migration command could not start: "+pgRun.Error)
	case code != 0:
		rep.add("fail", fmt.Sprintf("the migration exited %d on %s", code, pgRun.Engine))
	}

	if canReview {
		if err := v.pgbotToFile(pgURL, after, "schema", "snapshot"); err != nil {
			rep.SchemaReviewSkipped = "schema snapshot after the migration failed: " + err.Error()
		} else {
			out, _, err := pgbot.Output(context.Background(), v.bin, "", "schema", "diff", before, after, "--format", "json")
			if err != nil {
				rep.SchemaReviewSkipped = "schema diff failed: " + err.Error()
			} else {
				rep.SchemaReview = json.RawMessage(out)
				rep.Summary.Critical, rep.Summary.Warning, rep.Summary.Info = reviewCounts(out)
				switch sev := reviewSeverity(out); {
				case atOrAbove(sev, failOn):
					rep.add("dangerous", "schema review: "+summaryLine(out))
				case sev == "critical" || sev == "warning":
					rep.add("warning", "schema review (below --fail-on "+failOn+"): "+summaryLine(out))
				}
			}
		}
	}
	if rep.SchemaReviewSkipped != "" {
		rep.add("incomplete", "schema review did not run: "+rep.SchemaReviewSkipped)
	}
	if lintErr == nil {
		if lintAfter, err := v.lint(pgURL); err == nil {
			rep.NewLintFindings = newFindings(lintBefore, lintAfter)
			if len(rep.NewLintFindings) > 0 {
				rep.add("warning", fmt.Sprintf("%d new lint finding(s) after the migration", len(rep.NewLintFindings)))
			}
		} else {
			rep.add("incomplete", "lint after the migration failed: "+err.Error())
		}
	}

	if pgxRun != nil {
		v.verifyPGX(&rep, pgxRun, pgxURL, after, canReview && rep.SchemaReviewSkipped == "", work)
	}
	return rep
}

func reviewCounts(diff []byte) (critical, warning, info int) {
	var d struct {
		Summary struct{ Critical, Warning, Info int } `json:"summary"`
	}
	_ = json.Unmarshal(diff, &d)
	return d.Summary.Critical, d.Summary.Warning, d.Summary.Info
}

func engineTag(engine string) string {
	if engine == "pgx" {
		return "pgx"
	}
	return "pg"
}

func (v *verifier) create(run *verifyEngineRun) (api.Branch, []byte) {
	raw, b, err := v.client.CreateBranch(context.Background(), v.project, api.CreateBranchRequest{
		Name: run.Branch, TTL: v.ttl, ParentBranchID: v.from, Engine: run.RequestedEngine})
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && strings.Contains(string(raw), `"engine_unavailable"`) {
			run.Unavailable = json.RawMessage(raw)
		}
		run.Error = err.Error()
		return api.Branch{}, nil
	}
	v.mu.Lock()
	v.created = append(v.created, run.Branch)
	v.mu.Unlock()
	return b, raw
}

func (v *verifier) wait(run *verifyEngineRun, b api.Branch, raw []byte) string {
	if run.Error != "" {
		return ""
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), v.timeout)
	defer cancel()
	_, b, err := v.client.WaitForTerminal(ctx, v.project, raw, b)
	run.ReadyMs = time.Since(began).Milliseconds()
	switch {
	case err != nil:
		run.Error = err.Error()
	case b.Status != api.StatusReady:
		run.Error = api.WaitFailureReason(b.Name, b.Status)
	case b.ConnectionURL == "":
		run.Error = "ready but no connection_url"
	}
	run.Engine, run.Database = b.Engine, b.Database
	if run.Error != "" {
		return ""
	}
	v.mu.Lock()
	v.redactorURLs = append(v.redactorURLs, b.ConnectionURL)
	v.mu.Unlock()
	return b.ConnectionURL
}

// out redacts every branch URL this verifier has seen.
func (v *verifier) out() io.Writer {
	w := v.stderr
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, u := range v.redactorURLs {
		w = redactWriter(w, newRedactor(u))
	}
	return w
}

// migrate runs the caller's command; its output goes to stderr so stdout stays
// the report.
func (v *verifier) migrate(run *verifyEngineRun, url string) int {
	fmt.Fprintf(v.stderr, "pgrun: running the migration on %s branch %s\n", run.Engine, run.Branch)
	began := time.Now()
	cmd := exec.Command(v.command[0], v.command[1:]...)
	cmd.Stdout, cmd.Stderr = v.out(), v.out()
	cmd.Env = append(os.Environ(), "DATABASE_URL="+url)
	err := cmd.Run()
	run.MigrateMs = time.Since(began).Milliseconds()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = 127
			run.Error = err.Error()
		}
	}
	run.ExitCode = &code
	return code
}

func (v *verifier) pgbotToFile(url, path string, args ...string) error {
	out, _, err := pgbot.Output(context.Background(), v.bin, url, args...)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func (v *verifier) lint(url string) ([]lintFinding, error) {
	out, _, err := pgbot.Output(context.Background(), v.bin, url, "lint", "--fail-on", "none", "--format", "json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Findings []lintFinding `json:"findings"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	return doc.Findings, nil
}

func newFindings(before, after []lintFinding) []lintFinding {
	seen := map[string]bool{}
	for _, f := range before {
		seen[f.ID+"\x00"+f.Object] = true
	}
	out := []lintFinding{}
	for _, f := range after {
		if !seen[f.ID+"\x00"+f.Object] {
			out = append(out, f)
		}
	}
	return out
}

func (v *verifier) verifyPGX(rep *verifyReport, run *verifyEngineRun, url, pgAfter string, comparable bool, work string) {
	f := false
	if url == "" {
		rep.PGXCompatible = &f
		if len(run.Unavailable) > 0 {
			rep.add("unsupported", "PGX is not available for this project: "+run.Error)
		} else {
			rep.add("error", "pgx branch unavailable: "+run.Error)
		}
		return
	}
	code := v.migrate(run, url)
	pgCode := 0
	if rep.Postgres.ExitCode != nil {
		pgCode = *rep.Postgres.ExitCode
	}
	compatible := code == pgCode
	if !compatible {
		rep.add("unsupported", fmt.Sprintf("the migration exited %d on PGX but %d on PostgreSQL", code, pgCode))
	}
	if !comparable {
		// Without a schema review there is no PGX comparison either: say so,
		// never report a match that was not checked.
		rep.PGXCompatible = nil
		if compatible {
			rep.add("incomplete", "PGX schema comparison did not run (no schema review)")
		} else {
			rep.PGXCompatible = &f
		}
		return
	}
	pgxAfter := filepath.Join(work, "pgx-after.json")
	if err := v.pgbotToFile(url, pgxAfter, "schema", "snapshot"); err != nil {
		rep.add("incomplete", "pgx schema snapshot failed: "+err.Error())
		rep.PGXCompatible = nil
		return
	}
	out, _, err := pgbot.Output(context.Background(), v.bin, "", "schema", "compare", pgxAfter, pgAfter,
		"--format", "json", "--a-label", "pgx", "--b-label", "postgres")
	if err != nil {
		rep.add("incomplete", "pgx schema compare failed: "+err.Error())
		rep.PGXCompatible = nil
		return
	}
	rep.PGXComparison = json.RawMessage(out)
	var cmp struct {
		Identical   bool              `json:"identical"`
		Differences []json.RawMessage `json:"differences"`
	}
	if json.Unmarshal(out, &cmp) != nil {
		rep.add("incomplete", "pgx schema compare returned an unreadable report")
		rep.PGXCompatible = nil
		return
	}
	if !cmp.Identical {
		compatible = false
		rep.add("unsupported", fmt.Sprintf("PGX produced a different schema than PostgreSQL (%d differences)", len(cmp.Differences)))
	}
	rep.PGXCompatible = &compatible
}

func (v *verifier) cleanup() []verifyCleanup {
	v.mu.Lock()
	names := append([]string{}, v.created...)
	v.mu.Unlock()
	out := []verifyCleanup{}
	for _, n := range names {
		c := verifyCleanup{Branch: n}
		if v.keep {
			c.Kept = true
		} else if _, _, err := v.client.DeleteBranch(context.Background(), v.project, n); err != nil {
			c.Error = err.Error()
			fmt.Fprintf(v.stderr, "pgrun: warning: could not delete verification branch %s (its %s TTL will): %v\n", n, v.ttl, err)
		} else {
			c.Deleted = true
		}
		out = append(out, c)
	}
	return out
}

// reviewSeverity is the highest severity in a pgbot schema diff report.
func reviewSeverity(diff []byte) string {
	var d struct {
		Summary struct{ Critical, Warning, Info int } `json:"summary"`
	}
	if json.Unmarshal(diff, &d) != nil {
		return ""
	}
	switch {
	case d.Summary.Critical > 0:
		return "critical"
	case d.Summary.Warning > 0:
		return "warning"
	case d.Summary.Info > 0:
		return "info"
	}
	return ""
}

func atOrAbove(sev, failOn string) bool {
	switch failOn {
	case "critical":
		return sev == "critical"
	case "warning":
		return sev == "critical" || sev == "warning"
	}
	return false
}

func summaryLine(diff []byte) string {
	var d struct {
		Summary  struct{ Critical, Warning, Info int } `json:"summary"`
		Findings []struct {
			ID, Severity, Object string
		} `json:"findings"`
	}
	if json.Unmarshal(diff, &d) != nil {
		return "unreadable"
	}
	var top []string
	for _, f := range d.Findings {
		if f.Severity != "info" && len(top) < 3 {
			top = append(top, f.ID+" "+f.Object)
		}
	}
	return fmt.Sprintf("%d critical, %d warning (%s)", d.Summary.Critical, d.Summary.Warning, strings.Join(top, "; "))
}

func printVerify(w io.Writer, rep verifyReport) {
	block := "not blocking"
	if rep.Blocking {
		block = "BLOCKING (policy: " + strings.Join(rep.Policy.BlockOn, ",") + ")"
	}
	fmt.Fprintf(w, "verify %s: %s — %s; categories: %s\n", rep.Project, strings.ToUpper(rep.Verdict), block, strings.Join(rep.Categories, ", "))
	if r := rep.Postgres; r != nil {
		code := "-"
		if r.ExitCode != nil {
			code = fmt.Sprint(*r.ExitCode)
		}
		fmt.Fprintf(w, "  %s branch %s: ready in %dms, migration exit %s in %dms\n", dash(r.Engine), r.Branch, r.ReadyMs, code, r.MigrateMs)
	}
	for _, reason := range rep.Reasons {
		fmt.Fprintf(w, "  - %s\n", reason)
	}
	if rep.SchemaReviewSkipped != "" {
		fmt.Fprintf(w, "  schema review skipped: %s\n", rep.SchemaReviewSkipped)
	} else if len(rep.SchemaReview) > 0 {
		var d struct {
			Findings []struct{ ID, Severity, Object, Detail, Advice string } `json:"findings"`
		}
		_ = json.Unmarshal(rep.SchemaReview, &d)
		for _, f := range d.Findings {
			if f.Severity == "info" {
				continue
			}
			fmt.Fprintf(w, "  %-8s %s %s\n           %s\n", f.Severity, f.ID, f.Object, f.Advice)
		}
	}
	for _, f := range rep.NewLintFindings {
		fmt.Fprintf(w, "  lint %-8s %s %s — %s\n", f.Severity, f.ID, f.Object, f.Title)
	}
	if rep.PGXCompatible != nil {
		fmt.Fprintf(w, "  pgx matches postgres: %v\n", *rep.PGXCompatible)
	}
	for _, c := range rep.Cleanup {
		state := "deleted"
		if c.Kept {
			state = "kept"
		} else if !c.Deleted {
			state = "NOT deleted: " + c.Error
		}
		fmt.Fprintf(w, "  cleanup %s: %s\n", c.Branch, state)
	}
	fmt.Fprintln(w, "  --json for the full report")
}
