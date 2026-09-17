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

// exitCancelled is the conventional exit status for SIGINT.
const exitCancelled = 130

// authLogin implements `pgrun auth login`. The default is the device flow
// (docs/specs/cli-browser-auth.md in the Rails repo): start a login request,
// open the browser (or print the URL and code), poll until the person
// authorizes, then verify and save the token the server minted — the person
// never sees it. `--paste` keeps the older flow (link to the Tokens page,
// paste a token) for machines with no browser anywhere; CI never gets here —
// it sets PGRUN_API_TOKEN or runs `pgrun auth set --token`.
//
// No signal handling is installed here: it's the device flow's job alone
// (see runAuthLogin/env.withInterrupt). `--paste` blocks in readToken's
// br.ReadString, which never looks at a context, so intercepting SIGINT
// this early would disable the process's default "kill on Ctrl+C" behavior
// without anything to replace it — Ctrl+C would do nothing until the next
// Enter. Passing plain context.Background() here keeps --paste exactly as
// interruptible as v0.2.2.
func authLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runAuthLogin(context.Background(), args, stdin, stdout, stderr, defaultLoginEnv())
}

func runAuthLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, env loginEnv) int {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "API URL (default: PGRUN_API_URL, the saved config, or "+config.DefaultURL+")")
	paste := fs.Bool("paste", false, "paste a token from the Tokens page instead of authorizing in a browser")
	noBrowser := fs.Bool("no-browser", false, "don't open a browser; print the URL and code to open on any device")
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
	resolved, err := config.Resolve(*urlFlag, "")
	if err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	url := strings.TrimRight(resolved.URL, "/")
	if url == "" {
		url = config.DefaultURL
	}

	if *paste {
		return pasteLogin(ctx, url, path, stdin, stdout, stderr)
	}
	if env.ci {
		fmt.Fprintln(stderr, "pgrun: auth login waits for a person to authorize in a browser, which CI can't do —")
		fmt.Fprintln(stderr, "       set PGRUN_API_TOKEN (and PGRUN_API_URL), or run `pgrun auth set --token <TOKEN>`")
		return exitAuth
	}
	// The interrupt-catching context is installed only for the device flow
	// (which polls in a loop and can act on ctx.Done() between requests) —
	// see the note on authLogin. env.withInterrupt is a no-op passthrough in
	// tests, so runAuthLogin stays testable with an injected ctx.
	deviceCtx, stop := env.withInterrupt(ctx)
	defer stop()
	return deviceLogin(deviceCtx, url, path, !*noBrowser && env.canOpenBrowser, stdout, stderr, env)
}

