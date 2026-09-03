package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/pgrundev/pgrun/internal/api"
)

// branchEnv implements `pgrun branch env [<project>] <name>`: the
// credential-injection helper for shells, printing an eval-able
// `export DATABASE_URL="..."` line (or `--format=json`'s `{"database_url":
// "..."}`). Only ever succeeds once the branch is ready and credentialed —
// same reveal class as `branch url`, just shaped for `eval "$(...)"`
// instead of parsing `DATABASE_URL=...` text.
func branchEnv(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) == 0 {
		return usageErrf(stderr, "branch env: missing <name> — usage: pgrun branch env [<project>] <name> [--format=json]")
	}
	if len(pos) > 2 {
		return usageErrf(stderr, "branch env: unexpected argument %q — usage: pgrun branch env [<project>] <name> [--format=json]", pos[2])
	}
	var explicit, name string
	if len(pos) == 2 {
		explicit, name = pos[0], pos[1]
	} else {
		name = pos[0]
	}
	project, code, ok := resolveProject(explicit, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("branch env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", `output format: "json" for {"database_url":"..."} instead of an export line`)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "branch env: unexpected argument %q", fs.Args()[0])
	}
	if *format != "" && *format != "json" {
		return usageErrf(stderr, "branch env: --format must be \"json\"")
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	// Like `branch url`, this never prints raw API JSON — an export line or
	// {"database_url":...} on success, a sanitized reason otherwise.
	_, branch, err := client.GetBranch(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, false, nil, stdout, stderr)
	}
	if branch.Status != api.StatusReady {
		fmt.Fprintf(stderr, "pgrun: branch %s is not ready (status: %s)\n", name, branch.Status)
		return exitFailure
	}
	if branch.ConnectionURL == "" {
		fmt.Fprintf(stderr, "pgrun: branch %s is ready but not yet credentialed\n", name)
		return exitFailure
	}

	if *format == "json" {
		encoded, err := json.Marshal(map[string]string{"database_url": branch.ConnectionURL})
		if err != nil {
			fmt.Fprintf(stderr, "pgrun: %v\n", err)
			return exitFailure
		}
		dumpJSON(stdout, encoded)
		return exitSuccess
	}
	fmt.Fprintf(stdout, "export DATABASE_URL=%s\n", shellDoubleQuote(branch.ConnectionURL))
	return exitSuccess
}

// shellDoubleQuote wraps s in POSIX double quotes, escaping the four
// characters double quotes don't neutralize on their own (\, ", $, `) so
// the result is safe to `eval` even though it isn't as airtight as single
// quotes — double quotes are what `branch env`'s output contract specifies
// (an `export DATABASE_URL="..."` line), since single-quoting would break
// if the URL ever needs interpolating into another double-quoted context.
func shellDoubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"', '$', '`':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
