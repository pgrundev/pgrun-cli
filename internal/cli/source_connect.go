// This file implements the two `pgrun source ...` subcommands that carry a
// connection URL: `add` (POST /api/v1/sources) and `update` (PATCH
// /api/v1/sources/<name>). They share everything except where the name
// comes from and which API call they make, so the flag set, the URL
// acquisition (flag or hidden prompt), and the post-call connection check
// all live here once.
//
// The secret discipline is redact.go's: the URL is validated client-side
// without ever repeating its value, then — before the first API call —
// stdout and stderr are replaced with redacting writers, so nothing
// downstream can print it. See redact.go for why that is structural rather
// than a rule to remember.

package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// connectFlags is the flag set `source add` and `source update` share.
//
// Note --url: on these two commands it is the *connection* URL, per the
// documented usage (`pgrun source add --name <n> [--url <postgres://…>]`),
// so they cannot also use addSourceAuthFlags' --api-url flag under the name
// --url for the API base URL — hence addSourceAuthFlags (shared with every
// other source subcommand) registers the override as --api-url here too;
// --token is unchanged, and PGRUN_API_URL/config-file resolution work
// exactly as everywhere else.
type connectFlags struct {
	url     *string
	wait    *bool
	timeout *time.Duration
	json    *bool
	apiURL  *string
	token   *string
}

func addConnectFlags(fs *flag.FlagSet) connectFlags {
	apiURL, token := addSourceAuthFlags(fs)
	return connectFlags{
		url:     fs.String("url", "", "connection URL for the production database (omit for a hidden prompt)"),
		wait:    fs.Bool("wait", false, "poll until the connection check settles"),
		timeout: fs.Duration("timeout", 120*time.Second, "max time to wait with --wait"),
		json:    addJSONFlag(fs),
		apiURL:  apiURL,
		token:   token,
	}
}

// validConnectionURL is the client-side scheme check for ACCEPTING a URL:
// a fast, clear refusal instead of a round trip, and the only inspection of
// the value the CLI does before handing it to the API. Deliberately strict
// about case — "POSTGRES://…" is refused here with the same value-free
// message any other wrong scheme gets.
func validConnectionURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

// looksLikeConnectionURL is the check for REFUSING to print or transmit a
// value that might be a connection URL. It must fail closed where
// validConnectionURL fails open: a guard that lets "POSTGRES://u:pw@h/db"
// through would put that secret in a request path (and the API's access
// log), or echo it back in a usage error.
func looksLikeConnectionURL(s string) bool {
	return validConnectionURL(strings.ToLower(s))
}

// readConnectionURL prompts for the URL with input hidden, reusing
// auth login's readToken — which disables terminal echo only when stdin is
// the real os.Stdin, and otherwise reads a plain stream (that's what makes
// this testable without a TTY).
func readConnectionURL(stdin io.Reader, stdout, stderr io.Writer) (string, error) {
	return readToken(bufio.NewReader(stdin), stdin, stdout, stderr, "Connection URL (input hidden): ")
}

// connectionURL resolves the URL for one invocation: the --url flag when
// given, otherwise the hidden prompt. Every failure here is a usage error
// (exit 64) that names the rule and never the value. cmd is "add" or
// "update", used only to build those messages.
func (f connectFlags) connectionURL(cmd string, stdin io.Reader, stdout, stderr io.Writer) (string, int, bool) {
	connURL := strings.TrimSpace(*f.url)
	if connURL == "" {
		if *f.json {
			return "", usageErrf(stderr, "source %s: --url is required with --json (the URL prompt would corrupt the JSON output)", cmd), false
		}
		var err error
		connURL, err = readConnectionURL(stdin, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "pgrun: reading the connection URL: %v\n", err)
			return "", exitFailure, false
		}
		if connURL == "" {
			return "", usageErrf(stderr, "a connection URL is required"), false
		}
	}
	if !validConnectionURL(connURL) {
		return "", usageErrf(stderr, "source %s: the connection URL must start with postgres:// or postgresql://", cmd), false
	}
	return connURL, exitSuccess, true
}

func sourceAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("source add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "name for the production database (required)")
	f := addConnectFlags(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source add: unexpected argument %s — usage: pgrun source add --name <n> [--url <postgres://…>] [--wait] [--timeout 120s] [--json]", redactedArg(fs.Args()[0]))
	}
	if *name == "" {
		return usageErrf(stderr, "source add: --name is required")
	}
	// Same slip as in sourceNameArgs: the URL handed to --name would end up
	// in a request body (and a server log) as a name, outside the redactor
	// this command builds from --url.
	if looksLikeConnectionURL(*name) {
		return usageErrf(stderr, "source add: --name is the Production Database name, not a connection URL — pass the URL with --url")
	}
	connURL, code, ok := f.connectionURL("add", stdin, stdout, stderr)
	if !ok {
		return code
	}

	// From here on this process holds a secret, so everything it can print
	// is scrubbed of it — installed before the first API call, and passed
	// to every writer-taking helper below.
	red := newRedactor(connURL)
	stdout, stderr = redactWriter(stdout, red), redactWriter(stderr, red)
	fs.SetOutput(stderr) // every writer from here on is a redacting one

	cfg, code, ok := resolveOrHint(*f.apiURL, *f.token, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, src, err := client.CreateSource(context.Background(), *name, connURL)
	if err != nil {
		return handleAPIError(err, *f.json, raw, stdout, stderr)
	}
	return checkConnection(client, f, *name, fmt.Sprintf("✓ Production database %s added", *name), raw, src, stdout, stderr)
}

func sourceUpdate(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name, rest, code, ok := sourceNameArgs("update", args, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("source update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := addConnectFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source update: unexpected argument %s", redactedArg(fs.Args()[0]))
	}
	connURL, code, ok := f.connectionURL("update", stdin, stdout, stderr)
	if !ok {
		return code
	}

	red := newRedactor(connURL)
	stdout, stderr = redactWriter(stdout, red), redactWriter(stderr, red)
	fs.SetOutput(stderr) // every writer from here on is a redacting one

	cfg, code, ok := resolveOrHint(*f.apiURL, *f.token, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, src, err := client.UpdateSourceURL(context.Background(), name, connURL)
	if err != nil {
		return handleAPIError(err, *f.json, raw, stdout, stderr)
	}
	return checkConnection(client, f, name, fmt.Sprintf("✓ Connection URL updated for %s", name), raw, src, stdout, stderr)
}

// checkConnection reports on the connection check both commands kick off.
// Without --wait it prints headline plus the one command to run next and
// stops (the check is asynchronous — ruling 2). With --wait it polls until
// the check settles: connected prints the full status view (exit 0), a
// failed check prints the reason and how to supply a different URL (exit
// 1), and running out of --timeout says so without pretending either way
// (exit 1). stdout/stderr are the callers' redacting writers.
func checkConnection(client *api.Client, f connectFlags, name, headline string, raw []byte, src api.Source, stdout, stderr io.Writer) int {
	jsonOut := *f.json

	if !*f.wait {
		if jsonOut {
			dumpJSON(stdout, raw)
			return exitSuccess
		}
		fmt.Fprintln(stdout, headline)
		fmt.Fprintln(stdout, "→ Checking connection (read-only, nothing is copied)")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Next:")
		// sourceNext, not a literal: a source that has just been handed a
		// URL is "checking", for which sourceNext already names `pgrun
		// source status <name>` — one definition, so the two can't drift.
		fmt.Fprintf(stdout, "  %s\n", sourceNext(src))
		return exitSuccess
	}

	ctx, cancel := context.WithTimeout(context.Background(), *f.timeout)
	defer cancel()
	raw, src, err := client.WaitForSource(ctx, name, raw, src, api.CheckSettled)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if jsonOut {
				dumpJSON(stdout, raw)
			}
			fmt.Fprintf(stderr, "pgrun: still checking %s after %s — run `pgrun source status %s`\n", name, *f.timeout, name)
			return exitFailure
		}
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}

	// The check settled. A failure keeps the source in "checking" with
	// last_check_error set, which is the one state a new URL can fix.
	if api.CheckFailed(src) {
		if jsonOut {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: connection failed (%s).\n\nUpdate the connection URL and try again:\n  pgrun source update %s --url \"$DATABASE_URL\"\n", src.LastCheckError, name)
		return exitFailure
	}

	if jsonOut {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	fmt.Fprintf(stdout, "%s\n\n", headline)
	fmt.Fprint(stdout, sourceStatusView(src))
	return exitSuccess
}
