package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pgrundev/pgrun-cli/internal/config"
	"github.com/pgrundev/pgrun-cli/skills"
)

// `pgrun setup` is the one installation and capability-discovery routine.
// Both install paths converge on it: a developer who installed the CLI runs
// it once, and the pgrun-branching skill tells an agent to run it (it is
// idempotent) before its first branch. It:
//
//   - installs/refreshes the pgrun-branching skill for the chosen agents,
//   - makes pgbot available (keeps a compatible one, else downloads the pinned
//     release — never runs it, never starts a background process),
//   - reports whether the API token is configured (it never logs in for you),
//   - and only when asked (--agents-md <file>) writes a clearly delimited,
//     pgrun-managed block into an AGENTS.md-style file, leaving everything
//     outside the markers untouched.
//
// A failure of one component is reported, not fatal to the others.

const (
	SetupKind    = "pgrun-setup"
	SetupVersion = "1.0.0"

	agentsBlockBegin = "<!-- BEGIN pgrun (managed by `pgrun setup`; edits between these markers are replaced) -->"
	agentsBlockEnd   = "<!-- END pgrun -->"
)

type setupComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"` // installed | updated | current | found | failed | skipped | missing | configured
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

type setupDoc struct {
	Kind         string           `json:"kind"`
	Version      string           `json:"version"`
	PgrunVersion string           `json:"pgrun_version"`
	Ready        bool             `json:"ready"`
	Components   []setupComponent `json:"components"`
	Pgbot        botStatusDoc     `json:"pgbot"`
	Capabilities []string         `json:"capabilities"`
	Next         []string         `json:"next"`
}

var agentSkillRoots = map[string]func(home string) string{
	"claude": func(home string) string { return filepath.Join(home, ".claude", "skills") },
	"codex":  func(home string) string { return filepath.Join(home, ".codex", "skills") },
}

func runSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agents := fs.String("agent", "claude", "comma-separated agents to install the skill for: claude, codex")
	dir := fs.String("dir", "", "install the skill under this skills root instead (any other agent)")
	project := fs.Bool("project", false, "install the skill into ./.claude/skills (this repo only)")
	agentsMD := fs.String("agents-md", "", "also write a pgrun-managed block into this instructions file (e.g. AGENTS.md); content outside the markers is never changed")
	noBot := fs.Bool("no-bot", false, "do not install pgbot")
	jsonOut := fs.Bool("json", false, "print the setup report as JSON (pgrun-setup/1.0.0)")
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "setup: unexpected argument %q", fs.Args()[0])
	}
	if *dir != "" && *project {
		return usageErrf(stderr, "setup: --dir and --project are mutually exclusive")
	}
	doc := setupDoc{Kind: SetupKind, Version: SetupVersion, PgrunVersion: Version, Components: []setupComponent{}}

	// 1. The skill.
	var roots []string
	switch {
	case *dir != "":
		roots = []string{*dir}
	case *project:
		roots = []string{filepath.Join(".claude", "skills")}
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return usageErrf(stderr, "setup: %v", err)
		}
		for _, a := range strings.Split(*agents, ",") {
			a = strings.TrimSpace(a)
			fn, ok := agentSkillRoots[a]
			if !ok {
				return usageErrf(stderr, "setup: unknown --agent %q (claude, codex; use --dir for others)", a)
			}
			roots = append(roots, fn(home))
		}
	}
	skillOK := true
	for _, root := range roots {
		c := installSkillAt(filepath.Join(root, skills.PgrunBranchingName, "SKILL.md"))
		if c.Status == "failed" {
			skillOK = false
		}
		doc.Components = append(doc.Components, c)
	}

	// 2. pgbot.
	if *noBot {
		doc.Pgbot = botStatusNow(context.Background())
		doc.Pgbot.Action = "skipped"
		doc.Components = append(doc.Components, setupComponent{Name: "pgbot", Status: "skipped", Detail: "--no-bot"})
	} else {
		doc.Pgbot = ensureBot(context.Background(), "", false)
		c := setupComponent{Name: "pgbot", Status: doc.Pgbot.Action, Error: doc.Pgbot.Error, Detail: doc.Pgbot.Hint}
		if doc.Pgbot.Binary != nil {
			c.Path = doc.Pgbot.Binary.Path
			if c.Detail == "" {
				c.Detail = "pgbot " + doc.Pgbot.Binary.Version
			}
		}
		doc.Components = append(doc.Components, c)
	}

	// 3. Instructions file, only on request.
	if *agentsMD != "" {
		doc.Components = append(doc.Components, writeAgentsBlock(*agentsMD))
	}

	// 4. Auth: reported, never performed.
	auth := setupComponent{Name: "auth"}
	if cfg, err := config.Resolve("", ""); err == nil && cfg.Token != "" && cfg.URL != "" {
		auth.Status, auth.Detail = "configured", cfg.URL
	} else {
		auth.Status, auth.Detail = "missing", "run `pgrun auth login` yourself (it opens a browser; never paste a token into an agent chat)"
		doc.Next = append(doc.Next, "pgrun auth login")
	}
	doc.Components = append(doc.Components, auth)

	doc.Capabilities = []string{"branch.create", "branch.exec", "branch.delete", "engine.pgx", "engine.postgres"}
	if doc.Pgbot.Installed {
		doc.Capabilities = append(doc.Capabilities, "bot.inspect", "bot.lint", "bot.explain", "bot.verify")
		if doc.Pgbot.Binary != nil && doc.Pgbot.Binary.Capabilities.SchemaReview {
			doc.Capabilities = append(doc.Capabilities, "bot.schema_review", "bot.compare_pgx")
		}
	} else if !*noBot {
		doc.Next = append(doc.Next, "pgrun bot install")
	}
	if doc.Next == nil {
		doc.Next = []string{}
	}
	doc.Ready = skillOK && auth.Status == "configured" && (doc.Pgbot.Installed || *noBot)

	if *jsonOut {
		writeJSONDoc(stdout, doc)
	} else {
		printSetup(stdout, doc)
	}
	if !doc.Ready {
		return exitFailure
	}
	return exitSuccess
}

