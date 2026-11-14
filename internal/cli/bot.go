package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
	"github.com/pgrundev/pgrun-cli/internal/pgbot"
)

// `pgrun bot …` makes pgbot (read-only Postgres inspection) a standard part
// of pgrun. Every per-branch command resolves a PGRun branch through the API
// with the project-scoped token and hands its URL to pgbot in the
// environment only. `pgrun bot` never accepts a connection URL of its own:
// it inspects PGRun branches, nothing else, so it cannot be pointed at a
// production database. pgbot's optional AI layer (ask/explain/--ai) sends
// findings to an external model and is refused unless --allow-ai is given.

const botUsage = `pgrun bot — pgbot, read-only inspection and verification of PGRun branches

Usage:
  pgrun bot status [--json]                         is pgbot installed, which version, what it can do
  pgrun bot install [--version <v>] [--force] [--json]
  pgrun bot inspect [<project>] <branch> [--json]   health findings (pgbot inspect --no-store)
  pgrun bot lint [<project>] <branch> [--json]      schema-only findings (safe on any branch)
  pgrun bot schema [<project>] <branch>             schema snapshot as JSON (catalog only, no data)
  pgrun bot explain [<project>] <branch> "<SELECT …>" [--json]   plan only, never executed
  pgrun bot run [<project>] <branch> [--allow-ai] -- <pgbot args…>
  pgrun bot verify [<project>] [--from <parent>] [--engine postgres|pgx] [--compare-pgx]
                   [--ttl 1h] [--keep] [--fail-on critical|warning|none] [--timeout 300s] [--json]
                   -- <migration command…>
`

func runBot(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, botUsage)
		return exitUsage
	}
	switch args[0] {
	case "status":
		return botStatus(args[1:], stdout, stderr)
	case "install":
		return botInstall(args[1:], stdout, stderr)
	case "inspect", "lint", "schema", "explain", "run":
		return botBranchCommand(args[0], args[1:], stdout, stderr)
	case "verify":
		return botVerify(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, botUsage)
		return exitSuccess
	default:
		fmt.Fprintf(stderr, "pgrun: bot: unknown subcommand %s\n\n", redactedArg(args[0]))
		fmt.Fprint(stderr, botUsage)
		return exitUsage
	}
}

// botStatusDoc is `pgrun bot status --json` and the "pgbot" entry of `pgrun setup --json`.
type botStatusDoc struct {
	Installed     bool                 `json:"installed"`
	Binary        *pgbot.Binary        `json:"binary,omitempty"`
	MinVersion    string               `json:"min_version"`
	PinnedVersion string               `json:"pinned_version"`
	ManagedPath   string               `json:"managed_path"`
	Install       *pgbot.InstallResult `json:"install,omitempty"`
	Action        string               `json:"action,omitempty"` // found | installed | updated | failed | skipped
	Error         string               `json:"error,omitempty"`
	Hint          string               `json:"hint,omitempty"`
}

func botStatusNow(ctx context.Context) botStatusDoc {
	doc := botStatusDoc{MinVersion: pgbot.MinVersion, PinnedVersion: pgbot.PinnedVersion}
	doc.ManagedPath, _ = pgbot.ManagedPath()
	b, err := pgbot.Locate(ctx)
	if err == nil {
		doc.Installed = b.Compatible
		doc.Binary = &b
		if !b.Compatible {
			doc.Hint = "run `pgrun bot install` to install pgbot " + pgbot.PinnedVersion
		} else if !b.Capabilities.SchemaReview {
			doc.Hint = "this pgbot has no `schema` command: `pgrun bot verify` runs the migration and pgbot lint but skips the rewrite/lock review"
		}
		return doc
	}
	doc.Hint = "run `pgrun bot install` (or: brew install pgrundev/tap/pgbot, npm i -g @pgbot/cli)"
	return doc
}

func botStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bot status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := addJSONFlag(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	doc := botStatusNow(context.Background())
	if *jsonOut {
		writeJSONDoc(stdout, doc)
	} else {
		printBotStatus(stdout, doc)
	}
	if !doc.Installed {
		return exitFailure
	}
	return exitSuccess
}

