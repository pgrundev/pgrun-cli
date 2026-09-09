# Live end-to-end transcript — 2026-08-29

Against a local PGRun instance (demo server, branching flag on, `local_dev` provider)
at `http://localhost:3299`, token via `PGRUN_API_TOKEN` env (redacted).

## CLI

```
$ pgrun branch list demo-app
NAME     STATUS  BASE  PARENT    VERSION  CREATED               EXPIRES
main     ready   yes   -         17       2026-08-27T01:14:19Z  -
new-123  ready   -     branch_1  17       2026-08-27T01:41:05Z  -

$ pgrun branch create demo-app --name e2e-kit --ttl 1h --wait
DATABASE_URL=postgresql://branch_e2e_kit:<redacted>@br-<ref>.us.db.pgrun.dev:5432/postgres?sslmode=require
(exit 0)

$ pgrun branch url demo-app e2e-kit
DATABASE_URL=postgresql://branch_e2e_kit:<redacted>@br-<ref>.us.db.pgrun.dev:5432/postgres?sslmode=require

$ pgrun branch get demo-app e2e-kit --json
{"id":"branch_5","name":"e2e-kit","status":"ready","is_base":false,"parent_branch_id":"branch_1",
 "postgres_version":"17","created_at":"2026-08-29T23:40:15Z","expires_at":"2026-08-30T00:40:15Z",
 "connection_url":"postgresql://branch_e2e_kit:<redacted>@br-<ref>.us.db.pgrun.dev:5432/postgres?sslmode=require"}

$ pgrun branch delete demo-app e2e-kit
branch e2e-kit: deleting
(exit 0)
```

## MCP (`pgrun mcp serve`, scripted stdio session)

```
initialize            → serverInfo {name: "pgrun"}
tools/list            → pgrun_create_branch, pgrun_list_branches, pgrun_get_branch, pgrun_delete_branch
pgrun_create_branch   {project: demo-app, name: e2e-mcp, ttl: 1h, wait: true}
                      → status "ready", connection_url present, isError false
pgrun_delete_branch   {project: demo-app, name: e2e-mcp}
                      → status "deleting", isError false
```

Note: Go's resolver does not special-case `*.localhost` hostnames — use
`http://localhost:<port>` (or a real hostname) for local instances.
