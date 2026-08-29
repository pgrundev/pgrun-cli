package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/pgrundev/pgrun/internal/api"
)

// writeBranchTable renders `branch list`'s human table. Never includes
// connection_url — list responses don't carry it anyway (per the API
// contract), but this stays explicit rather than accidental.
func writeBranchTable(w io.Writer, branches []api.Branch) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tBASE\tPARENT\tCREATED\tEXPIRES")
	for _, b := range branches {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			b.Name, b.Status, dash(b.BaseBranch), dash(b.ParentBranchID), dash(b.CreatedAt), dash(b.ExpiresAt))
	}
	tw.Flush()
}

// branchLine renders `branch get`'s single human line. connection_url is
// deliberately never included — `branch url` is the only reveal point.
func branchLine(b api.Branch) string {
	return fmt.Sprintf("name=%s status=%s base=%s parent=%s created=%s expires=%s",
		b.Name, b.Status, dash(b.BaseBranch), dash(b.ParentBranchID), dash(b.CreatedAt), dash(b.ExpiresAt))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