func printBotStatus(w io.Writer, doc botStatusDoc) {
	switch {
	case doc.Binary == nil:
		fmt.Fprintf(w, "pgbot: not installed — %s\n", doc.Hint)
	case !doc.Binary.Compatible:
		fmt.Fprintf(w, "pgbot: incompatible at %s — %s\n", doc.Binary.Path, doc.Binary.Problem)
	default:
		review := "yes"
		if !doc.Binary.Capabilities.SchemaReview {
			review = "no (needs a pgbot with `schema`)"
		}
		fmt.Fprintf(w, "pgbot: %s at %s (%s)\n  migration schema review: %s\n", doc.Binary.Version, doc.Binary.Path, doc.Binary.Source, review)
	}
}

func botInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bot install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", pgbot.PinnedVersion, "pgbot release to install")
	force := fs.Bool("force", false, "download even when a compatible pgbot is already installed")
	jsonOut := addJSONFlag(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	doc := ensureBot(context.Background(), *version, *force)
	if *jsonOut {
		writeJSONDoc(stdout, doc)
	} else if doc.Error != "" {
		fmt.Fprintf(stderr, "pgrun: bot install: %s\n  %s\n", doc.Error, doc.Hint)
	} else {
		if doc.Action == "found" {
			fmt.Fprintf(stdout, "pgbot %s already installed at %s — nothing downloaded (--force to install pgrun's own copy)\n", doc.Binary.Version, doc.Binary.Path)
		} else {
			fmt.Fprintf(stdout, "%s pgbot %s at %s (sha256 %s verified)\n", doc.Action, doc.Install.Version, doc.Install.Path, doc.Install.SHA256[:12])
		}
	}
	if doc.Error != "" {
		return exitFailure
	}
	return exitSuccess
}

// ensureBot is the one installation routine shared by `pgrun bot install` and
// `pgrun setup`: keep a compatible pgbot that is already there (no duplicate
// binary), otherwise download the pinned release into pgrun's data dir.
func ensureBot(ctx context.Context, version string, force bool) botStatusDoc {
	doc := botStatusNow(ctx)
	if doc.Installed && !force && (version == "" || version == pgbot.PinnedVersion) {
		doc.Action = "found"
		return doc
	}
	res, err := pgbot.Install(ctx, version, "", nil)
	if err != nil {
		doc.Action = "failed"
		doc.Error = err.Error()
		doc.Hint = "install pgbot another way — brew install pgrundev/tap/pgbot · npm i -g @pgbot/cli · curl -fsSL https://pgbot.dev/install | sh — then re-run `pgrun bot status`"
		return doc
	}
	after := botStatusNow(ctx)
	after.Install = &res
	after.Action = "installed"
	if res.Replaced {
		after.Action = "updated"
	}
	if after.Binary != nil && after.Binary.Path != res.Path {
		// $PGRUN_PGBOT or a compatible pgbot on PATH still wins the lookup; say so.
		after.Hint = fmt.Sprintf("installed to %s, but %s (%s) is found first", res.Path, after.Binary.Path, after.Binary.Source)
	}
	return after
}

