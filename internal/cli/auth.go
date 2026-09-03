package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pgrundev/pgrun-cli/internal/config"
)

func runAuth(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErrf(stderr, "auth: expected a subcommand (login, logout, set, status)")
	}
	switch args[0] {
	case "login":
		return authLogin(args[1:], os.Stdin, stdout, stderr)
	case "logout":
		return authLogout(args[1:], stdout, stderr)
	case "set":
		return authSet(args[1:], stdout, stderr)
	case "status":
		return authStatus(args[1:], stdout, stderr)
	default:
		return usageErrf(stderr, "auth: unknown subcommand %q", args[0])
	}
}

// authSet implements `pgrun auth set` — the advanced/manual/CI path: writes
// a token straight to the config file with no prompt and no verification
// call. `pgrun auth login` (auth_login.go) is the everyday interactive path
// for a human at a terminal; scripts and CI should prefer this or the
// PGRUN_API_TOKEN/PGRUN_API_URL env vars, neither of which touch a TTY.
func authSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	token := fs.String("token", "", "API token (required)")
	urlFlag := fs.String("url", "", "API base URL")
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "auth set: unexpected argument %q", fs.Args()[0])
	}
	if *token == "" {
		return usageErrf(stderr, "auth set: --token is required")
	}

	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	cfg.Token = *token
	if *urlFlag != "" {
		cfg.URL = *urlFlag
	}
	if err := config.Save(path, cfg); err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "saved token %s to %s\n", config.Fingerprint(cfg.Token), path)
	return exitSuccess
}

func authStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag, tokenFlag := addAuthFlags(fs)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "auth status: unexpected argument %q", fs.Args()[0])
	}

	cfg, err := config.Resolve(*urlFlag, *tokenFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	if cfg.Token == "" {
		fmt.Fprintf(stderr, "pgrun: not configured — %s\n", authHint)
		return exitAuth
	}
	url := cfg.URL
	if url == "" {
		url = "(not set)"
	}
	fmt.Fprintf(stdout, "url: %s\ntoken: %s\n", url, config.Fingerprint(cfg.Token))
	return exitSuccess
}
