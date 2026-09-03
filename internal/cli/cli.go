// Package cli implements the pgrun command-line interface: flag parsing,
// dispatch, config resolution, and human/--json output. main.go is a thin
// wrapper around Run so the whole CLI is testable without exec'ing a binary.
package cli

import (
	"fmt"
	"io"
)

// Exit codes are a public interface (scripts and agents depend on them):
// 0 success, 1 operation/API failure, 2 auth/config missing, 64 usage.
const (
	exitSuccess = 0
	exitFailure = 1
	exitAuth    = 2
	exitUsage   = 64
)

// Version is overridden at release build time via
// -ldflags "-X github.com/pgrundev/pgrun/internal/cli.Version=...".
var Version = "dev"

const usage = `pgrun — create, use, and delete disposable PGRun database branches

Usage:
  pgrun branch create <project> --name <n> [--ttl 1h|6h|24h|7d] [--parent <name>] [--wait] [--timeout 300s] [--json]
  pgrun branch list <project> [--json]
  pgrun branch get <project> <name> [--json]      (alias: status)
  pgrun branch url <project> <name>
  pgrun branch env <project> <name> [--format=json]
  pgrun branch exec <project> <name> -- <command...>
  pgrun branch exec <project> --create <newname> [--ttl 1h|6h|24h|7d] [--from <parent>] [--delete-after] [--timeout 300s] -- <command...>
  pgrun branch delete <project> <name> [--json]
  pgrun auth login
  pgrun auth logout
  pgrun auth set --token <token> [--url <url>]    (advanced/CI — see PGRUN_API_TOKEN/PGRUN_API_URL below)
  pgrun auth status
  pgrun skill install [--project | --dir <dir>]   (install the pgrun-branching skill for Claude Code; default ~/.claude/skills)
  pgrun skill status [--project | --dir <dir>]
  pgrun skill show
  pgrun mcp serve
  pgrun version

Config resolution (highest wins): --url/--token flags > PGRUN_API_URL/PGRUN_API_TOKEN env > ~/.config/pgrun/config.json
mcp serve reads config from PGRUN_API_URL/PGRUN_API_TOKEN env only (no flags, no config file).
`

// Run parses args (excluding the program name), executes the command, and
// returns the process exit code. It never calls os.Exit itself.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	switch args[0] {
	case "branch":
		return runBranch(args[1:], stdout, stderr)
	case "auth":
		return runAuth(args[1:], stdout, stderr)
	case "skill":
		return runSkill(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "pgrun %s\n", Version)
		return exitSuccess
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitSuccess
	default:
		fmt.Fprintf(stderr, "pgrun: unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
}

// usageErrf writes a usage error to stderr and returns exitUsage — the
// standard return path for every malformed-invocation case.
func usageErrf(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "pgrun: "+format+"\n", a...)
	return exitUsage
}

// authHint is appended whenever a command fails for lack of (or rejection
// of) credentials, exit code 2. Leads with `auth login` — the everyday
// interactive path — but keeps mentioning `auth set` by name (scripts and
// existing docs point at that exact phrase; it's also the one CI should
// actually use, since it never touches a terminal).
const authHint = "run `pgrun auth login` (or `pgrun auth set --token <TOKEN> [--url <URL>]` for CI/scripts)"
