// This file renders `pgrun source ...` output: the list table, the
// single-line `get` summary, and the checklist `status` view. All
// derivation from an api.Source lives here so list/get/status (source.go)
// and later tasks (protect/copy) can share it. Hard rule throughout: a
// source object never carries a connection URL, and nothing rendered here
// may print one either.

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// writeSourceTable renders `source list`'s human table.
func writeSourceTable(w io.Writer, sources []api.Source) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tPROTECTION\tSAFE COPY\tBRANCHES")
	for _, s := range sources {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			s.Name, s.SafeCopyStatus, protectionCell(s), safeCopyCell(s), s.Branches)
	}
	tw.Flush()
}

// protectionCell renders the PROTECTION column: the raw value, plus " vN"
// once a policy has actually been activated.
func protectionCell(s api.Source) string {
	if s.Protection == "active" && s.PolicyVersion > 0 {
		return fmt.Sprintf("%s v%d", s.Protection, s.PolicyVersion)
	}
	return s.Protection
}

// safeCopyCell renders the SAFE COPY column/field: a short human word for
// the handful of safe_copy_status values worth calling out, "-" otherwise
// (connect/checking/protect/ready_to_copy all just mean "no Safe Copy yet").
func safeCopyCell(s api.Source) string {
	switch s.SafeCopyStatus {
	case api.SourceReady:
		return "ready"
	case api.SourceCopying:
		return "copying"
	case api.SourceFailed:
		return "failed"
	case api.SourceActionRequired:
		return "action required"
	default:
		return "-"
	}
}

// sourceLine renders `source get`'s single human summary — one line of
// grep-friendly key=value pairs (never connection_url, which the source
// object doesn't even carry), followed by the same "next" command status
// prints, so a script can single-line either command's most important
// takeaway.
func sourceLine(s api.Source) string {
	policy := "-"
	if s.PolicyVersion > 0 {
		policy = fmt.Sprintf("v%d", s.PolicyVersion)
	}
	return fmt.Sprintf(
		"name=%s status=%s step=%d protection=%s policy=%s safe_copy=%s unresolved=%d branches=%d check_error=%s\nnext: %s",
		s.Name, s.SafeCopyStatus, s.Step, s.Protection, policy, safeCopyCell(s), s.UnresolvedCount, s.Branches, dash(s.LastCheckError),
		sourceNext(s),
	)
}

// sourceStatusView renders `source status`'s checklist block — four lines
// tracking the connect -> analyze schema -> protect -> Safe Copy pipeline,
// plus the one command to run next.
func sourceStatusView(s api.Source) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Production Database: %s\n\n", s.Name)
	fmt.Fprintln(&b, connectionLine(s))
	fmt.Fprintln(&b, schemaLine(s))
	fmt.Fprintln(&b, protectionLine(s))
	fmt.Fprintln(&b, safeCopyLine(s))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Next:")
	fmt.Fprintf(&b, "  %s\n", sourceNext(s))
	return b.String()
}

// connectionLine: glyphs are ✓ done, → in progress, ! needs attention,
// ○ not yet, ✗ failed — consistent across all four checklist lines.
func connectionLine(s api.Source) string {
	switch {
	case s.SafeCopyStatus == api.SourceConnect:
		return "○ Not connected — pgrun needs a PostgreSQL connection URL"
	case s.SafeCopyStatus == api.SourceChecking && s.LastCheckError == "":
		return "→ Checking connection (read-only, nothing is copied)"
	case api.CheckFailed(s):
		return fmt.Sprintf("✗ Connection failed (%s)", s.LastCheckError)
	default:
		line := "✓ Connected"
		if s.PostgresVersion != "" {
			line += fmt.Sprintf(", Postgres %s", s.PostgresVersion)
		}
		return line
	}
}

func schemaLine(s api.Source) string {
	if s.Step >= 2 {
		return fmt.Sprintf("✓ Schema analyzed (%d tables, %d potentially sensitive columns)", s.Tables, s.SensitiveColumns)
	}
	return "○ Schema not analyzed yet"
}

func protectionLine(s api.Source) string {
	switch s.Protection {
	case "active":
		if s.SchemaChanged {
			return fmt.Sprintf("! Data protection policy v%d was approved against an older schema", s.PolicyVersion)
		}
		return fmt.Sprintf("✓ Data protection active (policy v%d)", s.PolicyVersion)
	case "draft":
		if s.UnresolvedCount > 0 {
			noun, verb := "columns", "need"
			if s.UnresolvedCount == 1 {
				noun, verb = "column", "needs"
			}
			return fmt.Sprintf("! Data protection needs review (%d %s %s a decision)", s.UnresolvedCount, noun, verb)
		}
		return "! Data protection reviewed but not approved"
	default: // "none"
		return "○ Data protection not started"
	}
}

func safeCopyLine(s api.Source) string {
	switch s.SafeCopyStatus {
	case api.SourceReady:
		return "✓ Safe Copy ready"
	case api.SourceCopying:
		return "→ Creating Safe Copy"
	case api.SourceFailed:
		return "✗ Safe Copy failed"
	case api.SourceActionRequired:
		if s.Step == 4 {
			return "! Safe Copy ready, but production schema changed since the policy was approved"
		}
		return "○ Safe Copy not created — production schema changed, review protection first"
	default:
		return "○ Safe Copy not created"
	}
}

// sourceNext names the one command to run next — never prose, so it's
// always something a user (or an agent) can copy-paste or exec directly.
func sourceNext(s api.Source) string {
	if s.SafeCopyStatus == api.SourceConnect || api.CheckFailed(s) {
		return fmt.Sprintf(`pgrun source update %s --url "$DATABASE_URL"`, s.Name)
	}
	switch s.SafeCopyStatus {
	case api.SourceChecking, api.SourceCopying:
		return fmt.Sprintf("pgrun source status %s", s.Name)
	case api.SourceProtect, api.SourceActionRequired:
		return fmt.Sprintf("pgrun source protect %s", s.Name)
	case api.SourceReadyToCopy:
		return fmt.Sprintf("pgrun source copy %s --wait", s.Name)
	case api.SourceReady:
		return fmt.Sprintf("pgrun branch create %s --name dev --wait", s.Name)
	case api.SourceFailed:
		return "write to support@postgresrun.com (a failed Safe Copy is a support case)"
	default:
		return fmt.Sprintf("pgrun source status %s", s.Name)
	}
}

// sourceActionable reports whether a source needs human attention right
// now: `status` uses this for its exit code (ruling 4).
func sourceActionable(s api.Source) bool {
	return s.SafeCopyStatus == api.SourceFailed || s.SafeCopyStatus == api.SourceActionRequired || api.CheckFailed(s)
}

// dispositionLabel renders a protect-table disposition in the UI's
// vocabulary (customer-facing words, not the API's internal ones).
// "remove" is the CLI's --set alias for "null" (ruling 5); it never comes
// back from the API, but is included here so both directions render the
// same word.
func dispositionLabel(d string) string {
	switch d {
	case "copy", "copy_data":
		return "COPY"
	case "fake":
		return "FAKE"
	case "null", "remove":
		return "REMOVE"
	case "schema_only":
		return "SCHEMA ONLY"
	case "exclude":
		return "EXCLUDE"
	default:
		return strings.ToUpper(d)
	}
}