func writeJSONDoc(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// requireBot locates a compatible pgbot or prints why not.
func requireBot(ctx context.Context, stderr io.Writer) (pgbot.Binary, bool) {
	b, err := pgbot.Locate(ctx)
	if errors.Is(err, pgbot.ErrNotFound) {
		fmt.Fprintln(stderr, "pgrun: pgbot is not installed — run `pgrun bot install` (or `pgrun setup`)")
		return b, false
	}
	if !b.Compatible {
		fmt.Fprintf(stderr, "pgrun: %s\n", b.Problem)
		return b, false
	}
	return b, true
}

// aiCommands are pgbot invocations that call an external model.
func refusesAI(args []string) string {
	for i, a := range args {
		if a == "--" {
			break
		}
		if i == 0 && (a == "ask" || a == "explain") {
			return "pgbot " + a + " sends findings to an external AI model"
		}
		if a == "--ai" {
			return "--ai sends the query text to an external AI model"
		}
	}
	return ""
}

func botBranchCommand(sub string, args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	maxPos := 2
	if sub == "explain" {
		maxPos = 3
	}
	if len(pos) > maxPos {
		return usageErrf(stderr, "bot %s: unexpected argument %s", sub, redactedArg(pos[maxPos]))
	}
	fs := flag.NewFlagSet("bot "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "machine-readable output (pgbot's versioned JSON)")
	allowAI := fs.Bool("allow-ai", false, "run: allow pgbot's AI layer (sends findings or SQL to the model provider whose key is in your environment)")
	timeout := fs.Duration("timeout", 2*time.Minute, "wall-clock budget for pgbot")
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	extra := fs.Args()

	var project, branch, sql string
	if sub == "explain" {
		// the SQL is the last positional
		if len(pos) < 2 {
			return usageErrf(stderr, `bot explain: expected [<project>] <branch> "<SELECT …>"`)
		}
		sql, pos = pos[len(pos)-1], pos[:len(pos)-1]
	}
	switch len(pos) {
	case 2:
		project, branch = pos[0], pos[1]
	case 1:
		branch = pos[0]
		var code int
		var ok bool
		if project, code, ok = resolveProject("", rest, stderr); !ok {
			return code
		}
	default:
		return usageErrf(stderr, "bot %s: expected [<project>] <branch>", sub)
	}
	if sub == "run" && len(extra) == 0 {
		return usageErrf(stderr, "bot run: missing pgbot arguments after --")
	}
	if sub != "run" && len(extra) > 0 {
		return usageErrf(stderr, "bot %s: unexpected argument %s (use `pgrun bot run … -- <pgbot args>` for anything else)", sub, redactedArg(extra[0]))
	}
	for _, a := range append(append([]string{}, extra...), sql) {
		if containsConnectionURL(a) || strings.HasPrefix(a, "service=") {
			return usageErrf(stderr, "bot %s: pgrun bot only inspects PGRun branches — it never takes a connection string (run pgbot directly for other databases)", sub)
		}
	}
	if sub == "run" && !*allowAI {
		if why := refusesAI(extra); why != "" {
			return usageErrf(stderr, "bot run: %s; add --allow-ai if that is what you want", why)
		}
	}

	var pgbotArgs []string
	switch sub {
	case "inspect":
		pgbotArgs = []string{"inspect", "--no-store"}
		if *jsonOut {
			pgbotArgs = append(pgbotArgs, "--json")
		}
	case "lint":
		pgbotArgs = []string{"lint", "--fail-on", "none"}
		if *jsonOut {
			pgbotArgs = append(pgbotArgs, "--format", "json")
		}
	case "schema":
		pgbotArgs = []string{"schema", "snapshot"}
	case "explain":
		pgbotArgs = []string{"sql", "explain"}
		if *jsonOut {
			pgbotArgs = append(pgbotArgs, "--json")
		}
		pgbotArgs = append(pgbotArgs, sql)
	case "run":
		pgbotArgs = extra
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	bin, ok := requireBot(ctx, stderr)
	if !ok {
		return exitFailure
	}
	if sub == "schema" && !bin.Capabilities.SchemaReview {
		fmt.Fprintf(stderr, "pgrun: pgbot %s has no `schema` command — install a newer pgbot\n", bin.Version)
		return exitFailure
	}
	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)
	_, b, err := client.GetBranch(ctx, project, branch)
	if err != nil {
		return handleAPIError(err, false, nil, stdout, stderr)
	}
	if b.Status != api.StatusReady || b.ConnectionURL == "" {
		fmt.Fprintf(stderr, "pgrun: branch %s is not ready (status: %s)\n", branch, b.Status)
		return exitFailure
	}
	red := newRedactor(b.ConnectionURL)
	cmd := pgbot.Command(ctx, bin, b.ConnectionURL, *allowAI, pgbotArgs...)
	cmd.Stdout, cmd.Stderr = redactWriter(stdout, red), redactWriter(stderr, red)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode() // pgbot's own exit contract (1 warn, 2 critical, 3 connection)
		}
		fmt.Fprintf(stderr, "pgrun: run pgbot: %v\n", err)
		return exitFailure
	}
	return exitSuccess
}
