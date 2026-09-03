package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/pgrundev/pgrun-cli/internal/api"
)

// handleAPIError maps a client error to an exit code and a sanitized
// message. In --json mode it also dumps the raw error body (the API's own
// {"error": "..."} JSON) to stdout, if one was received.
func handleAPIError(err error, jsonOut bool, raw []byte, stdout, stderr io.Writer) int {
	var nameErr *api.InvalidNameError
	if errors.As(err, &nameErr) {
		// Never reached the API — a malformed argument, not an operation/API
		// failure, so it gets the usage exit code like a bad flag would.
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitUsage
	}

	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		if jsonOut && raw != nil {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: authentication failed — %s\n", authHint)
		return exitAuth
	}

	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		if jsonOut && raw != nil {
			dumpJSON(stdout, raw)
		}
		fmt.Fprintf(stderr, "pgrun: %s\n", apiErr.Message)
		return exitFailure
	}

	// Never reached the API (network/build/decode failure) — nothing raw to
	// show either way.
	fmt.Fprintf(stderr, "pgrun: %v\n", err)
	return exitFailure
}

// dumpJSON writes raw verbatim, adding a trailing newline if it lacks one.
func dumpJSON(w io.Writer, raw []byte) {
	w.Write(raw)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		fmt.Fprintln(w)
	}
}
