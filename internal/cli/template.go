package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// maxSeedBytes mirrors the server's 1 MiB seed limit, so an oversized file
// fails before upload with a clear message.
const maxSeedBytes = 1 << 20

const templateUsage = "usage: pgrun template create [<project>] <name> --schema-only [--seed <file.sql>] [--wait] [--timeout 15m] [--json]\n" +
	"       pgrun template list [<project>] [--json]\n" +
	"       pgrun template get [<project>] <name> [--json]\n" +
	"       pgrun template build [<project>] <name> [--wait] [--timeout 15m] [--json]\n" +
	"       pgrun template delete [<project>] <name> [--json]"

// runTemplate implements `pgrun template`: CI templates — a project's schema
// (no application rows) plus optional deterministic seed SQL, built once
// and cloned per branch with `branch create/exec --template <name>`.
func runTemplate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErrf(stderr, "template: expected a subcommand (create, list, get, build, delete)\n"+templateUsage)
	}
	switch args[0] {
	case "create", "apply":
		return templateCreate(args[1:], stdout, stderr)
	case "list":
		return templateList(args[1:], stdout, stderr)
	case "get", "status":
		return templateGet(args[1:], stdout, stderr)
	case "build", "rebuild":
		return templateBuild(args[1:], stdout, stderr)
	case "delete":
		return templateDelete(args[1:], stdout, stderr)
	default:
		return usageErrf(stderr, "template: unknown subcommand %q\n"+templateUsage, args[0])
	}
}

// projectAndName resolves `[<project>] <name>` the way branch get does.
func projectAndName(cmd string, args []string, stderr io.Writer) (project, name string, rest []string, code int, ok bool) {
	pos, rest := leadingPositionals(args)
	if len(pos) == 0 {
		return "", "", nil, usageErrf(stderr, "template %s: missing <name>\n"+templateUsage, cmd), false
	}
	if len(pos) > 2 {
		return "", "", nil, usageErrf(stderr, "template %s: unexpected argument %q\n"+templateUsage, cmd, pos[2]), false
	}
	explicit := ""
	if len(pos) == 2 {
		explicit, name = pos[0], pos[1]
	} else {
		name = pos[0]
	}
	project, code, ok = resolveProject(explicit, stderr)
	return project, name, rest, code, ok
}

func templateCreate(args []string, stdout, stderr io.Writer) int {
	project, name, rest, code, ok := projectAndName("create", args, stderr)
	if !ok {
		return code
	}
	fs := flag.NewFlagSet("template create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	schemaOnly := fs.Bool("schema-only", false, "copy the schema (and migration bookkeeping rows) but no application rows")
	seed := fs.String("seed", "", "SQL file applied once to the template after the schema (deterministic seed/reference data)")
	wait := fs.Bool("wait", false, "poll until the template is ready or failed")
	timeout := fs.Duration("timeout", 15*time.Minute, "max time to wait with --wait")
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "template create: unexpected argument %q", fs.Args()[0])
	}
	if !*schemaOnly {
		return usageErrf(stderr, "template create: --schema-only is required (it is the only template mode today)")
	}
	req := api.PutTemplateRequest{SchemaOnly: true}
	if *seed != "" {
		sql, err := os.ReadFile(*seed)
		if err != nil {
			fmt.Fprintf(stderr, "pgrun: template create: %v\n", err)
			return exitFailure
		}
		if len(sql) > maxSeedBytes {
			return usageErrf(stderr, "template create: %s is %d bytes; the seed limit is %d", *seed, len(sql), maxSeedBytes)
		}
		req.SeedSQL = string(sql)
	}

	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)
	raw, t, err := client.PutTemplate(context.Background(), project, name, req)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *wait {
		return finishTemplateWait(client, project, raw, t, *timeout, *jsonFlag, stdout, stderr)
	}
	return printTemplate(raw, t, *jsonFlag, stdout)
}