func deviceLogin(ctx context.Context, url, path string, tryBrowser bool, stdout, stderr io.Writer, env loginEnv) int {
	client := api.New(url, "")
	auth, err := client.StartDeviceAuthorization(ctx, deviceName(env.hostname))
	if err != nil {
		if ctx.Err() != nil {
			return cancelled(stderr)
		}
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 429 {
			fmt.Fprintf(stderr, "pgrun: %s\n", apiErr.Message)
		} else {
			fmt.Fprintf(stderr, "pgrun: could not start login: %v\n", err)
		}
		return exitFailure
	}

	openURL := auth.VerificationURIComplete
	if openURL == "" {
		openURL = auth.VerificationURI
	}
	if tryBrowser {
		fmt.Fprintf(stdout, "Opening browser to authenticate (code %s)...\n", auth.UserCode)
		// cmd.Start() succeeding doesn't mean a browser actually showed up
		// (e.g. xdg-open with no handler configured exits non-zero after
		// Start has already returned nil) — always leave a copy-pasteable
		// fallback, not just on a detected Start failure.
		fmt.Fprintf(stdout, "If it didn't open, visit: %s\n", openURL)
		if env.openBrowser(openURL) != nil {
			fmt.Fprintf(stdout, "Could not open your browser.\n\nOpen:\n%s\n\nCode:\n%s\n\n", auth.VerificationURI, auth.UserCode)
		}
	} else {
		fmt.Fprintf(stdout, "Open this on any device to authenticate:\n\nOpen:\n%s\n\nCode:\n%s\n\n", auth.VerificationURI, auth.UserCode)
	}

	fmt.Fprint(stdout, "Waiting for authorization... ")
	interval := time.Duration(auth.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(auth.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 10 * time.Minute
	}
	deadline := env.now().Add(expiresIn)

	var tok api.DeviceToken
poll:
	for {
		if err := env.sleep(ctx, interval); err != nil {
			fmt.Fprintln(stdout)
			return cancelled(stderr)
		}
		if env.now().After(deadline) {
			return pollFailed(stdout, stderr, exitFailure, "the login code expired — run `pgrun auth login` again")
		}
		tok, err = client.PollDeviceToken(ctx, auth.DeviceCode)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(stdout)
				return cancelled(stderr)
			}
			if errors.Is(err, api.ErrMalformedDeviceResponse) {
				return pollFailed(stdout, stderr, exitFailure, "malformed response from the login API")
			}
			// R10: only a transport failure (no HTTP response at all) or a
			// 5xx is transient — retry those until the deadline. Any other
			// *APIError (4xx outside the device-token contract: a bad
			// request, a stale client, ...) is not going to fix itself by
			// waiting, so it ends the loop now instead of being retried
			// silently for up to 10 minutes and misreported as "expired".
			var apiErr *api.APIError
			if errors.As(err, &apiErr) {
				if apiErr.StatusCode >= 500 {
					continue
				}
				return pollFailed(stdout, stderr, exitFailure, fmt.Sprintf("login failed: %s (status %d)", apiErr.Message, apiErr.StatusCode))
			}
			continue // transport error (network blip): keep polling until the deadline
		}
		switch tok.Status {
		case api.DeviceStatusPending:
			continue
		case api.DeviceStatusSlowDown:
			interval += 5 * time.Second
			continue
		case api.DeviceStatusAuthorized:
			break poll
		case api.DeviceStatusDenied:
			return pollFailed(stdout, stderr, exitAuth, "login was denied in the browser")
		case api.DeviceStatusExpired:
			return pollFailed(stdout, stderr, exitFailure, "the login code expired — run `pgrun auth login` again")
		case api.DeviceStatusConsumed:
			return pollFailed(stdout, stderr, exitFailure, "this login was already used — run `pgrun auth login` again")
		case api.DeviceStatusInvalid:
			return pollFailed(stdout, stderr, exitFailure, "the login request was not recognized — run `pgrun auth login` again")
		}
	}
	fmt.Fprintln(stdout, "✓")

	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := api.New(url, tok.Token).VerifyToken(verifyCtx); err != nil {
		fmt.Fprintf(stderr, "pgrun: the new token could not be verified: %v\n", err)
		return exitFailure
	}
	if err := config.Save(path, config.Config{URL: url, Token: tok.Token}); err != nil {
		fmt.Fprintf(stderr, "pgrun: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "\nLogged in as %s\nAccount: %s (%s)\n", tok.User.Email, tok.Account.Name, tok.Account.Slug)
	return exitSuccess
}

func pollFailed(stdout, stderr io.Writer, code int, msg string) int {
	fmt.Fprintln(stdout, "✗")
	fmt.Fprintf(stderr, "pgrun: %s\n", msg)
	return code
}

func cancelled(stderr io.Writer) int {
	fmt.Fprintln(stderr, "Authentication cancelled.")
	return exitCancelled
}

// deviceName is what the Tokens page will call this machine.
func deviceName(hostname func() (string, error)) string {
	name, err := hostname()
	name = strings.TrimSpace(name)
	if err != nil || name == "" {
		return "unknown device"
	}
	return name
}

// pasteLogin is the v0.2.2 flow: link to the Tokens page, read a pasted
// token with echo off, verify, save.
func pasteLogin(ctx context.Context, url, path string, stdin io.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, "pgrun auth login")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  1. Open %s/accounts/default/tokens (sign in if asked)\n", url)
	fmt.Fprintln(stdout, "  2. Click \"New token\" and copy it")
	fmt.Fprintln(stdout, "  3. Paste it below")
	fmt.Fprintln(stdout)
	br := bufio.NewReader(stdin)
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
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := api.New(url, token).VerifyToken(verifyCtx); err != nil {
		var authErr *api.AuthError
		if errors.As(err, &authErr) {
			fmt.Fprintln(stderr, "pgrun: that token was rejected — check it and run `pgrun auth login --paste` again")
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
