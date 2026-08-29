package cli

import (
	"fmt"
	"io"

	"github.com/pgrundev/pgrun/internal/config"
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
