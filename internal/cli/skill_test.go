package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgrundev/pgrun-cli/skills"
)

func TestSkillInstall_WritesToClaudeSkillsDirByDefault(t *testing.T) {
	home := isolateHome(t)
	code, out, stderr := run(t, "skill", "install")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr = %s", code, stderr)
	}
	path := filepath.Join(home, ".claude", "skills", "pgrun-branching", "SKILL.md")
	if !strings.Contains(out, "installed pgrun-branching skill at "+path) {
		t.Fatalf("out = %q", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("skill file not written: %v", err)
	}
	if string(data) != skills.PgrunBranching {
		t.Fatalf("written skill differs from the embedded copy")
	}
	if !strings.HasPrefix(string(data), "---\nname: pgrun-branching\n") {
		t.Fatalf("embedded skill lost its frontmatter: %q", string(data)[:40])
	}
}

func TestSkillInstall_IsIdempotentAndUpdatesDriftedCopy(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".claude", "skills", "pgrun-branching", "SKILL.md")

	if code, _, _ := run(t, "skill", "install"); code != exitSuccess {
		t.Fatalf("first install failed")
	}
	code, out, _ := run(t, "skill", "install")
	if code != exitSuccess || !strings.Contains(out, "already up to date") {
		t.Fatalf("second install: code = %d, out = %q", code, out)
	}

	// An older/edited copy is overwritten, and reported as an update.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, "skill", "install")
	if code != exitSuccess || !strings.HasPrefix(out, "updated pgrun-branching skill at ") {
		t.Fatalf("update: code = %d, out = %q", code, out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != skills.PgrunBranching {
		t.Fatalf("stale copy was not replaced")
	}
}

func TestSkillInstall_DirAndProjectTargets(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	code, out, _ := run(t, "skill", "install", "--dir", dir)
	if code != exitSuccess {
		t.Fatalf("--dir: code = %d", code)
	}
	want := filepath.Join(dir, "pgrun-branching", "SKILL.md")
	if !strings.Contains(out, want) {
		t.Fatalf("--dir out = %q, want path %s", out, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("--dir did not write %s: %v", want, err)
	}

	// --project writes relative to the working directory.
	repo := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	code, _, _ = run(t, "skill", "install", "--project")
	if code != exitSuccess {
		t.Fatalf("--project: code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "skills", "pgrun-branching", "SKILL.md")); err != nil {
		t.Fatalf("--project did not write into ./.claude/skills: %v", err)
	}

	code, _, stderr := run(t, "skill", "install", "--project", "--dir", dir)
	if code != exitUsage || !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("--dir + --project: code = %d, stderr = %q", code, stderr)
	}
}

func TestSkillStatus_ExitCodesTrackInstallState(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".claude", "skills", "pgrun-branching", "SKILL.md")

	code, out, _ := run(t, "skill", "status")
	if code != exitFailure || !strings.Contains(out, "not installed") || !strings.Contains(out, "pgrun skill install") {
		t.Fatalf("missing: code = %d, out = %q", code, out)
	}

	run(t, "skill", "install")
	code, out, _ = run(t, "skill", "status")
	if code != exitSuccess || !strings.Contains(out, "installed (current) at "+path) {
		t.Fatalf("current: code = %d, out = %q", code, out)
	}

	os.WriteFile(path, []byte("stale"), 0o644)
	code, out, _ = run(t, "skill", "status")
	if code != exitFailure || !strings.Contains(out, "outdated") {
		t.Fatalf("outdated: code = %d, out = %q", code, out)
	}
}

func TestSkillShow_PrintsEmbeddedSkill(t *testing.T) {
	code, out, _ := run(t, "skill", "show")
	if code != exitSuccess || out != skills.PgrunBranching {
		t.Fatalf("code = %d, out differs from embedded skill", code)
	}
}

func TestSkill_UsageErrors(t *testing.T) {
	code, _, stderr := run(t, "skill")
	if code != exitUsage || !strings.Contains(stderr, "expected a subcommand") {
		t.Fatalf("no subcommand: code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = run(t, "skill", "bogus")
	if code != exitUsage || !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("unknown: code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = run(t, "skill", "install", "extra")
	if code != exitUsage || !strings.Contains(stderr, "unexpected argument") {
		t.Fatalf("extra arg: code = %d, stderr = %q", code, stderr)
	}
}
