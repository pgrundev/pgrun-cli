package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/pgrundev/pgrun/internal/api"
	"github.com/pgrundev/pgrun/internal/config"
)

// runProject implements both `pgrun project` and `pgrun projects` —
// listing what a token can see, and selecting one as the repo-local default
// that branch commands fall back to when their <project> positional is
// omitted (see resolveProject in resolve.go). Bare `pgrun project`/`pgrun
// projects` with no subcommand defaults to `list`.
func runProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return projectList(nil, stdout, stderr)
	}
	switch args[0] {
	case "list":
		return projectList(args[1:], stdout, stderr)
	case "use":
		return projectUse(args[1:], stdout, stderr)
	case "show":
		return projectShow(args[1:], stdout, stderr)
	default:
		return usageErrf(stderr, "project: unknown subcommand %q", args[0])
	}
}

// projectList implements `pgrun projects list`.
func projectList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("projects list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "projects list: unexpected argument %q", fs.Args()[0])
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)

	raw, projects, err := client.ListProjects(context.Background())
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	if len(projects) == 0 {
		fmt.Fprintln(stderr, "no projects found")
		return exitSuccess
	}

	var current string
	if cwd, err := os.Getwd(); err == nil {
		current, _, _ = config.FindProject(cwd)
	}
	writeProjectsTable(stdout, projects, current)
	return exitSuccess
}

// writeProjectsTable renders `projects list`'s human table, prefixing the
// row whose name matches the repo's currently selected project (from
// .pgrun/project) with "* " so it's visible at a glance which one branch
// commands will default to.
func writeProjectsTable(w io.Writer, projects []api.Project, current string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROJECT\tSTATUS\tBASE\tBRANCHES\tPG")
	for _, p := range projects {
		marker := "  "
		if p.Name == current {
			marker = "* "
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%d\t%s\n",
			marker, p.Name, p.Status, dash(p.BaseBranch), p.Branches, dash(p.PostgresVersion))
	}
	tw.Flush()
}

// projectUse implements `pgrun project use <slug>`: selects slug as this
// repo's default project by writing .pgrun/project. Unless --no-verify is
// given, it first confirms via the API that slug actually exists (which
// also exercises the auth gate, same as any other API call).
func projectUse(args []string, stdout, stderr io.Writer) int {
	positional, rest, err := splitPositional(args, 1)
	if err != nil {
		return usageErrf(stderr, "project use: %v — usage: pgrun project use <slug> [--no-verify] [--url --token]", err)
	}
	slug := positional[0]

	fs := flag.NewFlagSet("project use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noVerify := fs.Bool("no-verify", false, "skip confirming the slug exists via the API")
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "project use: unexpected argument %q", fs.Args()[0])
	}

	if !*noVerify {
		cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
		if !ok {
			return code
		}
		client := api.New(cfg.URL, cfg.Token)

		raw, projects, err := client.ListProjects(context.Background())
		if err != nil {
			return handleAPIError(err, false, raw, stdout, stderr)
		}
		found := false
		names := make([]string, 0, len(projects))
		for _, p := range projects {
			names = append(names, p.Name)
			if p.Name == slug {
				found = true
			}
		}
		if !found {
			fmt.Fprintf(stderr, "pgrun: no project named %q — available: %s\n", slug, strings.Join(names, ", "))
			return exitFailure
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	path, err := config.SaveProject(cwd, slug)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "using project %q — wrote %s\n", slug, path)
	return exitSuccess
}

// projectShow implements `pgrun project show`: reports the repo's currently
// selected project, if any. The exit code alone tells a script whether one
// is selected (mirrors `skill status`).
func projectShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "project show: unexpected argument %q", fs.Args()[0])
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	slug, path, err := config.FindProject(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	if slug == "" {
		fmt.Fprintln(stdout, "no project selected — run `pgrun project use <slug>`")
		return exitFailure
	}
	fmt.Fprintf(stdout, "%s  (%s)\n", slug, path)
	return exitSuccess
}
