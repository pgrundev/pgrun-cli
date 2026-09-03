// Package skills embeds the agent-facing skills shipped with pgrun so the
// binary can install them (`pgrun skill install`) with no network fetch and
// no separate download step. The Markdown files live beside this package —
// the repo copy IS the embedded copy, so they can never drift.
package skills

import _ "embed"

// PgrunBranchingName is the skill's directory name under a skills root
// (e.g. ~/.claude/skills/pgrun-branching/SKILL.md).
const PgrunBranchingName = "pgrun-branching"

// PgrunBranching is the pgrun-branching skill: the playbook a coding agent
// follows to decide when a task needs a real disposable Postgres, create a
// branch, inject its DATABASE_URL into only the commands that need it, and
// delete the branch when done.
//
//go:embed pgrun-branching/SKILL.md
var PgrunBranching string
