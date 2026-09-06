package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/api"
	"github.com/pgrundev/pgrun-cli/internal/config"
)

// authLogin implements `pgrun auth login`: a friendly interactive prompt for
// the two things `pgrun auth set` expects on the command line, verified
// against the real API before anything is saved. It is the everyday path;
// `pgrun auth set --token` remains the advanced/scriptable/CI path (it never
// touches a terminal and never makes a network call before saving).
//
// stdin is a parameter (not read from os.Stdin directly) so tests can drive
// the whole flow with a plain io.Reader instead of a real TTY — see
// readToken for how that also decides whether to attempt to disable
// terminal echo.
func authLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "auth login: unexpected argument %q", fs.Args()[0])
	}

	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	existing, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}

	fmt.Fprintln(stdout, "pgrun auth login")
	br := bufio.NewReader(stdin)

	defaultURL := existing.URL
	if defaultURL == "" {
		defaultURL = config.DefaultURL
	}
	url, err := promptLine(br, stdout, "API URL", defaultURL)
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: reading API URL: %v\n", err)
		return exitFailure
	}
	if url == "" {
		fmt.Fprintln(stderr, "pgrun: an API URL is required")
		return exitFailure
	}

	fmt.Fprintf(stdout, "\nGet a token from the dashboard's Tokens page: %s (sign in, then your account -> Tokens)\n", strings.TrimRight(url, "/"))
	token, err := readToken(br, stdin, stdout, stderr, "Token (input hidden): ")
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: reading token: %v\n", err)
		return exitFailure
	}
	if token == "" {
		fmt.Fprintln(stderr, "pgrun: a token is required")
		return exitFailure
	}

	fmt.Fprintln(stdout, "verifying token...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := api.New(url, token).VerifyToken(ctx); err != nil {
		var authErr *api.AuthError
		if errors.As(err, &authErr) {
			fmt.Fprintln(stderr, "pgrun: that token was rejected — check it and run `pgrun auth login` again")
			return exitAuth
		}
		fmt.Fprintf(stderr, "pgrun: could not verify token: %v\n", err)
		return exitFailure
	}

	if err := config.Save(path, config.Config{URL: url, Token: token}); err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "\nlogged in — saved token %s to %s\nurl: %s\n", config.Fingerprint(token), path, url)
	return exitSuccess
}

// authLogout implements `pgrun auth logout`: removes the stored config file.
// Exits 0 even when there was nothing to remove — "log out" when you're
// already logged out is success, not an error.
func authLogout(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if ok, code := parseOrExit(fs, args); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "auth logout: unexpected argument %q", fs.Args()[0])
	}

	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "not logged in — nothing to do")
			return exitSuccess
		}
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "logged out — removed %s\n", path)
	return exitSuccess
}

// promptLine prints "label [def]: " (or "label: " when def is empty), reads
// one line, and returns def when the line is blank — the standard
// press-enter-to-accept-the-default prompt shape.
func promptLine(br *bufio.Reader, out io.Writer, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// readToken prints prompt, then reads one line as the token. When stdin is
// literally os.Stdin (the process's real standard input — the only stream a
// child `stty` process sharing that fd can affect) it first tries to
// disable local terminal echo via `stty -echo`/`stty echo` through os/exec;
// if that fails (no `stty` binary, or stdin isn't actually a terminal) it
// prints a warning and reads visibly instead. Tests pass a plain io.Reader
// (never os.Stdin), which deliberately skips the whole TTY dance and goes
// straight to a plain read — that's the injection point that makes this
// testable without a real terminal.
func readToken(br *bufio.Reader, stdin io.Reader, stdout, stderr io.Writer, prompt string) (string, error) {
	fmt.Fprint(stdout, prompt)

	if stdin == os.Stdin {
		if err := setEcho(false); err == nil {
			// deferred so a panic in ReadString can never leave the user's
			// terminal with echo disabled.
			defer func() {
				if restoreErr := setEcho(true); restoreErr != nil {
					fmt.Fprintln(stderr, "warning: could not restore terminal echo — run `stty echo` if your terminal looks odd afterward")
				}
				fmt.Fprintln(stdout) // the newline from pressing Enter was never echoed
			}()
		} else {
			// Noun-free: readToken also reads connection URLs (source
			// add/update), not just tokens.
			fmt.Fprintln(stderr, "warning: could not disable terminal echo (is `stty` installed?) — the value will be visible as you type it")
		}
	}

	line, err := br.ReadString('\n')

	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// setEcho toggles local echo on the process's controlling terminal via
// `stty`. Stdlib-only substitute for golang.org/x/term's terminal-raw-mode
// helpers: `stty`'s own stdin must be the real terminal fd for it to have
// anything to act on, which is exactly os.Stdin here (readToken only calls
// this when stdin == os.Stdin).
func setEcho(on bool) error {
	arg := "echo"
	if !on {
		arg = "-echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
