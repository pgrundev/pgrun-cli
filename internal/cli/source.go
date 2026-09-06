// This file implements `pgrun source ...`: Connect Postgres -> Protect
// Data -> Create Safe Copy, the CLI surface for the sources API added in
// internal/api/sources.go. list/get/status are implemented here; add,
// update, protect, and copy are placeholders for later tasks. Same
// conventions as branch.go: leadingPositionals for the optional [<name>]
// positional, resolveProject/resolveOrHint for config resolution,
// handleAPIError/dumpJSON for API errors and --json passthrough. Per
// ruling 1, a source's name IS a project slug — sourceNameArgs falls back
// through the same .pgrun/project file branch commands use.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// runSource dispatches `pgrun source <subcommand>`. It accepts stdin even
// though list/get/status never read it — Task 3's `source add` (no --url)
// needs a hidden-prompt read from it, and giving all subcommands the same
// signature now avoids reshaping this dispatcher later.
func runSource(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErrf(stderr, "source: expected a subcommand (list, get, status, add, update, protect, copy)")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		return sourceList(rest, stdout, stderr)
	case "get":
		return sourceGet(rest, stdout, stderr)
	case "status":
		return sourceStatus(rest, stdout, stderr)
	case "add":
		return sourceAdd(rest, stdin, stdout, stderr)
	case "update":
		return sourceUpdate(rest, stdin, stdout, stderr)
	case "protect":
		return sourceProtect(rest, stdout, stderr)
	case "copy":
		// Task 5 replaces this with a real implementation.
		return usageErrf(stderr, "source %s: not implemented yet", sub)
	default:
		return usageErrf(stderr, "source: unknown subcommand %q", sub)
	}
}

// sourceUsageFlags returns the "[--flag ...]" text that follows "[<name>]"
// in a source subcommand's usage line — one entry per line in cli.go's
// usage block, so a "too many arguments" error always shows the flags that
// command actually accepts, not a generic "[--json]" for every one of
// them. Unrecognized cmd values (there shouldn't be any) fall back to
// "[--json]", the smallest common denominator every source subcommand has.
func sourceUsageFlags(cmd string) string {
	switch cmd {
	case "update":
		return `[--url <postgres://…>] [--wait] [--timeout 120s] [--json]`
	case "protect":
		return `[--set <table>.<column>=copy|fake|null ...] [--acknowledge <table>.<column> ...] [--approve] [--review] [--json]`
	case "copy":
		return `[--wait] [--timeout 30m] [--json]`
	case "get", "status":
		return `[--json]`
	default:
		return `[--json]`
	}
}

// sourceNameArgs peels the optional leading [<name>] positional shared by
// get/status/update/protect/copy and resolves it exactly like a branch
// command's [<project>] (ruling 1: a source name is a project slug). cmd
// is the bare subcommand name (e.g. "get", "copy") — used both to build
// the usage-error message and, via sourceUsageFlags, to pick the right
// flags text for it.
func sourceNameArgs(cmd string, args []string, stderr io.Writer) (name string, rest []string, code int, ok bool) {
	pos, rest := leadingPositionals(args)
	if len(pos) > 1 {
		// redactedArg, not %q: `pgrun source update <name> <url>` (the URL
		// typed without --url) reaches here, and echoing it back would leak
		// the password before any redactor exists — see redact.go.
		return "", nil, usageErrf(stderr, "source %s: unexpected argument %s — usage: pgrun source %s [<name>] %s", cmd, redactedArg(pos[1]), cmd, sourceUsageFlags(cmd)), false
	}
	explicit := ""
	if len(pos) == 1 {
		explicit = pos[0]
	}
	// A connection URL where the name goes (`pgrun source update
	// postgres://…`) is a plausible slip, and an expensive one: the name
	// becomes a URL path segment, so the secret would be sent to — and
	// logged by — the API, then echoed back in the 404. Refused here,
	// before any request, without repeating the value.
	if looksLikeConnectionURL(explicit) {
		return "", nil, usageErrf(stderr, "source %s: that argument is the Production Database name, not a connection URL — usage: pgrun source %s [<name>] %s", cmd, cmd, sourceUsageFlags(cmd)), false
	}
	name, code, ok = resolveProject(explicit, stderr)
	if !ok {
		return "", nil, code, false
	}
	return name, rest, exitSuccess, true
}

func sourceList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("source list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source list: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, sources, err := client.ListSources(context.Background())
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	if len(sources) == 0 {
		fmt.Fprintln(stderr, `no production databases yet — run `+"`"+`pgrun source add --name <n> --url "$DATABASE_URL"`+"`")
		return exitSuccess
	}
	writeSourceTable(stdout, sources)
	return exitSuccess
}

func sourceGet(args []string, stdout, stderr io.Writer) int {
	name, rest, code, ok := sourceNameArgs("get", args, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("source get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source get: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, src, err := client.GetSource(context.Background(), name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	fmt.Fprintln(stdout, sourceLine(src)) // never connection_url — the source object doesn't carry one
	return exitSuccess
}

func sourceStatus(args []string, stdout, stderr io.Writer) int {
	name, rest, code, ok := sourceNameArgs("status", args, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("source status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source status: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, src, err := client.GetSource(context.Background(), name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
	} else {
		fmt.Fprint(stdout, sourceStatusView(src)) // never connection_url
	}
	// Ruling 4: status exits 1 when the source needs attention, 0
	// otherwise — regardless of --json, which only changes how the
	// outcome is reported.
	if sourceActionable(src) {
		return exitFailure
	}
	return exitSuccess
}
