package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pgrundev/pgrun-cli/skills"
)

// runSkill implements `pgrun skill ...` — installing the embedded
// pgrun-branching skill where a coding agent will find it. Claude Code is
// the default target (~/.claude/skills); --project targets the current
// repo's .claude/skills, and --dir points at any other skills root (Codex,
// Cursor, a shared dotfiles checkout).
func runSkill(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErrf(stderr, "skill: expected a subcommand (install, status, show)")
	}
	switch args[0] {
	case "install":
		return skillInstall(args[1:], stdout, stderr)
	case "status":
		return skillStatus(args[1:], stdout, stderr)
	case "show":
		return skillShow(args[1:], stdout, stderr)
	default:
		return usageErrf(stderr, "skill: unknown subcommand %q", args[0])
	}
}

// addSkillFlags registers the target-location flags shared by install and
// status. Exactly one of --dir/--project may be given; neither means the
// user-level Claude Code skills directory.
func addSkillFlags(fs *flag.FlagSet) (dir *string, project *bool) {
	dir = fs.String("dir", "", "skills root directory (default: ~/.claude/skills)")
	project = fs.Bool("project", false, "install into ./.claude/skills (this repo only) instead of ~/.claude/skills")
	return dir, project
}

// skillPath resolves where the skill file lives (or would live) for the
// given flags: <root>/pgrun-branching/SKILL.md.
func skillPath(dir string, project bool) (string, error) {
	if dir != "" && project {
		return "", fmt.Errorf("--dir and --project are mutually exclusive")
	}
	root := dir
	switch {
	case root != "":
	case project:
		root = filepath.Join(".claude", "skills")
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, ".claude", "skills")
	}
	return filepath.Join(root, skills.PgrunBranchingName, "SKILL.md"), nil
}

func parseSkillTarget(name string, args []string, stderr io.Writer) (path string, ok bool, code int) {
	fs := flag.NewFlagSet("skill "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir, project := addSkillFlags(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return "", false, code
	}
	if len(fs.Args()) > 0 {
		return "", false, usageErrf(stderr, "skill %s: unexpected argument %q", name, fs.Args()[0])
	}
	path, err := skillPath(*dir, *project)
	if err != nil {
		return "", false, usageErrf(stderr, "skill %s: %v", name, err)
	}
	return path, true, exitSuccess
}

// skillInstall writes the embedded skill to its target path, creating the
// directory tree as needed. Idempotent: an identical file is left alone and
// reported as up to date; a differing one (an older pgrun's copy, a local
// edit) is overwritten, since the embedded copy is the one that matches
// this binary's commands and flags.
func skillInstall(args []string, stdout, stderr io.Writer) int {
	path, ok, code := parseSkillTarget("install", args, stderr)
	if !ok {
		return code
	}
	want := []byte(skills.PgrunBranching)
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(existing, want):
		fmt.Fprintf(stdout, "%s skill already up to date at %s\n", skills.PgrunBranchingName, path)
		return exitSuccess
	case err != nil && !os.IsNotExist(err):
		fmt.Fprintf(stderr, "pgrun: skill install: %v\n", err)
		return exitFailure
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "pgrun: skill install: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		fmt.Fprintf(stderr, "pgrun: skill install: %v\n", err)
		return exitFailure
	}
	verb := "installed"
	if err == nil {
		verb = "updated"
	}
	fmt.Fprintf(stdout, "%s %s skill at %s\n", verb, skills.PgrunBranchingName, path)
	fmt.Fprintln(stdout, "Claude Code picks it up on its next run — no restart of an already-open session needed for new sessions.")
	return exitSuccess
}

// skillStatus reports whether the skill is installed at the target path and
// whether it matches this binary's embedded copy. Exit 0 only when it is
// installed AND current, so a script (or an onboarding check) can rely on
// the code alone.
func skillStatus(args []string, stdout, stderr io.Writer) int {
	path, ok, code := parseSkillTarget("status", args, stderr)
	if !ok {
		return code
	}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(existing, []byte(skills.PgrunBranching)):
		fmt.Fprintf(stdout, "%s: installed (current) at %s\n", skills.PgrunBranchingName, path)
		return exitSuccess
	case err == nil:
		fmt.Fprintf(stdout, "%s: installed (outdated) at %s — run `pgrun skill install` to update\n", skills.PgrunBranchingName, path)
		return exitFailure
	case os.IsNotExist(err):
		fmt.Fprintf(stdout, "%s: not installed (expected at %s) — run `pgrun skill install`\n", skills.PgrunBranchingName, path)
		return exitFailure
	default:
		fmt.Fprintf(stderr, "pgrun: skill status: %v\n", err)
		return exitFailure
	}
}

// skillShow prints the embedded skill to stdout — for agents and hosts that
// read a skill from a pipe rather than a directory, and for eyeballing what
// `install` would write.
func skillShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skill show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "skill show: unexpected argument %q", fs.Args()[0])
	}
	io.WriteString(stdout, skills.PgrunBranching)
	return exitSuccess
}
