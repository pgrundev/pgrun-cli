package mcpserver

// toolDefinitions is tools/list's static payload — one entry per handler in
// toolHandlers (server.go), inputSchema following JSON Schema draft-07 as
// MCP hosts expect.
var toolDefinitions = []map[string]any{
	{
		"name": "pgrun_create_branch",
		"description": "Create a disposable PGRun Postgres branch. By default waits until it is " +
			"ready (or failed) before returning; the returned JSON includes connection_url once ready. " +
			"Branches are cheap and meant to be deleted when done — always set a ttl for CI/agent use.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": "Project slug"},
				"name":    map[string]any{"type": "string", "description": "Branch name (lowercase, e.g. \"agent-task-42\")"},
				"ttl": map[string]any{
					"type": "string", "enum": []string{"1h", "6h", "24h", "7d"},
					"description": "Time-to-live after which the branch is automatically deleted. Omit for no TTL.",
				},
				"parent": map[string]any{
					"type":        "string",
					"description": "Parent branch — id, \"branch_<id>\", or name. Defaults to the project's base branch.",
				},
				"wait": map[string]any{
					"type":        "boolean",
					"description": "Poll until the branch reaches ready or failed before returning (default true). false returns immediately with the creating-status response.",
				},
				"timeout_seconds": map[string]any{
					"type":        "number",
					"description": "Max seconds to wait when wait is true (default 300).",
				},
			},
			"required": []string{"project", "name"},
		},
	},
	{
		"name":        "pgrun_list_branches",
		"description": "List a project's branches. Never includes connection_url — use pgrun_get_branch for that.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"project": map[string]any{"type": "string", "description": "Project slug"}},
			"required":   []string{"project"},
		},
	},
	{
		"name":        "pgrun_get_branch",
		"description": "Get one branch's current status. connection_url is present only when the branch is ready and credentialed.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": "Project slug"},
				"name":    map[string]any{"type": "string", "description": "Branch name"},
			},
			"required": []string{"project", "name"},
		},
	},
	{
		"name":        "pgrun_delete_branch",
		"description": "Delete a branch. Deleting the base branch, or a branch with children, fails with a 409 — delete children first.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": "Project slug"},
				"name":    map[string]any{"type": "string", "description": "Branch name"},
			},
			"required": []string{"project", "name"},
		},
	},
}
