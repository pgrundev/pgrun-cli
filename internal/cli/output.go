package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// writeBranchTable renders `branch list`'s human table. Never includes
// connection_url — list responses don't carry it anyway (per the API
// contract), but this stays explicit rather than accidental.
func writeBranchTable(w io.Writer, branches []api.Branch) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tBASE\tPARENT\tVERSION\tCREATED\tEXPIRES")
	for _, b := range branches {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.Name, b.Status, boolDash(b.IsBase), dash(b.ParentBranchID), dash(b.PostgresVersion), dash(b.CreatedAt), dash(b.ExpiresAt))
	}
	tw.Flush()
}

// branchLine renders `branch get`'s single human line. connection_url is
// deliberately never included — `branch url` is the only reveal point.
func branchLine(b api.Branch) string {
	return fmt.Sprintf("name=%s status=%s base=%s parent=%s version=%s created=%s expires=%s",
		b.Name, b.Status, boolDash(b.IsBase), dash(b.ParentBranchID), dash(b.PostgresVersion), dash(b.CreatedAt), dash(b.ExpiresAt))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// boolDash renders is_base as "yes" or "-" — consistent with dash() for the
// other columns, and avoids a stray "false" cluttering every non-base row.
func boolDash(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}
