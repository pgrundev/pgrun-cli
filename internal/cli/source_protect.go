// This file implements `pgrun source protect`: the data-protection review
// table, --set/--acknowledge decisions, explicit --approve, and --review.
// It consumes Task 1's ProtectSource/ReviewSource/GetSource and Task 2's
// sourceNameArgs/dispositionLabel/sourceNext.
//
// The hard rule throughout (see the task brief's global constraints): the
// CLI never activates anything itself. --approve is the only thing that
// ever sends "approve":true, the server is the sole authority on whether a
// policy may be activated (it refuses with 422 while anything is
// unresolved), and this file only ever surfaces that refusal — it never
// works around it. There is no "copy everything" shortcut and none may be
// added here.

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// multiFlag is a repeatable string flag: flag.Var appends every occurrence
// instead of the stdlib default of keeping only the last one. Used for
// --set and --acknowledge, both repeatable per the brief.
type multiFlag []string

func (m *multiFlag) String() string {
	if m == nil {
		return ""
	}
	return strings.Join(*m, ",")
}

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseSetItem parses one --set <table>.<column>=<disposition> item.
// "remove" is the CLI's alias for "null" (ruling 5 — the UI's word); ok is
// false for anything else malformed (no "=", empty key/value, or an
// unrecognized disposition), so the caller can report the bad item verbatim
// without ever reaching the server with it.
func parseSetItem(item string) (key, disposition string, ok bool) {
	idx := strings.Index(item, "=")
	if idx <= 0 || idx == len(item)-1 {
		return "", "", false
	}
	key = item[:idx]
	switch val := item[idx+1:]; val {
	case "copy", "fake", "null":
		return key, val, true
	case "remove":
		return key, "null", true
	default:
		return "", "", false
	}
}

func sourceProtect(args []string, stdout, stderr io.Writer) int {
	name, rest, code, ok := sourceNameArgs("protect", args, stderr)
	if !ok {
		return code
	}

	fs := flag.NewFlagSet("source protect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var sets, acks multiFlag
	fs.Var(&sets, "set", "<table>.<column>=copy|fake|null|remove (repeatable)")
	fs.Var(&acks, "acknowledge", "<table>.<column> — acknowledge a sensitive column's Copy decision (repeatable)")
	approve := fs.Bool("approve", false, "activate the policy (refused while any column is unresolved)")
	review := fs.Bool("review", false, "ask pgrun to flag every unresolved column for review")
	jsonFlag := addJSONFlag(fs)
	apiURLFlag, tokenFlag := addSourceAuthFlags(fs)
	if ok, code := parseOrExit(fs, rest); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "source protect: unexpected argument %q", fs.Args()[0])
	}

	if *review && (len(sets) > 0 || len(acks) > 0 || *approve) {
		return usageErrf(stderr, "source protect: --review cannot be combined with --set, --acknowledge, or --approve")
	}

	decisions := map[string]string{}
	for _, item := range sets {
		key, disposition, ok := parseSetItem(item)
		if !ok {
			return usageErrf(stderr, "source protect: invalid --set %q — expected <table>.<column>=copy|fake|null|remove", item)
		}
		if _, dup := decisions[key]; dup {
			// A repeated --set is almost certainly a mistake (which decision
			// did the caller actually mean?) — refused before any request
			// rather than silently letting the last one win.
			return usageErrf(stderr, "source protect: --set names %s twice", key)
		}
		decisions[key] = disposition
	}

	cfg, code, ok := resolveOrHint(*apiURLFlag, *tokenFlag, stderr)
	if !ok {
		return code
	}
	client := api.New(cfg.URL, cfg.Token)
	ctx := context.Background()

	if *review {
		return sourceReview(ctx, client, name, *jsonFlag, stdout, stderr)
	}

	// Pre-flight: nothing to protect before the schema has been analyzed —
	// refuse before any POST (ruling: fail closed, never contact the
	// protect endpoint for a source that isn't even connected yet).
	raw, src, err := client.GetSource(ctx, name)
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}
	if src.SafeCopyStatus == api.SourceConnect || src.SafeCopyStatus == api.SourceChecking {
		fmt.Fprintf(stderr, "pgrun: %s is not connected yet — %s\n", name, sourceNext(src))
		return exitFailure
	}

	raw, res, err := client.ProtectSource(ctx, name, api.ProtectRequest{
		Decisions:   decisions,
		Acknowledge: []string(acks),
		Approve:     *approve,
	})
	if err != nil {
		return handleAPIError(err, *jsonFlag, raw, stdout, stderr)
	}

	if *jsonFlag {
		dumpJSON(stdout, raw)
		return protectExitCode(res)
	}

	writeProtectReview(stdout, name, src, res)
	return protectExitCode(res)
}

// hasUnresolved is the single predicate for "is this response unresolved" —
// shared by protectExitCode and writeProtectReview's closing-variant choice
// so the exit code can never contradict what the table just showed. It
// checks both UnresolvedCount and len(Unresolved): a server response could
// in principle carry entries in one without the other being consistent, and
// the table renders off Unresolved, so the exit code must account for it
// too.
func hasUnresolved(res api.ProtectResult) bool {
	return res.UnresolvedCount > 0 || len(res.Unresolved) > 0
}

