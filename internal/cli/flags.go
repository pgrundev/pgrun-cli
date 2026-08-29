package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// addAuthFlags registers the two flags that override config resolution for
// one invocation, per the flags > env > file precedence.
func addAuthFlags(fs *flag.FlagSet) (urlFlag, tokenFlag *string) {
	return fs.String("url", "", "API base URL (overrides env/config)"),
		fs.String("token", "", "API token (overrides env/config)")
}

func addJSONFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print the raw API JSON instead of a human summary")
}

// splitPositional takes the first n args as positional operands. pgrun's
// usage is always "<subcommand> <positional...> [flags...]", so the fixed
// positional prefix is peeled off before flag.Parse ever sees the rest —
// stdlib flag stops at the first non-flag argument, which would otherwise
// swallow every flag that follows a leading positional.
func splitPositional(args []string, n int) ([]string, []string, error) {
	if len(args) < n {
		return nil, nil, fmt.Errorf("expected %d argument(s), got %d", n, len(args))
	}
	for _, a := range args[:n] {
		if strings.HasPrefix(a, "-") {
			return nil, nil, fmt.Errorf("expected %d argument(s) before any flags", n)
		}
	}
	return args[:n], args[n:], nil
}

// parseOrExit runs fs.Parse and translates the result into a "should the
// caller stop, and with what code" pair. -h/--help exits 0; any other parse
// failure (flag package already printed it to fs's output) exits 64.
func parseOrExit(fs *flag.FlagSet, args []string) (proceed bool, code int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, exitSuccess
		}
		return false, exitUsage
	}
	return true, exitSuccess
}
