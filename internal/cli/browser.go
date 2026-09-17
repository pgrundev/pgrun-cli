package cli

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"
)

// loginEnv is everything `pgrun auth login` asks the operating system, so
// tests can drive the whole flow without a browser, a TTY or a real clock.
type loginEnv struct {
	openBrowser    func(url string) error
	canOpenBrowser bool // an interactive terminal with a display, not over SSH
	ci             bool // CI env var is truthy: never wait for a person
	hostname       func() (string, error)
	sleep          func(ctx context.Context, d time.Duration) error
	now            func() time.Time
	// withInterrupt installs Ctrl+C handling for the device flow's polling
	// loop only — never for --paste, whose blocking token read can't act on
	// a context (see auth_login.go's authLogin). Tests use a passthrough
	// that returns ctx unchanged, since they drive cancellation directly.
	withInterrupt func(ctx context.Context) (context.Context, func())
}

func defaultLoginEnv() loginEnv {
	return loginEnv{
		openBrowser:    openBrowser,
		canOpenBrowser: stdinIsTerminal() && hasDisplay(),
		ci:             ciEnvTruthy(os.Getenv("CI")),
		hostname:       os.Hostname,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		now: time.Now,
		withInterrupt: func(ctx context.Context) (context.Context, func()) {
			return signal.NotifyContext(ctx, os.Interrupt)
		},
	}
}

// ciEnvTruthy: GitHub Actions, GitLab, CircleCI… all set CI=true; "0" and
// "false" are how people switch it off.
func ciEnvTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false":
		return false
	}
	return true
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// hasDisplay: over SSH a browser would open on the far machine (or nowhere);
// on Linux without X11/Wayland there is nothing to open one on.
func hasDisplay() bool {
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if os.Getenv(v) != "" {
			return false
		}
	}
	if runtime.GOOS == "linux" {
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
	return true
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it; the opener exits on its own
	return nil
}