// protectExitCode is ruling 4 for `protect`: 1 while columns are
// unresolved, 0 once the review is complete or the policy was activated —
// true in both output modes, --json only changes how it's reported.
func protectExitCode(res api.ProtectResult) int {
	if hasUnresolved(res) {
		return exitFailure
	}
	return exitSuccess
}

// sourceReview implements the --review branch: it never touches GetSource
// or ProtectSource, and always exits 0 — asking pgrun to flag unresolved
// columns for review isn't itself an action that can fail the way
// activating a policy can.
func sourceReview(ctx context.Context, client *api.Client, name string, jsonOut bool, stdout, stderr io.Writer) int {
	raw, res, err := client.ReviewSource(ctx, name)
	if err != nil {
		return handleAPIError(err, jsonOut, raw, stdout, stderr)
	}
	if jsonOut {
		dumpJSON(stdout, raw)
		return exitSuccess
	}
	if len(res.Unresolved) == 0 {
		fmt.Fprintln(stdout, "nothing is unresolved — no review needed")
		return exitSuccess
	}
	fmt.Fprintf(stdout, "review requested for %d columns: %s\n", len(res.Unresolved), strings.Join(res.Unresolved, ", "))
	return exitSuccess
}

// writeProtectReview renders the human `source protect` output: the header,
// the schema-change block (when present), the review table(s), and one of
// the three closing variants. src is the pre-flight GetSource response (the
// only place schema_changed/schema_changes/policy_version-before-this-call
// live); res is what ProtectSource just returned.
func writeProtectReview(w io.Writer, name string, src api.Source, res api.ProtectResult) {
	fmt.Fprintf(w, "Data protection review — %s\n", name)

	if src.SchemaChanged && src.SchemaChanges != nil {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Production schema changed since policy v%d was approved:\n", src.PolicyVersion)
		fmt.Fprintf(w, "  %-9s%s\n", "added:", commaOrDash(src.SchemaChanges.Added))
		fmt.Fprintf(w, "  %-9s%s\n", "removed:", commaOrDash(src.SchemaChanges.Removed))
		fmt.Fprintf(w, "  %-9s%s\n", "retyped:", commaOrDash(src.SchemaChanges.Retyped))
	}

	// A bare "COLUMN DISPOSITION SOURCE" header with no rows helps nobody —
	// skip the whole table when there's nothing to put in it (e.g. a
	// schema-change-only response with no column rules yet).
	if len(res.Rules) > 0 || len(res.Unresolved) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "COLUMN\tDISPOSITION\tSOURCE")
		for _, rule := range res.Rules {
			column := rule.Column
			if rule.Sensitive {
				column += " (sensitive)"
			}
			source := "decided"
			if rule.Recommended {
				source = "recommended"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", column, dispositionLabel(rule.Disposition), source)
		}
		for _, u := range res.Unresolved {
			source := "valid: " + strings.Join(u.Valid, ", ")
			if u.Sensitive {
				source += " (sensitive — copying needs --acknowledge)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Column, "UNRESOLVED", source)
		}
		tw.Flush()
	}

	if len(res.TableRules) > 0 {
		fmt.Fprintln(w)
		ttw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(ttw, "TABLE\tDISPOSITION")
		for _, tr := range res.TableRules {
			fmt.Fprintf(ttw, "%s\t%s\n", tr.Table, dispositionLabel(tr.Disposition))
		}
		ttw.Flush()
	}

	fmt.Fprintln(w)
	switch {
	case hasUnresolved(res):
		// Count off len(Unresolved) — the table above rendered exactly
		// these rows, so the number in this sentence must match what's on
		// screen even if UnresolvedCount itself were ever inconsistent.
		fmt.Fprintf(w, "%d column(s) need a decision.\n", len(res.Unresolved))
		fmt.Fprintln(w, "Run:")
		for _, u := range res.Unresolved {
			line := fmt.Sprintf("  pgrun source protect %s --set %s=copy|fake|null", name, u.Column)
			if u.Sensitive {
				line += fmt.Sprintf(" --acknowledge %s", u.Column)
			}
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w, "or ask pgrun to review them:")
		fmt.Fprintf(w, "  pgrun source protect %s --review\n", name)
	case res.Activated:
		fmt.Fprintf(w, "✓ Data protection active — policy v%d\n", res.PolicyVersion)
		fmt.Fprintln(w, "Next:")
		fmt.Fprintf(w, "  pgrun source copy %s --wait\n", name)
	default:
		fmt.Fprintln(w, "Every column is resolved. Approve to activate the policy:")
		fmt.Fprintf(w, "  pgrun source protect %s --approve\n", name)
	}
}

// commaOrDash joins a schema_changes list with ", ", or "-" when empty —
// consistent with dash() in output.go for a single empty field.
func commaOrDash(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}
