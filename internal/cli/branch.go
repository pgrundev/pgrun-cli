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
		return usageErrf(stderr, "branch: expected a subcommand (create, list, get, url, delete)")
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
	case "delete":
		return branchDelete(args[1:], stdout, stderr)
	default:
		return usageErrf(stderr, "branch: unknown subcommand %q", args[0])
	}
}

func validTTL(ttl string) bool {
	switch ttl {
	case "1h", "6h", "24h", "7d":
		return true
	}
	return false
}

func branchCreate(args []string, stdout, stderr io.Writer) int {
	positional, rest, err := splitPositional(args, 1)
	if err != nil {
		return usageErrf(stderr, "branch create: %v — usage: pgrun branch create <project> --name <n> [--ttl 1h|6h|24h|7d] [--parent <name>] [--wait] [--timeout 300s] [--json]", err)
	}
	project := positional[0]

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
	if *ttl != "" && !validTTL(*ttl) {
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

	if jsonOut {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	switch branch.Status {
	case api.StatusReady:
		if branch.ConnectionURL == "" {
			fmt.Fprintf(stderr, "pgrun: branch %s is ready but has no connection_url yet\n", branch.Name)
			return exitFailure
		}
		fmt.Fprintf(stdout, "DATABASE_URL=%s\n", branch.ConnectionURL)
		return exitSuccess
	case api.StatusFailed:
		fmt.Fprintf(stderr, "pgrun: branch %s failed\n", branch.Name)
		return exitFailure
	default:
		fmt.Fprintf(stderr, "pgrun: branch %s ended in unexpected status %q\n", branch.Name, branch.Status)
		return exitFailure
	}
}

func branchList(args []string, stdout, stderr io.Writer) int {
	positional, rest, err := splitPositional(args, 1)
	if err != nil {
		return usageErrf(stderr, "branch list: %v — usage: pgrun branch list <project> [--json]", err)
	}
	project := positional[0]

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
	positional, rest, err := splitPositional(args, 2)
	if err != nil {
		return usageErrf(stderr, "branch get: %v — usage: pgrun branch get <project> <name> [--json]", err)
	}
	project, name := positional[0], positional[1]

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
	positional, rest, err := splitPositional(args, 2)
	if err != nil {
		return usageErrf(stderr, "branch url: %v — usage: pgrun branch url <project> <name>", err)
	}
	project, name := positional[0], positional[1]

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
	positional, rest, err := splitPositional(args, 2)
	if err != nil {
		return usageErrf(stderr, "branch delete: %v — usage: pgrun branch delete <project> <name> [--json]", err)
	}
	project, name := positional[0], positional[1]

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