func installSkillAt(path string) setupComponent {
	c := setupComponent{Name: "skill:" + skills.PgrunBranchingName, Path: path}
	want := []byte(skills.PgrunBranching)
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(existing, want):
		c.Status = "current"
		return c
	case err != nil && !os.IsNotExist(err):
		c.Status, c.Error = "failed", err.Error()
		return c
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.Status, c.Error = "failed", err.Error()
		return c
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		c.Status, c.Error = "failed", err.Error()
		return c
	}
	c.Status = "installed"
	if err == nil {
		c.Status = "updated"
	}
	return c
}

// upsertManagedBlock returns content with the pgrun block inserted (appended
// when absent) or replaced in place. Anything outside the markers is kept
// byte for byte. A file with a BEGIN marker but no END marker is refused:
// guessing where the block ends could delete the user's text.
func upsertManagedBlock(content, block string) (string, error) {
	managed := agentsBlockBegin + "\n" + strings.TrimRight(block, "\n") + "\n" + agentsBlockEnd
	start := strings.Index(content, agentsBlockBegin)
	if start < 0 {
		if strings.Contains(content, agentsBlockEnd) {
			return "", errors.New("found an END pgrun marker without a BEGIN marker; fix the file by hand")
		}
		sep := ""
		switch {
		case content == "":
		case strings.HasSuffix(content, "\n\n"):
		case strings.HasSuffix(content, "\n"):
			sep = "\n"
		default:
			sep = "\n\n"
		}
		return content + sep + managed + "\n", nil
	}
	rel := strings.Index(content[start:], agentsBlockEnd)
	if rel < 0 {
		return "", errors.New("found a BEGIN pgrun marker without an END marker; fix the file by hand")
	}
	end := start + rel + len(agentsBlockEnd)
	if strings.Contains(content[end:], agentsBlockBegin) {
		return "", errors.New("found more than one pgrun block; remove the extra one by hand")
	}
	return content[:start] + managed + content[end:], nil
}

func writeAgentsBlock(path string) setupComponent {
	c := setupComponent{Name: "instructions", Path: path}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		c.Status, c.Error = "failed", err.Error()
		return c
	}
	updated, err := upsertManagedBlock(string(existing), skills.AgentsBlock)
	if err != nil {
		c.Status, c.Error = "failed", err.Error()
		return c
	}
	if updated == string(existing) {
		c.Status = "current"
		return c
	}
	mode := os.FileMode(0o644)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(updated), mode); err != nil {
		c.Status, c.Error = "failed", err.Error()
		return c
	}
	c.Status = "installed"
	if len(existing) > 0 {
		c.Status = "updated"
		c.Detail = "only the block between the pgrun markers changed"
	}
	return c
}

func printSetup(w io.Writer, doc setupDoc) {
	for _, c := range doc.Components {
		line := fmt.Sprintf("%-22s %s", c.Name, c.Status)
		if c.Path != "" {
			line += "  " + c.Path
		}
		fmt.Fprintln(w, line)
		if c.Error != "" {
			fmt.Fprintf(w, "%-22s error: %s\n", "", c.Error)
		}
		if c.Detail != "" && c.Status != "current" {
			fmt.Fprintf(w, "%-22s %s\n", "", c.Detail)
		}
	}
	if doc.Ready {
		fmt.Fprintln(w, "\nready: agents can create PGRun branches (PGX by default where the project allows it) and verify changes with `pgrun bot`.")
	} else if len(doc.Next) > 0 {
		fmt.Fprintf(w, "\nnext: %s\n", strings.Join(doc.Next, " · "))
	}
}
