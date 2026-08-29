package api

import (
	"context"
	"fmt"
	"time"
)

// PollInterval is the cadence for WaitForTerminal's GET loop. A var (not a
// const) so tests can shrink it instead of waiting on the real cadence.
var PollInterval = 2 * time.Second

// Terminal reports whether a branch status is a poll-loop stopping point —
// any status that doesn't move on its own without further action. Only
// StatusReady is a *successful* outcome; every other terminal status is a
// wait failure (see WaitFailureReason) — the branch got deleted out from
// under the wait, was stopped, became unhealthy, or failed outright.
func Terminal(status string) bool {
	switch status {
	case StatusReady, StatusFailed, StatusDeleted, StatusStopped, StatusUnhealthy:
		return true
	}
	return false
}

// WaitFailureReason describes why a wait ended on a non-ready terminal
// status. Returns "" for StatusReady (not a failure — callers should check
// that first). Centralized here so the CLI and MCP server report the exact
// same wording for the exact same outcome.
func WaitFailureReason(name, status string) string {
	switch status {
	case StatusReady:
		return ""
	case StatusFailed:
		return fmt.Sprintf("branch %s failed", name)
	case StatusDeleted:
		return fmt.Sprintf("branch %s was deleted while waiting", name)
	case StatusStopped:
		return fmt.Sprintf("branch %s was stopped while waiting", name)
	case StatusUnhealthy:
		return fmt.Sprintf("branch %s became unhealthy while waiting", name)
	default:
		return fmt.Sprintf("branch %s ended in unexpected status %q while waiting", name, status)
	}
}

// WaitForTerminal polls GetBranch every PollInterval until the branch
// reaches a Terminal status, ctx is done (the caller's timeout), or a
// request errors. It always returns the most recent raw body it has — even
// when returning an error — so callers can still honor --json / isError with
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
