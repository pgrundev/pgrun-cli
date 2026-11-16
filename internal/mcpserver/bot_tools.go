package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
	"github.com/pgrundev/pgrun-cli/internal/pgbot"
)

// PgBot over MCP: read-only, branch-scoped tools. Each resolves the branch
// through the API (project-scoped token) and runs pgbot with the branch URL in
// its environment; no tool takes a connection string, and none runs pgbot's AI
// layer. Migration verification runs the agent's own migration command, so it
// stays a CLI command (`pgrun bot verify`) rather than an MCP tool that would
// execute arbitrary commands.

var botToolDefinitions = []map[string]any{
	{
		"name": "pgrun_bot_lint",
		"description": "Schema-only PgBot findings for a ready PGRun branch (invalid/duplicate indexes, unindexed foreign keys, " +
			"narrow identity columns…). Read-only. Returns pgbot's versioned JSON.",
		"inputSchema": branchArgsSchema(nil),
	},
	{
		"name": "pgrun_bot_schema",
		"description": "The branch's schema structure as JSON (relations, columns, indexes, constraints, extensions) from the catalog " +
			"only — never table data. Needs a pgbot with the `schema` command.",
		"inputSchema": branchArgsSchema(nil),
	},
	{
		"name":        "pgrun_bot_explain",
		"description": "The planner's plan for one SELECT on a ready branch (plan only, the query is never executed). Read-only.",
		"inputSchema": branchArgsSchema(map[string]any{"sql": map[string]any{"type": "string", "description": "One SELECT statement"}}),
	},
}

func branchArgsSchema(extra map[string]any) map[string]any {
	props := map[string]any{
		"project": map[string]any{"type": "string", "description": "Project slug"},
		"name":    map[string]any{"type": "string", "description": "Branch name"},
	}
	required := []string{"project", "name"}
	for k, v := range extra {
		props[k] = v
		required = append(required, k)
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func init() {
	toolDefinitions = append(toolDefinitions, botToolDefinitions...)
	toolHandlers["pgrun_bot_lint"] = func(s *Server, ctx context.Context, raw json.RawMessage) any {
		return s.botTool(ctx, raw, func(map[string]string) ([]string, string) {
			return []string{"lint", "--fail-on", "none", "--format", "json"}, ""
		})
	}
	toolHandlers["pgrun_bot_schema"] = func(s *Server, ctx context.Context, raw json.RawMessage) any {
		return s.botTool(ctx, raw, func(map[string]string) ([]string, string) {
			return []string{"schema", "snapshot"}, "schema"
		})
	}
	toolHandlers["pgrun_bot_explain"] = func(s *Server, ctx context.Context, raw json.RawMessage) any {
		return s.botTool(ctx, raw, func(a map[string]string) ([]string, string) {
			return []string{"sql", "explain", "--json", a["sql"]}, ""
		})
	}
}

func (s *Server) botTool(ctx context.Context, raw json.RawMessage, argv func(map[string]string) ([]string, string)) any {
	var a map[string]string
	if err := json.Unmarshal(raw, &a); err != nil || a["project"] == "" || a["name"] == "" {
		return toolError("project and name are required")
	}
	args, needs := argv(a)
	for _, x := range args {
		low := strings.ToLower(x)
		if strings.HasPrefix(low, "postgres://") || strings.HasPrefix(low, "postgresql://") || strings.Contains(low, "service=") {
			return toolError("pgrun bot tools only inspect PGRun branches; they never take a connection string")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	bin, err := pgbot.Locate(ctx)
	if errors.Is(err, pgbot.ErrNotFound) {
		return toolError("pgbot is not installed — run `pgrun bot install` (or `pgrun setup`)")
	}
	if !bin.Compatible {
		return toolError(bin.Problem)
	}
	if needs == "schema" && !bin.Capabilities.SchemaReview {
		return toolError("this pgbot has no `schema` command — install a newer pgbot")
	}
	_, b, err := s.Client.GetBranch(ctx, a["project"], a["name"])
	if err != nil {
		return apiToolError(err)
	}
	if b.Status != api.StatusReady || b.ConnectionURL == "" {
		return toolError("branch " + a["name"] + " is not ready (status: " + b.Status + ")")
	}
	out, _, err := pgbot.Output(ctx, bin, b.ConnectionURL, args...)
	if err != nil {
		return toolError(strings.ReplaceAll(err.Error(), b.ConnectionURL, "[redacted]"))
	}
	return jsonContent([]byte(strings.ReplaceAll(string(out), b.ConnectionURL, "[redacted]")))
}
