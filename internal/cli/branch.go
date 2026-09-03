package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/pgrundev/pgrun/internal/api"
)

func runBranch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErrf(stderr, "branch: expected a subcommand (create, list, get, url, env, exec, delete)")
	}
	switch args[0] {
	case "create":
		return branchCreate(args[1:], stdout, stderr)
	case "list":
		return branchList(args[1:], stdout, stderr)
	case "get", "status":
		return branchGet(args[1:], stdout, stderr)
	case "url":
		return branchURL(args[1:], stdout, stderr)
	case "env":
		return branchEnv(args[1:], stdout, stderr)
	case "exec":
		return branchExec(args[1:], stdout, stderr)
	case "delete":
		return branchDelete(args[1:], stdout, stderr)
	default:
		return usageErrf(stderr, "branch: unknown subcommand %q", args[0])
	}
}

func branchCreate(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) > 1 {
		return usageErrf(stderr, "branch create: unexpected argument %q — usage: pgrun branch create [<project>] --name <n> [--ttl 1h|6h|24h|7d] [--parent <name>] [--wait] [--timeout 300s] [--json]", pos[1])
	}
	explicit := ""
	if len(pos) == 1 {
		explicit = pos[0]
	}
	project, code, ok := resolveProject(explicit, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("branch create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "branch name (required)")
	ttl := fs.String("ttl", "", "time-to-live: 1h, 6h, 24h, or 7d")
	parent := fs.String("parent", "", "parent branch (id, branch_<id>, or name)")
	wait := fs.Bool("wait", false, "poll until the branch is ready or failed")
	timeout := fs.Duration("timeout", 300*time.Second, "max time to wait with --wait")
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "branch create: unexpected argument %q", fs.Args()[0])
	}
	if *name == "" {
		return usageErrf(stderr, "branch create: --name is required")
	}
	if *ttl != "" && !api.ValidTTL(*ttl) {
		return usageErrf(stderr, "branch create: --ttl must be one of 1h, 6h, 24h, 7d")
	}
	jsonOut := *jsonFlag
	timeoutVal := *timeout

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, branch, err := client.CreateBranch(context.Background(), project, api.CreateBranchRequest{
		Name: *name, TTL: *ttl, ParentBranchID: *parent,
	})
	if err != nil {
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}

	if !*wait {
		if jsonOut {
			dumpJSON(stdout, raw)
			return exitSuccess
		}
		fmt.Fprintf(stdout, "branch %s: %s\n", branch.Name, branch.Status)
		return exitSuccess
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), timeoutVal)
	defer cancel()
	raw, branch, err = client.WaitForTerminal(waitCtx, project, raw, branch)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if jsonOut {
				dumpJSON(stdout, raw)
			}
			fmt.Fprintf(stderr, "pgrun: timed out after %s waiting for branch %s to become ready (last status: %s)\n", timeoutVal, branch.Name, branch.Status)
			return exitFailure
		}
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}

	// branch.Status is Terminal here (WaitForTerminal only returns nil err at
	// a terminal status) — ready is the one successful outcome; every other
	// terminal status (failed/deleted/stopped/unhealthy) is a failure, and
	// that must hold in --json mode too: the exit code reflects the actual
	// outcome regardless of output format, --json only changes how it's
	// reported.
	if branch.Status == api.StatusReady {
		if jsonOut {
			out := raw
			if branch.ConnectionURL != "" {
				// Agent ergonomics: add a "database_url" alias for
				// connection_url so a --wait --json caller gets everything
				// (id/name/status/database_url) from this one response,
				// without needing to know the API's own field name or make
				// a second `branch get` just to learn it.
				out = api.WithDatabaseURL(raw, branch.ConnectionURL)
			}
			dumpJSON(stdout, out)
			return exitSuccess
		}
		if branch.ConnectionURL == "" {
			fmt.Fprintf(stderr, "pgrun: branch %s is ready but has no connection_url yet\n", branch.Name)
			return exitFailure
		}
		fmt.Fprintf(stdout, "DATABASE_URL=%s\n", branch.ConnectionURL)
		return exitSuccess
	}
	if jsonOut {
		dumpJSON(stdout, raw)
	}
	fmt.Fprintf(stderr, "pgrun: %s\n", api.WaitFailureReason(branch.Name, branch.Status))
	return exitFailure
}

func branchList(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) > 1 {
		return usageErrf(stderr, "branch list: unexpected argument %q — usage: pgrun branch list [<project>] [--json]", pos[1])
	}
	explicit := ""
	if len(pos) == 1 {
		explicit = pos[0]
	}
	project, code, ok := resolveProject(explicit, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("branch list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "branch list: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, branches, err := client.ListBranches(context.Background(), project)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	writeBranchTable(stdout, branches)
	return exitSuccess
}

func branchGet(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) == 0 {
		return usageErrf(stderr, "branch get: missing <name> — usage: pgrun branch get [<project>] <name> [--json]")
	}
	if len(pos) > 2 {
		return usageErrf(stderr, "branch get: unexpected argument %q — usage: pgrun branch get [<project>] <name> [--json]", pos[2])
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

	fs := flag.NewFlagSet("branch get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "branch get: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, branch, err := client.GetBranch(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	fmt.Fprintln(stdout, branchLine(branch)) // never connection_url
	return exitSuccess
}

func branchURL(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) == 0 {
		return usageErrf(stderr, "branch url: missing <name> — usage: pgrun branch url [<project>] <name>")
	}
	if len(pos) > 2 {
		return usageErrf(stderr, "branch url: unexpected argument %q — usage: pgrun branch url [<project>] <name>", pos[2])
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

	fs := flag.NewFlagSet("branch url", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "branch url: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	// branch url never prints raw API JSON — DATABASE_URL or a sanitized
	// reason are the only two outputs (hard constraint).
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
	fmt.Fprintf(stdout, "DATABASE_URL=%s\n", branch.ConnectionURL)
	return exitSuccess
}

func branchDelete(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) == 0 {
		return usageErrf(stderr, "branch delete: missing <name> — usage: pgrun branch delete [<project>] <name> [--json]")
	}
	if len(pos) > 2 {
		return usageErrf(stderr, "branch delete: unexpected argument %q — usage: pgrun branch delete [<project>] <name> [--json]", pos[2])
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

	fs := flag.NewFlagSet("branch delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "branch delete: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, status, err := client.DeleteBranch(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	fmt.Fprintf(stdout, "branch %s: %s\n", name, status)
	return exitSuccess
}
