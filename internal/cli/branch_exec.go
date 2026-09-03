package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/pgrundev/pgrun/internal/api"
)

const branchExecUsage = "usage: pgrun branch exec [<project>] <name> -- <command...>\n" +
	"   or: pgrun branch exec [<project>] --create <newname> [--ttl 1h|6h|24h|7d] [--from <parent>] [--delete-after] [--timeout 300s] -- <command...>"

// branchExec implements `pgrun branch exec`: the headline agent primitive.
// It resolves a branch's DATABASE_URL, runs a command with it set in the
// child process's environment (never on argv, never written to a file),
// passes stdio straight through, and exits with the command's own exit
// code. With --create it creates (and waits for) a fresh branch first, and
// with --delete-after it deletes that branch afterward — even if the
// command itself failed.
//
// Positional shape: an optional <project> (falling back to .pgrun/project,
// see resolveProject) may lead. Exactly one of a following <name> (use an
// existing ready branch) or --create <newname> (make a new one) selects the
// branch — never both, never neither. Everything after a literal "--" is
// the command to run (flag.FlagSet treats "--" as its own terminator, so
// this falls out of fs.Parse for free once the leading positionals are
// peeled off by hand).
func branchExec(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) > 2 {
		return usageErrf(stderr, "branch exec: unexpected argument %q — "+branchExecUsage, pos[2])
	}

	fs := flag.NewFlagSet("branch exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	create := fs.String("create", "", "create a fresh branch with this name before running the command")
	ttl := fs.String("ttl", "", "time-to-live for --create: 1h, 6h, 24h, or 7d")
	from := fs.String("from", "", "parent branch for --create (id, branch_<id>, or name)")
	deleteAfter := fs.Bool("delete-after", false, "delete the --create'd branch after the command runs, even if it fails")
	timeout := fs.Duration("timeout", 300*time.Second, "max time to wait for --create's branch to become ready")
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	command := fs.Args()

	// Resolve <project>/<name> from the leading positionals now that
	// --create is known: with --create, a lone positional is the project
	// (falling back to .pgrun/project when absent) and a second one is the
	// existing "cannot combine <name> with --create" error; without it, the
	// familiar two-positional <project> <name> (or one-positional <name>,
	// project from the fallback) pattern applies.
	var project, name string
	switch {
	case *create != "":
		if len(pos) == 2 {
			return usageErrf(stderr, "branch exec: cannot combine <name> (%q) with --create %q — "+branchExecUsage, pos[1], *create)
		}
		explicit := ""
		if len(pos) == 1 {
			explicit = pos[0]
		}
		var code int
		var ok bool
		project, code, ok = resolveProject(explicit, stderr)
		if !ok {
			return code
		}
	case len(pos) == 2:
		project, name = pos[0], pos[1]
	case len(pos) == 1:
		name = pos[0]
		var code int
		var ok bool
		project, code, ok = resolveProject("", stderr)
		if !ok {
			return code
		}
	default: // len(pos) == 0, no --create
		return usageErrf(stderr, "branch exec: either <name> or --create <newname> is required — "+branchExecUsage)
	}

	if *create == "" && (*ttl != "" || *from != "" || *deleteAfter) {
		return usageErrf(stderr, "branch exec: --ttl/--from/--delete-after require --create")
	}
	if *ttl != "" && !api.ValidTTL(*ttl) {
		return usageErrf(stderr, "branch exec: --ttl must be one of 1h, 6h, 24h, 7d")
	}
	if len(command) == 0 {
		return usageErrf(stderr, "branch exec: missing a command after -- — "+branchExecUsage)
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	if *create == "" {
		return execAgainstExistingBranch(client, project, name, command, stdout, stderr)
	}
	return execAgainstCreatedBranch(client, project, *create, *ttl, *from, *deleteAfter, *timeout, command, stdout, stderr)
}

// execAgainstExistingBranch is the plain `branch exec <project> <name> --
// <command>` path: the branch must already exist and be ready.
func execAgainstExistingBranch(client *api.Client, project, name string, command []string, stdout, stderr io.Writer) int {
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
	return runCommandWithDatabaseURL(command, branch.ConnectionURL, stdout, stderr)
}

// execAgainstCreatedBranch is the `--create` path: create, wait for ready,
// run the command, and — if deleteAfter — delete the branch afterward. The
// delete is registered via defer as soon as the branch is known to exist
// (right after a successful CreateBranch), so it fires on every exit from
// that point on: a failed/timed-out wait, or any exit code from the
// command. That's stronger than the spec's minimum ("even on command
// failure") on purpose — an agent that asked for cleanup shouldn't leak a
// branch that got stuck mid-provision either; the branch's TTL is the
// backstop of last resort, not the plan.
func execAgainstCreatedBranch(client *api.Client, project, name, ttl, parent string, deleteAfter bool, timeout time.Duration, command []string, stdout, stderr io.Writer) (code int) {
	raw, branch, err := client.CreateBranch(context.Background(), project, api.CreateBranchRequest{
		Name: name, TTL: ttl, ParentBranchID: parent,
	})
	if err != nil {
		return handleAPIError(err, false, raw, stdout, stderr)
	}

	if deleteAfter {
		defer func() {
			if _, _, delErr := client.DeleteBranch(context.Background(), project, name); delErr != nil {
				fmt.Fprintf(stderr, "pgrun: warning: failed to delete branch %s after exec: %v\n", name, delErr)
			}
		}()
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, branch, err = client.WaitForTerminal(waitCtx, project, raw, branch)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "pgrun: timed out after %s waiting for branch %s to become ready (last status: %s)\n", timeout, branch.Name, branch.Status)
			return exitFailure
		}
		return handleAPIError(err, false, raw, stdout, stderr)
	}
	if branch.Status != api.StatusReady {
		fmt.Fprintf(stderr, "pgrun: %s\n", api.WaitFailureReason(branch.Name, branch.Status))
		return exitFailure
	}
	if branch.ConnectionURL == "" {
		fmt.Fprintf(stderr, "pgrun: branch %s is ready but has no connection_url yet\n", branch.Name)
		return exitFailure
	}

	return runCommandWithDatabaseURL(command, branch.ConnectionURL, stdout, stderr)
}

// runCommandWithDatabaseURL execs command with DATABASE_URL set in its
// environment only — never appended to argv (where it would show up in
// `ps`), never written to a file. Stdout/stderr are the writers Run was
// given (os.Stdout/os.Stderr in production, so a real terminal command
// behaves exactly as if it had been run directly); stdin is the process's
// real os.Stdin, matching how `pgrun mcp serve` also reaches past Run's
// stdout/stderr-only signature for the one thing that needs more.
func runCommandWithDatabaseURL(command []string, databaseURL string, stdout, stderr io.Writer) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "DATABASE_URL="+databaseURL)

	err := cmd.Run()
	if err == nil {
		return exitSuccess
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(stderr, "pgrun: branch exec: %v\n", err)
	return exitFailure
}
