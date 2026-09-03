// Package config resolves and persists pgrun's API URL and token.
// Precedence (highest first): flags > env (PGRUN_API_URL / PGRUN_API_TOKEN)
// > file (~/.config/pgrun/config.json, mode 0600).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is both the file format and the resolved result.
type Config struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
}

// DefaultURL is pgrun's built-in default API/dashboard host. `pgrun auth
// login` pre-fills its URL prompt with this when nothing is configured yet
// (an existing config file's URL, if any, takes priority over it).
const DefaultURL = "https://app.pgrun.dev"

// Path returns the fixed config file location, ~/.config/pgrun/config.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "pgrun", "config.json"), nil
}

// Load reads the config file. A missing file is not an error — it returns a
// zero Config, matching "nothing configured yet".
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg as the config file, creating its parent directory and
// forcing mode 0600 — the token is a secret, and Save enforces that even if
// a file already existed with looser permissions.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return os.Chmod(path, 0o600) // WriteFile's perm only applies on create
}

// Resolve merges the file, environment, and explicit flag values in
// precedence order (flags win, then env, then file). flagURL/flagToken
// should be the literal flag values ("" meaning unset).
func Resolve(flagURL, flagToken string) (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	cfg, err := Load(path)
	if err != nil {
		return Config{}, err
	}
	if v := os.Getenv("PGRUN_API_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("PGRUN_API_TOKEN"); v != "" {
		cfg.Token = v
	}
	if flagURL != "" {
		cfg.URL = flagURL
	}
	if flagToken != "" {
		cfg.Token = flagToken
	}
	return cfg, nil
}

// ProjectFileDir is the repo-local directory holding the selected project
// slug: <repo>/.pgrun/project. Not a secret (unlike the token config file
// above) — never put a token in it.
const ProjectFileDir = ".pgrun"

// projectFileName is the file within ProjectFileDir that holds the slug.
const projectFileName = "project"

// FindProject walks up from startDir — the same way git locates .git —
// looking for a .pgrun/project file. It returns the slug taken from the
// file's first non-empty line that doesn't start with "#" (trimmed of
// surrounding whitespace), along with the file's path. Absence is not an
// error: reaching the filesystem root without finding one returns
// ("", "", nil), and the same applies if a file is found but has no
// slug-shaped line in it. A non-nil error only means a read actually
// failed (permissions, etc.).
func FindProject(startDir string) (slug, path string, err error) {
	dir := startDir
	for {
		candidate := filepath.Join(dir, ProjectFileDir, projectFileName)
		data, readErr := os.ReadFile(candidate)
		switch {
		case readErr == nil:
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				return line, candidate, nil
			}
			return "", "", nil
		case !os.IsNotExist(readErr):
			return "", "", fmt.Errorf("reading %s: %w", candidate, readErr)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", nil
		}
		dir = parent
	}
}

// SaveProject writes slug to dir/.pgrun/project, creating the directory as
// needed and overwriting any existing file. Mode 0644, not 0600 — this file
// is not a secret. Returns the path written.
func SaveProject(dir, slug string) (path string, err error) {
	projectDir := filepath.Join(dir, ProjectFileDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", projectDir, err)
	}
	path = filepath.Join(projectDir, projectFileName)
	if err := os.WriteFile(path, []byte(slug+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// Fingerprint is the safe-to-display stand-in for a token: its first 6
// characters plus an ellipsis. Never return or log the token itself.
func Fingerprint(token string) string {
	n := len(token)
	if n > 6 {
		n = 6
	}
	return token[:n] + "…"
}
