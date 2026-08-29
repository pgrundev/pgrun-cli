package api

import (
	"context"
	"time"
)

// PollInterval is the cadence for WaitForTerminal's GET loop. A var (not a
// const) so tests can shrink it instead of waiting on the real cadence.
var PollInterval = 2 * time.Second

// Terminal reports whether a branch status is a poll-loop stopping point.
func Terminal(status string) bool {
	return status == StatusReady || status == StatusFailed
}

// WaitForTerminal polls GetBranch every PollInterval until the branch
// reaches ready/failed, ctx is done (the caller's timeout), or a request
// errors. It always returns the most recent raw body it has — even when
// returning an error — so callers can still honor --json / isError with
// whatever the API last said.
func (c *Client) WaitForTerminal(ctx context.Context, project string, raw []byte, branch Branch) ([]byte, Branch, error) {
	for {
		if Terminal(branch.Status) {
			return raw, branch, nil
		}
		select {
		case <-ctx.Done():
			return raw, branch, ctx.Err()
		case <-time.After(PollInterval):
		}
		var err error
		raw, branch, err = c.GetBranch(ctx, project, branch.Name)
		if err != nil {
			return raw, branch, err
		}
	}
}
