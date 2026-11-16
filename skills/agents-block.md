## Databases: PGRun + PgBot

This project uses [PGRun](https://pgrun.dev) for disposable Postgres branches and PgBot for
read-only database checks. Both come with the `pgrun` CLI (`pgrun setup --json` reports what is
installed; it is idempotent).

- **Never point work at a production DSN.** Create a branch instead:
  `pgrun branch exec --create <task> --ttl 1h --delete-after -- <command>` runs one command with
  `DATABASE_URL` set (never written to a file) and deletes the branch afterwards.
- **Engines.** The server picks the engine: PGX (lightweight, fast to start) where the project
  allows it, PostgreSQL otherwise. `--engine postgres` when the task needs exact PostgreSQL
  behaviour or an extension PGX lacks; `--engine pgx` to insist (an `engine_unavailable` error
  names why, never a silent switch).
- **Check schema changes before you finish.** After writing a migration, run
  `pgrun bot verify --json -- <migrate command>`: it runs the migration on a disposable
  PostgreSQL branch of the Safe Copy, reports rewrites, blocking index builds, drops and renames,
  new lint findings, and cleans up. Add `--compare-pgx` when you developed against PGX.
- **Inspect a branch** (read-only): `pgrun bot lint <branch> --json`, `pgrun bot schema <branch>`,
  `pgrun bot explain <branch> "<SELECT …>" --json`, `pgrun bot inspect <branch> --json`.
- Never print or commit a branch's `DATABASE_URL`; never ask the user to paste a token
  (`pgrun auth login` is theirs to run).