func templateBuild(args []string, stdout, stderr io.Writer) int {
	project, name, rest, code, ok := projectAndName("build", args, stderr)
	if !ok {
		return code
	}
	fs := flag.NewFlagSet("template build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Bool("wait", false, "poll until the template is ready or failed")
	timeout := fs.Duration("timeout", 15*time.Minute, "max time to wait with --wait")
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)
	raw, t, err := client.BuildTemplate(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *wait {
		return finishTemplateWait(client, project, raw, t, *timeout, *jsonFlag, stdout, stderr)
	}
	return printTemplate(raw, t, *jsonFlag, stdout)
}

func templateGet(args []string, stdout, stderr io.Writer) int {
	project, name, rest, code, ok := projectAndName("get", args, stderr)
	if !ok {
		return code
	}
	fs := flag.NewFlagSet("template get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	raw, t, err := api.New(cfg.URL, cfg.Token).GetTemplate(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	return printTemplate(raw, t, *jsonFlag, stdout)
}

func templateList(args []string, stdout, stderr io.Writer) int {
	pos, rest := leadingPositionals(args)
	if len(pos) > 1 {
		return usageErrf(stderr, "template list: unexpected argument %q\n"+templateUsage, pos[1])
	}
	explicit := ""
	if len(pos) == 1 {
		explicit = pos[0]
	}
	project, code, ok := resolveProject(explicit, stderr)
	if !ok {
		return code
	}
	fs := flag.NewFlagSet("template list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	raw, list, err := api.New(cfg.URL, cfg.Token).ListTemplates(context.Background(), project)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no templates")
		return exitSuccess
	}
	for _, t := range list {
		fmt.Fprintln(stdout, templateLine(t))
	}
	return exitSuccess
}

func templateDelete(args []string, stdout, stderr io.Writer) int {
	project, name, rest, code, ok := projectAndName("delete", args, stderr)
	if !ok {
		return code
	}
	fs := flag.NewFlagSet("template delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	cfg, code, ok := resolveOrHint(*urlFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	raw, status, err := api.New(cfg.URL, cfg.Token).DeleteTemplate(context.Background(), project, name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if *jsonFlag {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	fmt.Fprintf(stdout, "template %s: %s\n", name, status)
	return exitSuccess
}

func finishTemplateWait(client *api.Client, project string, raw []byte, t api.Template, timeout time.Duration, jsonOut bool, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, t, err := client.WaitForTemplate(ctx, project, raw, t)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "pgrun: timed out after %s waiting for template %s (last status: %s)\n", timeout, t.Name, t.Status)
			return exitFailure
		}
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}
	code := printTemplate(raw, t, jsonOut, stdout)
	if t.Status != api.TemplateReady {
		fmt.Fprintf(stderr, "pgrun: template %s %s: %s\n", t.Name, t.Status, t.Error)
		return exitFailure
	}
	return code
}

func printTemplate(raw []byte, t api.Template, jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	fmt.Fprintln(stdout, templateLine(t))
	return exitSuccess
}

func templateLine(t api.Template) string {
	line := fmt.Sprintf("template %s: %s (%s, v%d", t.Name, t.Status, t.Mode, t.Version)
	if t.SeedSHA256 != "" {
		short := t.SeedSHA256
		if len(short) > 12 {
			short = short[:12]
		}
		line += ", seed " + short
	}
	if t.SizeBytes > 0 {
		line += fmt.Sprintf(", %d MB", t.SizeBytes/(1<<20))
	}
	if t.BuildMs > 0 {
		line += fmt.Sprintf(", built in %d ms", t.BuildMs)
	}
	line += ")"
	if t.Error != "" {
		line += " — " + t.Error
	}
	return line
}

// ensureTemplateReady is what `branch create/exec --template` and
// `--schema-only` run before asking for a branch: the server refuses to mint
// from a template that is not ready (409 template_not_ready), so wait for an
// in-flight build, rebuild a stale one, and — for --schema-only — define the
// built-in schema-only template first (idempotent; a no-op once it exists
// and matches the current schema).
func ensureTemplateReady(client *api.Client, project, name string, define bool, timeout time.Duration, stderr io.Writer) (ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var (
		t   api.Template
		err error
	)
	if define {
		_, t, err = client.PutTemplate(ctx, project, name, api.PutTemplateRequest{SchemaOnly: true})
	} else {
		_, t, err = client.GetTemplate(ctx, project, name)
	}
	if err == nil && t.Status != api.TemplateReady {
		fmt.Fprintf(stderr, "pgrun: template %s is %s; waiting for it to be ready\n", name, t.Status)
		_, t, err = client.WaitForTemplate(ctx, project, nil, t)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(stderr, "pgrun: timed out after %s waiting for template %s (last status: %s)\n", timeout, name, t.Status)
		return false
	case err != nil:
		fmt.Fprintf(stderr, "pgrun: template %s: %v\n", name, err)
		return false
	case t.Status != api.TemplateReady:
		fmt.Fprintf(stderr, "pgrun: template %s %s: %s\n", name, t.Status, t.Error)
		return false
	}
	return true
}
