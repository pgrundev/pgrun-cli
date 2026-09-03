package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pgrundev/pgrun-cli/internal/config"
)

// resolveOrHint resolves config and confirms both URL and token are present
// before any command that needs to call the API. Missing either is exit 2
// with the auth hint, matching a rejected token (401) from the API itself.
func resolveOrHint(flagURL, flagToken string, stderr io.Writer) (config.Config, int, bool) {
	cfg, err := config.Resolve(flagURL, flagToken)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return cfg, exitFailure, false
	}
	if cfg.Token == "" || cfg.URL == "" {
		fmt.Fprintf(stderr, "pgrun: no API token/URL configured — %s\n", authHint)
		return cfg, exitAuth, false
	}
	return cfg, exitSuccess, true
}

// leadingPositionals peels every leading token in args that doesn't start
// with "-" off the front, stopping at the first one that does (including a
// literal "--", which needs to reach flag.FlagSet as its own terminator).
// Like splitPositional, this runs before fs.Parse ever sees the rest — but
// unlike splitPositional's fixed count, callers with an optional leading
// <project> don't know how many positionals to expect up front.
func leadingPositionals(args []string) (positionals, rest []string) {
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		i++
	}
	return args[:i], args[i:]
}

// resolveProject fills in a possibly-omitted <project> positional: explicit
// (when non-empty) always wins, otherwise it falls back to the current
// directory's .pgrun/project file (see config.FindProject, which walks up
// like git finding .git). Neither present is a usage error, not an
// operation failure — there's nothing to contact the API about yet.
func resolveProject(explicit string, stderr io.Writer) (slug string, code int, ok bool) {
	if explicit != "" {
		return explicit, exitSuccess, true
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return "", exitFailure, false
	}
	slug, _, err = config.FindProject(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return "", exitFailure, false
	}
	if slug == "" {
		fmt.Fprint(stderr, "pgrun: no project given and no .pgrun/project found — pass <project> or run `pgrun project use <slug>`\n")
		return "", exitUsage, false
	}
	return slug, exitSuccess, true
}
