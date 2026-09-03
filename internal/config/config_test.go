package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")

	want := Config{URL: "https://api.example.com", Token: "sekret"}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

func TestSaveWrites0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Save(path, Config{URL: "u", Token: "t"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 0600", perm)
	}
}

func TestSaveForces0600OnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := Save(path, Config{URL: "u", Token: "t"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 0600 even though file preexisted looser", perm)
	}
}

func TestLoadMissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(filepath.Join(dir, "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != (Config{}) {
		t.Fatalf("got %+v, want zero value", got)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

// TestResolvePrecedence exercises flags > env > file, one layer at a time.
func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // os.UserHomeDir() honors $HOME on darwin/linux
	t.Setenv("PGRUN_API_URL", "")
	t.Setenv("PGRUN_API_TOKEN", "")

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := Save(path, Config{URL: "file-url", Token: "file-token"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File only.
	cfg, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.URL != "file-url" || cfg.Token != "file-token" {
		t.Fatalf("file layer: got %+v", cfg)
	}

	// Env overrides file.
	t.Setenv("PGRUN_API_URL", "env-url")
	t.Setenv("PGRUN_API_TOKEN", "env-token")
	cfg, err = Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.URL != "env-url" || cfg.Token != "env-token" {
		t.Fatalf("env layer: got %+v", cfg)
	}

	// Flags override env (and file).
	cfg, err = Resolve("flag-url", "flag-token")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.URL != "flag-url" || cfg.Token != "flag-token" {
		t.Fatalf("flag layer: got %+v", cfg)
	}
}

func TestResolveNoConfigIsZeroValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PGRUN_API_URL", "")
	t.Setenv("PGRUN_API_TOKEN", "")

	cfg, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("got %+v, want zero value", cfg)
	}
}

func TestFingerprint(t *testing.T) {
	cases := []struct{ token, want string }{
		{"", "…"},
		{"abc", "abc…"},
		{"abcdef", "abcdef…"},
		{"abcdefghij", "abcdef…"},
	}
	for _, c := range cases {
		if got := Fingerprint(c.token); got != c.want {
			t.Errorf("Fingerprint(%q) = %q, want %q", c.token, got, c.want)
		}
	}
}

// --- FindProject / SaveProject (.pgrun/project) ---

func TestSaveProjectFindProjectRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, err := SaveProject(dir, "jobsgpt")
	if err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	wantPath := filepath.Join(dir, ProjectFileDir, "project")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	slug, gotPath, err := FindProject(dir)
	if err != nil {
		t.Fatalf("FindProject: %v", err)
	}
	if slug != "jobsgpt" || gotPath != wantPath {
		t.Fatalf("FindProject = (%q, %q), want (%q, %q)", slug, gotPath, "jobsgpt", wantPath)
	}
}

func TestFindProjectWalksUpFromNestedSubdir(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveProject(dir, "jobsgpt"); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	slug, path, err := FindProject(nested)
	if err != nil {
		t.Fatalf("FindProject: %v", err)
	}
	if slug != "jobsgpt" {
		t.Fatalf("slug = %q, want jobsgpt", slug)
	}
	if want := filepath.Join(dir, ProjectFileDir, "project"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestFindProjectAbsentReturnsZeroValues(t *testing.T) {
	dir := t.TempDir()
	slug, path, err := FindProject(dir)
	if err != nil {
		t.Fatalf("FindProject: %v", err)
	}
	if slug != "" || path != "" {
		t.Fatalf("FindProject = (%q, %q), want (\"\", \"\")", slug, path)
	}
}

func TestFindProjectIgnoresCommentAndBlankFirstLines(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, ProjectFileDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "\n# a comment\n  jobsgpt  \n"
	if err := os.WriteFile(filepath.Join(projectDir, "project"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	slug, _, err := FindProject(dir)
	if err != nil {
		t.Fatalf("FindProject: %v", err)
	}
	if slug != "jobsgpt" {
		t.Fatalf("slug = %q, want jobsgpt (blank/comment lines and surrounding whitespace should be skipped/trimmed)", slug)
	}
}

func TestFingerprintNeverContainsFullLongToken(t *testing.T) {
	token := "pgrunfaketoken_abcdefghijklmnopqrstuvwxyz0123456789"
	fp := Fingerprint(token)
	if fp == token {
		t.Fatal("fingerprint equals the full token")
	}
	if len(fp) >= len(token) {
		t.Fatalf("fingerprint %q is not shorter than the token", fp)
	}
}
