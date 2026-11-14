// Package pgbot finds, version-checks and installs the pgbot binary that
// `pgrun bot` drives. pgbot is a separate, versioned dependency — pgrun never
// re-implements it and never bundles a second copy when a compatible one is
// already installed.
//
// Lookup order (the first compatible one wins):
//
//  1. $PGRUN_PGBOT — an explicit path (the npm wrapper sets it to the
//     @pgbot/cli binary it installed alongside @pgrun/cli).
//  2. `pgbot` on $PATH (Homebrew, install.sh, go install, a package manager).
//  3. The managed copy `pgrun bot install` writes: <data dir>/pgrun/bin/pgbot.
//
// A binary older than MinVersion is reported as incompatible, not used.
package pgbot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// MinVersion is the oldest pgbot whose commands and --json contract `pgrun bot`
// relies on (lint --format json, sql explain, inspect --json).
const MinVersion = "0.9.0"

// PinnedVersion is what `pgrun bot install` downloads by default: the release
// this pgrun was tested against.
const PinnedVersion = "0.9.0"

// Capabilities are probed, not inferred from the version, so a dev build or a
// newer release is judged by what it actually has.
type Capabilities struct {
	// SchemaReview: `pgbot schema snapshot|diff|compare` (migration review,
	// PGX-vs-PostgreSQL comparison). Newer than 0.9.0.
	SchemaReview bool `json:"schema_review"`
}

type Binary struct {
	Path         string       `json:"path"`
	Version      string       `json:"version"` // "0.9.0", or "dev"
	Source       string       `json:"source"`  // env | path | managed
	Compatible   bool         `json:"compatible"`
	Problem      string       `json:"problem,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// ErrNotFound: no pgbot anywhere in the lookup order.
var ErrNotFound = errors.New("pgbot is not installed")

func exeName() string {
	if runtime.GOOS == "windows" {
		return "pgbot.exe"
	}
	return "pgbot"
}

// DataDir is where pgrun keeps the managed pgbot: $PGRUN_HOME/bin when set,
// else the platform's per-user data directory.
func DataDir() (string, error) {
	if h := os.Getenv("PGRUN_HOME"); h != "" {
		return h, nil
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "pgrun"), nil
		}
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "pgrun"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "pgrun"), nil
}

// ManagedPath is where `pgrun bot install` puts the binary.
func ManagedPath() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "bin", exeName()), nil
}

// Locate returns the first pgbot in the lookup order, compatible or not
// (callers check Compatible). Candidates that fail to run are skipped, except
// an explicit $PGRUN_PGBOT, which is reported as the problem it is.
func Locate(ctx context.Context) (Binary, error) {
	if p := os.Getenv("PGRUN_PGBOT"); p != "" {
		b := probe(ctx, p, "env")
		return b, nil
	}
	var firstIncompatible *Binary
	if p, err := exec.LookPath("pgbot"); err == nil {
		b := probe(ctx, p, "path")
		if b.Compatible {
			return b, nil
		}
		firstIncompatible = &b
	}
	if p, err := ManagedPath(); err == nil {
		if _, statErr := os.Stat(p); statErr == nil {
			b := probe(ctx, p, "managed")
			if b.Compatible || firstIncompatible == nil {
				return b, nil
			}
		}
	}
	if firstIncompatible != nil {
		return *firstIncompatible, nil
	}
	return Binary{}, ErrNotFound
}

var versionRE = regexp.MustCompile(`(?i)pgbot version v?([0-9]+\.[0-9]+\.[0-9]+[^\s]*|dev)`)

func probe(ctx context.Context, path, source string) Binary {
	b := Binary{Path: path, Source: source}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Probes run with the same scrubbed environment as every other pgbot call:
	// no DATABASE_URL, no libpq variables, no model API keys.
	env := childEnv(os.Environ(), "", false)
	v := exec.CommandContext(ctx, path, "--version")
	v.Env = env
	out, err := v.CombinedOutput()
	if err != nil {
		b.Problem = fmt.Sprintf("%s --version failed: %v", path, err)
		return b
	}
	m := versionRE.FindSubmatch(out)
	if m == nil {
		b.Problem = fmt.Sprintf("%s --version printed %q, not a pgbot version", path, strings.TrimSpace(string(out)))
		return b
	}
	b.Version = string(m[1])
	if b.Version != "dev" && !AtLeast(b.Version, MinVersion) {
		b.Problem = fmt.Sprintf("pgbot %s is older than %s; run `pgrun bot install`", b.Version, MinVersion)
		return b
	}
	b.Compatible = true
	sc := exec.CommandContext(ctx, path, "schema", "snapshot", "--help")
	sc.Env = env
	// Judge by the help text, not only the exit code: the usage line names the
	// command only when pgbot really has it.
	help, err := sc.CombinedOutput()
	b.Capabilities.SchemaReview = err == nil && strings.Contains(string(help), "schema snapshot")
	return b
}

// AtLeast compares dotted numeric versions ("0.10.0" >= "0.9.0"); a
// pre-release suffix ("1.0.0-rc1") compares as its numeric part.
func AtLeast(v, min string) bool {
	a, b := parts(v), parts(min)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}

// Command builds an exec.Cmd for pgbot with databaseURL in the environment
// only (never argv, where `ps` would show it). Model API keys are removed
// from the child's environment unless allowAI: pgbot's ask/explain layer
// would send findings to an external model, and pgrun never does that
// silently.
func Command(ctx context.Context, bin Binary, databaseURL string, allowAI bool, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin.Path, args...)
	cmd.Env = childEnv(os.Environ(), databaseURL, allowAI)
	return cmd
}

// AIKeyEnv lists the variables pgbot's optional AI layer reads.
var AIKeyEnv = []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY",
	"PGBOT_AI_PROVIDER", "PGBOT_AI_BASE_URL", "PGBOT_AI_API_KEY", "PGBOT_AI_MODEL"}

func childEnv(parent []string, databaseURL string, allowAI bool) []string {
	drop := map[string]bool{"DATABASE_URL": true, "PGBOT_DATABASE_URL": true, "PGSERVICE": true,
		// libpq-style variables would let a stray PGHOST/PGPASSWORD override the branch URL's parts.
		"PGHOST": true, "PGPORT": true, "PGUSER": true, "PGPASSWORD": true, "PGDATABASE": true, "PGSSLMODE": true}
	if !allowAI {
		for _, k := range AIKeyEnv {
			drop[k] = true
		}
	}
	env := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		env = append(env, kv)
	}
	if databaseURL != "" {
		env = append(env, "DATABASE_URL="+databaseURL)
	}
	return env
}

// Output runs pgbot and returns stdout; stderr is returned in the error.
func Output(ctx context.Context, bin Binary, databaseURL string, args ...string) ([]byte, int, error) {
	cmd := Command(ctx, bin, databaseURL, false, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
		err = nil // a non-zero exit is pgbot's verdict (findings), not a failure to run
	}
	if err != nil {
		return nil, -1, fmt.Errorf("run pgbot %s: %w", strings.Join(args, " "), err)
	}
	if code >= 3 { // pgbot: 3 = connection/execution failure, 64 = usage
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), code, fmt.Errorf("pgbot %s exited %d: %s", args[0], code, firstLine(msg))
	}
	return stdout.Bytes(), code, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
