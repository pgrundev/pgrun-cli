package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgrundev/pgrun-cli/internal/config"
)

// newVerifyServer starts an httptest server and registers its cleanup.
func newVerifyServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// fakeLoginAPI scripts the device endpoints: every poll pops the next
// (status, body) pair, repeating the last one once the script runs out.
// Any other path answers the VerifyToken probe (404 = token accepted).
type fakeLoginAPI struct {
	mu          sync.Mutex
	polls       [][2]any // {int status, string body}
	pollCount   int
	startBody   string
	startStatus int
	deviceName  string
	verifyAuth  string
}

func (f *fakeLoginAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/cli/device":
			buf := new(bytes.Buffer)
			buf.ReadFrom(r.Body)
			f.deviceName = buf.String()
			status := f.startStatus
			if status == 0 {
				status = http.StatusCreated
			}
			w.WriteHeader(status)
			w.Write([]byte(f.startBody))
		case "/api/cli/device/token":
			i := f.pollCount
			if i >= len(f.polls) {
				i = len(f.polls) - 1
			}
			f.pollCount++
			w.WriteHeader(f.polls[i][0].(int))
			w.Write([]byte(f.polls[i][1].(string)))
		default:
			f.verifyAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"project not found"}`))
		}
	}
}

func startBody(base string) string {
	return `{"device_code":"dc-secret","user_code":"WDJB-MJHT","verification_uri":"` + base + `/cli/auth","verification_uri_complete":"` + base + `/cli/auth?code=WDJB-MJHT","expires_in":600,"interval":5}`
}

const authorizedBody = `{"status":"authorized","token":"pgrun_devicetoken123","user":{"email":"alex@example.com"},"account":{"id":"default-26","slug":"default-26","name":"Default"}}`

// testEnv is a loginEnv that never touches the OS: the browser "opens"
// (recording the URL) unless openErr is set, sleeping is instant, and the
// clock advances by the slept duration.
type testEnv struct {
	opened  []string
	openErr error
	clock   time.Time
	slept   []time.Duration
}

func (e *testEnv) env(canOpen, ci bool) loginEnv {
	e.clock = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	return loginEnv{
		openBrowser:    func(u string) error { e.opened = append(e.opened, u); return e.openErr },
		canOpenBrowser: canOpen,
		ci:             ci,
		hostname:       func() (string, error) { return "alex-macbook", nil },
		sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			e.slept = append(e.slept, d)
			e.clock = e.clock.Add(d)
			return nil
		},
		now: func() time.Time { return e.clock },
	}
}

func runDeviceLogin(t *testing.T, ctx context.Context, env loginEnv, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runAuthLogin(ctx, args, strings.NewReader(""), &out, &errBuf, env)
	return code, out.String(), errBuf.String()
}

func TestAuthLogin_Device_HappyPath_OpensBrowserPollsSavesAndVerifies(t *testing.T) {
	dir := isolateHome(t)
	api := &fakeLoginAPI{polls: [][2]any{{200, `{"status":"authorization_pending"}`}, {200, `{"status":"authorization_pending"}`}, {200, authorizedBody}}}
	srv := newVerifyServer(t, api.handler(t))
	api.startBody = startBody(srv.URL)
	te := &testEnv{}

	code, out, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", srv.URL)
	if code != exitSuccess {
		t.Fatalf("code = %d stderr=%q out=%q", code, stderr, out)
	}
	if len(te.opened) != 1 || te.opened[0] != srv.URL+"/cli/auth?code=WDJB-MJHT" {
		t.Fatalf("opened = %v", te.opened)
	}
	if !strings.Contains(api.deviceName, `"device_name":"alex-macbook"`) {
		t.Fatalf("start body = %q", api.deviceName)
	}
	for _, want := range []string{"Opening browser to authenticate", "WDJB-MJHT", "Waiting for authorization... ✓", "Logged in as alex@example.com", "Account: Default (default-26)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "pgrun_devicetoken123") || strings.Contains(stderr, "pgrun_devicetoken123") || strings.Contains(out, "dc-secret") {
		t.Fatalf("a secret leaked: out=%q stderr=%q", out, stderr)
	}
	if strings.Count(out, "Waiting for authorization") != 1 {
		t.Fatalf("polling must not be noisy: %q", out)
	}
	if api.pollCount != 3 || len(te.slept) != 3 || te.slept[0] != 5*time.Second {
		t.Fatalf("polls=%d slept=%v", api.pollCount, te.slept)
	}
	if api.verifyAuth != "Bearer pgrun_devicetoken123" {
		t.Fatalf("the token must be verified before saving; verify auth = %q", api.verifyAuth)
	}
	cfg, err := config.Load(filepath.Join(dir, ".config", "pgrun", "config.json"))
	if err != nil || cfg.URL != srv.URL || cfg.Token != "pgrun_devicetoken123" {
		t.Fatalf("saved config = %+v err=%v", cfg, err)
	}
}

func TestAuthLogin_Device_BrowserOpenFails_PrintsURLAndCodeAndKeepsPolling(t *testing.T) {
	isolateHome(t)
	api := &fakeLoginAPI{polls: [][2]any{{200, authorizedBody}}}
	srv := newVerifyServer(t, api.handler(t))
	api.startBody = startBody(srv.URL)
	te := &testEnv{openErr: errors.New("no xdg-open")}

	code, out, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", srv.URL)
	if code != exitSuccess {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	for _, want := range []string{"Could not open your browser.", "Open:\n" + srv.URL + "/cli/auth\n", "Code:\nWDJB-MJHT\n", "Logged in as alex@example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
}

func TestAuthLogin_Device_NoBrowserAvailable_NeverTriesToOpen(t *testing.T) {
	isolateHome(t)
	for _, tc := range []struct {
		name    string
		canOpen bool
		args    []string
	}{
		{"ssh/no display/non-tty", false, nil},
		{"--no-browser", true, []string{"--no-browser"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeLoginAPI{polls: [][2]any{{200, authorizedBody}}}
			srv := newVerifyServer(t, api.handler(t))
			api.startBody = startBody(srv.URL)
			te := &testEnv{}
			code, out, stderr := runDeviceLogin(t, context.Background(), te.env(tc.canOpen, false), append([]string{"--url", srv.URL}, tc.args...)...)
			if code != exitSuccess {
				t.Fatalf("code = %d stderr=%q", code, stderr)
			}
			if len(te.opened) != 0 {
				t.Fatalf("must not open a browser: %v", te.opened)
			}
			if !strings.Contains(out, "Open:\n"+srv.URL+"/cli/auth\n") || !strings.Contains(out, "Code:\nWDJB-MJHT\n") {
				t.Fatalf("stdout = %q", out)
			}
		})
	}
}

func TestAuthLogin_Device_TerminalFailures(t *testing.T) {
	cases := []struct {
		name     string
		poll     [2]any
		wantCode int
		wantErr  string
	}{
		{"denied", [2]any{403, `{"status":"access_denied"}`}, exitAuth, "denied"},
		{"expired", [2]any{410, `{"status":"expired"}`}, exitFailure, "expired"},
		{"consumed", [2]any{410, `{"status":"consumed"}`}, exitFailure, "already used"},
		{"invalid", [2]any{404, `{"status":"invalid_device_code"}`}, exitFailure, "not recognized"},
		{"malformed", [2]any{200, `{"status":"party"}`}, exitFailure, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateHome(t)
			api := &fakeLoginAPI{polls: [][2]any{tc.poll}}
			srv := newVerifyServer(t, api.handler(t))
			api.startBody = startBody(srv.URL)
			te := &testEnv{}
			code, _, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", srv.URL)
			if code != tc.wantCode || !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("code = %d stderr = %q", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, ".config", "pgrun", "config.json")); !os.IsNotExist(err) {
				t.Fatalf("nothing may be saved on failure (stat err = %v)", err)
			}
		})
	}
}

func TestAuthLogin_Device_SlowDownAndTransientErrorsKeepPolling(t *testing.T) {
	isolateHome(t)
	api := &fakeLoginAPI{polls: [][2]any{{429, `{"status":"slow_down"}`}, {502, `bad gateway`}, {200, authorizedBody}}}
	srv := newVerifyServer(t, api.handler(t))
	api.startBody = startBody(srv.URL)
	te := &testEnv{}
	code, _, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", srv.URL)
	if code != exitSuccess {
		t.Fatalf("code = %d stderr=%q", code, stderr)
	}
	if len(te.slept) != 3 || te.slept[1] != 10*time.Second {
		t.Fatalf("slow_down must add 5s to the interval; slept = %v", te.slept)
	}
}

func TestAuthLogin_Device_StopsAtTheExpiryDeadline(t *testing.T) {
	isolateHome(t)
	api := &fakeLoginAPI{polls: [][2]any{{200, `{"status":"authorization_pending"}`}}}
	srv := newVerifyServer(t, api.handler(t))
	api.startBody = startBody(srv.URL)
	te := &testEnv{}
	code, _, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", srv.URL)
	if code != exitFailure || !strings.Contains(stderr, "expired") {
		t.Fatalf("code = %d stderr = %q", code, stderr)
	}
	if api.pollCount > 121 {
		t.Fatalf("polled past the 600s deadline: %d polls", api.pollCount)
	}
}

func TestAuthLogin_Device_CtrlC_Cancels(t *testing.T) {
	isolateHome(t)
	api := &fakeLoginAPI{polls: [][2]any{{200, `{"status":"authorization_pending"}`}}}
	srv := newVerifyServer(t, api.handler(t))
	api.startBody = startBody(srv.URL)
	te := &testEnv{}
	ctx, cancel := context.WithCancel(context.Background())
	env := te.env(true, false)
	realSleep := env.sleep
	env.sleep = func(c context.Context, d time.Duration) error {
		if len(te.slept) == 2 {
			cancel() // the person presses Ctrl+C while waiting
		}
		return realSleep(c, d)
	}
	code, _, stderr := runDeviceLogin(t, ctx, env, "--url", srv.URL)
	if code != exitCancelled || !strings.Contains(stderr, "Authentication cancelled.") {
		t.Fatalf("code = %d stderr = %q", code, stderr)
	}
}

func TestAuthLogin_Device_StartFailures(t *testing.T) {
	cases := []struct {
		name        string
		startStatus int
		startBody   string
		wantErr     string
	}{
		{"throttled", 429, `{"error":"too many login attempts — wait a few minutes and try again"}`, "too many login attempts"},
		{"malformed", 201, `{"nope":true}`, "malformed"},
		{"server error", 500, `boom`, "could not start login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			api := &fakeLoginAPI{startStatus: tc.startStatus, startBody: tc.startBody}
			srv := newVerifyServer(t, api.handler(t))
			te := &testEnv{}
			code, _, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", srv.URL)
			if code != exitFailure || !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("code = %d stderr = %q", code, stderr)
			}
			if len(te.opened) != 0 {
				t.Fatalf("no browser without a login request: %v", te.opened)
			}
		})
	}
}

func TestAuthLogin_Device_UnreachableAPI(t *testing.T) {
	isolateHome(t)
	te := &testEnv{}
	code, _, stderr := runDeviceLogin(t, context.Background(), te.env(true, false), "--url", "http://127.0.0.1:1")
	if code != exitFailure || !strings.Contains(stderr, "could not start login") {
		t.Fatalf("code = %d stderr = %q", code, stderr)
	}
}

func TestAuthLogin_CI_RefusesBrowserLoginWithAHint(t *testing.T) {
	isolateHome(t)
	te := &testEnv{}
	code, _, stderr := runDeviceLogin(t, context.Background(), te.env(true, true), "--url", "http://127.0.0.1:1")
	if code != exitAuth {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"PGRUN_API_TOKEN", "pgrun auth set --token"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q: %q", want, stderr)
		}
	}
	if len(te.opened) != 0 {
		t.Fatalf("CI must never open a browser")
	}
}

func TestCIEnvIsTruthy(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "false": false, "FALSE": false, "true": true, "1": true, "yes": true} {
		if got := ciEnvTruthy(value); got != want {
			t.Fatalf("ciEnvTruthy(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestDeviceName(t *testing.T) {
	if got := deviceName(func() (string, error) { return "alex-macbook.local", nil }); got != "alex-macbook.local" {
		t.Fatalf("got %q", got)
	}
	if got := deviceName(func() (string, error) { return "", errors.New("no hostname") }); got != "unknown device" {
		t.Fatalf("got %q", got)
	}
}

// --- --paste: the v0.2.2 flow, kept for machines with no browser anywhere ---

// runLogin drives the paste flow with a plain io.Reader standing in for a
// terminal (readToken skips its stty dance for anything but os.Stdin).
func runLogin(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	te := &testEnv{}
	code = runAuthLogin(context.Background(), append([]string{"--paste"}, args...), strings.NewReader(stdin), &outBuf, &errBuf, te.env(true, false))
	return code, outBuf.String(), errBuf.String()
}

func TestAuthLogin_Paste_HappyPath(t *testing.T) {
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
	if gotAuth != "Bearer my-token-123" {
		t.Fatalf("verify call Authorization = %q", gotAuth)
	}
	if strings.Contains(out, "my-token-123") {
		t.Fatalf("stdout leaked the token: %q", out)
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
	if err != nil || cfg.URL != srv.URL || cfg.Token != "my-token-123" {
		t.Fatalf("saved config = %+v err=%v", cfg, err)
	}
}

func TestAuthLogin_Paste_BadToken_NotSaved(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(dir, ".config", "pgrun", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("a rejected token must not be saved (stat err = %v)", err)
	}
}

func TestAuthLogin_Paste_EmptyTokenFails(t *testing.T) {
	isolateHome(t)
	code, _, stderr := runLogin(t, "\n", "--url", "https://api.example.com")
	if code != exitFailure || !strings.Contains(stderr, "token is required") {
		t.Fatalf("code = %d stderr = %q", code, stderr)
	}
}

func TestAuthLogin_URLPrecedence_FlagThenEnvThenConfig(t *testing.T) {
	isolateHome(t)
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	if err := config.Save(path, config.Config{URL: "https://config.example.test"}); err != nil {
		t.Fatalf("seeding config: %v", err)
	}
	_, out, _ := runLogin(t, "\n")
	if !strings.Contains(out, "https://config.example.test/accounts/default/tokens") {
		t.Fatalf("config URL should be used: %q", out)
	}
	t.Setenv("PGRUN_API_URL", "https://env.example.test")
	_, out, _ = runLogin(t, "\n")
	if !strings.Contains(out, "https://env.example.test/accounts/default/tokens") {
		t.Fatalf("env should win over config: %q", out)
	}
	_, out, _ = runLogin(t, "\n", "--url", "https://flag.example.test/")
	if !strings.Contains(out, "https://flag.example.test/accounts/default/tokens") {
		t.Fatalf("--url should win over env: %q", out)
	}
}

func TestAuthLogin_UnexpectedArgIsUsage(t *testing.T) {
	isolateHome(t)
	te := &testEnv{}
	code, _, _ := runDeviceLogin(t, context.Background(), te.env(true, false), "extra-arg")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestAuthSet_StillWorksForCI(t *testing.T) {
	dir := isolateHome(t)
	code, _, stderr := run(t, "auth", "set", "--token", "ci-token", "--url", "https://api.example.com")
	if code != exitSuccess {
		t.Fatalf("code = %d stderr = %q", code, stderr)
	}
	cfg, err := config.Load(filepath.Join(dir, ".config", "pgrun", "config.json"))
	if err != nil || cfg.Token != "ci-token" {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
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
