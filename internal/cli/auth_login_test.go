package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgrundev/pgrun-cli/internal/config"
)

// newVerifyServer starts an httptest server and registers its cleanup —
// just a name-only wrapper so the login tests below read as "a fake API",
// not "an httptest.Server".
func newVerifyServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// runLogin drives authLogin directly (bypassing Run, which has no stdin
// parameter) with a plain io.Reader standing in for a terminal — the
// injection point that makes the interactive flow testable without a real
// TTY. Since the reader is never os.Stdin, readToken's echo-off `stty` path
// is never attempted (see auth_login.go), which is exactly the behavior
// under test here: the token read must work over a plain stream too.
func runLogin(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = authLogin(args, strings.NewReader(stdin), &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

func TestAuthLogin_HappyPath(t *testing.T) {
	dir := isolateHome(t)
	var gotAuth string
	srv := newVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"project not found"}`))
	})

	code, out, stderr := runLogin(t, "my-token-123\n", "--url", srv.URL)
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if strings.Contains(out, "API URL") {
		t.Fatalf("login must not ask for an API URL: %q", out)
	}
	if gotAuth != "Bearer my-token-123" {
		t.Fatalf("verify call Authorization = %q", gotAuth)
	}
	if strings.Contains(out, "my-token-123") {
		t.Fatalf("stdout leaked the token: %q", out)
	}
	if !strings.Contains(out, "my-tok…") {
		t.Fatalf("stdout missing the token fingerprint: %q", out)
	}
	if !strings.Contains(out, srv.URL+"/accounts/default/tokens") {
		t.Fatalf("stdout should link straight to the Tokens page: %q", out)
	}

	path := filepath.Join(dir, ".config", "pgrun", "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file mode = %o, want 0600", perm)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading saved config: %v", err)
	}
	if cfg.URL != srv.URL || cfg.Token != "my-token-123" {
		t.Fatalf("saved config = %+v", cfg)
	}
}

func TestAuthLogin_BadToken_NotSaved(t *testing.T) {
	dir := isolateHome(t)
	srv := newVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})

	code, out, stderr := runLogin(t, "bad-token\n", "--url", srv.URL)
	if code != exitAuth {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitAuth, stderr)
	}
	if strings.Contains(out, "bad-token") || strings.Contains(stderr, "bad-token") {
		t.Fatalf("token leaked: out=%q stderr=%q", out, stderr)
	}
	if !strings.Contains(stderr, "rejected") {
		t.Fatalf("stderr = %q", stderr)
	}

	path := filepath.Join(dir, ".config", "pgrun", "config.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a rejected token must not be saved (stat err = %v)", err)
	}
}

// TestAuthLogin_UsesExistingConfigURL_WithoutAsking: an already logged-in
// URL is reused as-is — the only thing login asks for is the token.
func TestAuthLogin_UsesExistingConfigURL_WithoutAsking(t *testing.T) {
	isolateHome(t)
	srv := newVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	})

	path, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	if err := config.Save(path, config.Config{URL: srv.URL, Token: "old-token"}); err != nil {
		t.Fatalf("seeding config: %v", err)
	}

	code, out, stderr := runLogin(t, "new-token\n")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if strings.Contains(out, "API URL") {
		t.Fatalf("login must not ask for an API URL: %q", out)
	}
	if !strings.Contains(out, srv.URL+"/accounts/default/tokens") {
		t.Fatalf("the Tokens link should use the configured URL: %q", out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading saved config: %v", err)
	}
	if cfg.URL != srv.URL || cfg.Token != "new-token" {
		t.Fatalf("saved config = %+v", cfg)
	}
}

// A fresh install goes straight to the token: pgrun's own URL, a direct
// link to the Tokens page, and the three steps — no URL prompt to confuse.
func TestAuthLogin_FreshInstall_GoesStraightToTheToken(t *testing.T) {
	isolateHome(t)
	code, out, stderr := runLogin(t, "\n") // empty token: stops before any network call
	if code != exitFailure {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitFailure, stderr)
	}
	if strings.Contains(out, "API URL") {
		t.Fatalf("login must not ask for an API URL: %q", out)
	}
	for _, want := range []string{
		config.DefaultURL + "/accounts/default/tokens",
		"New token",
		"Token (input hidden): ",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
}

// PGRUN_API_URL wins over the saved config, and --url over both — the same
// precedence every other command resolves with.
func TestAuthLogin_URLPrecedence_FlagThenEnvThenConfig(t *testing.T) {
	isolateHome(t)
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	if err := config.Save(path, config.Config{URL: "https://config.example.test"}); err != nil {
		t.Fatalf("seeding config: %v", err)
	}
	t.Setenv("PGRUN_API_URL", "https://env.example.test")

	_, out, _ := runLogin(t, "\n")
	if !strings.Contains(out, "https://env.example.test/accounts/default/tokens") {
		t.Fatalf("env should win over config: %q", out)
	}
	_, out, _ = runLogin(t, "\n", "--url", "https://flag.example.test/")
	if !strings.Contains(out, "https://flag.example.test/accounts/default/tokens") {
		t.Fatalf("--url should win over env: %q", out)
	}
}

func TestAuthLogin_EmptyTokenFails(t *testing.T) {
	isolateHome(t)
	code, _, stderr := runLogin(t, "\n", "--url", "https://api.example.com")
	if code != exitFailure {
		t.Fatalf("code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "token is required") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestAuthLogin_UnexpectedArgIsUsage(t *testing.T) {
	isolateHome(t)
	code, _, _ := runLogin(t, "", "extra-arg")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

// --- auth logout ---

func TestAuthLogout_ClearsConfig(t *testing.T) {
	dir := isolateHome(t)
	if code, _, _ := run(t, "auth", "set", "--token", "tok", "--url", "https://api.example.com"); code != exitSuccess {
		t.Fatalf("auth set failed")
	}
	path := filepath.Join(dir, ".config", "pgrun", "config.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config to exist before logout: %v", err)
	}

	code, out, stderr := run(t, "auth", "logout")
	if code != exitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "logged out") {
		t.Fatalf("out = %q", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config file should be gone after logout (stat err = %v)", err)
	}

	// auth status should now report not-configured.
	code, _, stderr = run(t, "auth", "status")
	if code != exitAuth {
		t.Fatalf("auth status after logout: code = %d, want %d", code, exitAuth)
	}
	_ = stderr
}

func TestAuthLogout_AlreadyLoggedOut_ExitsSuccess(t *testing.T) {
	isolateHome(t)
	code, out, stderr := run(t, "auth", "logout")
	if code != exitSuccess {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitSuccess, stderr)
	}
	if out == "" {
		t.Fatalf("expected an explanatory message, got empty stdout")
	}
}

func TestAuthLogout_UnexpectedArgIsUsage(t *testing.T) {
	isolateHome(t)
	code, _, _ := run(t, "auth", "logout", "extra-arg")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}
