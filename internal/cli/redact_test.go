package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRedactor_FullURL covers the four forms newRedactor is built to catch
// for one ordinary connection URL: the URL itself, the bare password, the
// password's percent-encoded form (how it actually appears inside the URL),
// and the URL with the password stripped out (how a server or a driver
// often echoes it back).
func TestRedactor_FullURL(t *testing.T) {
	const secret = "postgres://app:s3cr3t-p%40ss@db.example.com:5432/prod?sslmode=require"
	red := newRedactor(secret)

	cases := map[string]string{
		"full URL":         secret,
		"raw password":     "s3cr3t-p@ss",
		"encoded password": "s3cr3t-p%40ss",
		"stripped URL":     "postgres://app@db.example.com:5432/prod?sslmode=require",
	}
	for what, in := range cases {
		got := red.Replace("server said: " + in + " is bad")
		if strings.Contains(got, "s3cr3t") {
			t.Errorf("%s: %q still contains the password", what, got)
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("%s: %q has no [redacted] marker", what, got)
		}
	}
}

// TestRedactor_PercentEncodedPassword uses a password whose encoded form
// differs from its raw form in more than one byte (a space and a percent),
// so the PathEscape/QueryEscape variants newRedactor adds are actually
// exercised rather than coinciding with the raw password.
func TestRedactor_PercentEncodedPassword(t *testing.T) {
	const secret = "postgres://u:p%40ss%20w%25rd@h:5432/db"
	red := newRedactor(secret)

	for _, in := range []string{
		secret,                   // as typed
		"p@ss w%rd",              // decoded, as a driver would report it
		"p@ss%20w%25rd",          // path-escaped, as it appears inside the URL
		"p%40ss+w%25rd",          // query-escaped
		"postgres://u@h:5432/db", // password stripped
	} {
		got := red.Replace(in)
		if got != "[redacted]" {
			t.Errorf("Replace(%q) = %q, want %q", in, got, "[redacted]")
		}
	}
}

// TestRedactor_NoPassword: a URL with no password at all still has the URL
// itself scrubbed, and nothing else is disturbed.
func TestRedactor_NoPassword(t *testing.T) {
	const secret = "postgres://u@h/db"
	red := newRedactor(secret)

	if got := red.Replace(secret); got != "[redacted]" {
		t.Errorf("Replace(secret) = %q, want [redacted]", got)
	}
	const unrelated = "connection refused: could not translate host name"
	if got := red.Replace(unrelated); got != unrelated {
		t.Errorf("Replace(%q) = %q — unrelated text must pass through", unrelated, got)
	}
}

// TestRedactor_UnparsableSecret: url.Parse failing must not disable
// redaction — the literal string the user gave us is still scrubbed.
func TestRedactor_UnparsableSecret(t *testing.T) {
	const secret = "postgres://u:p@%zz/db" // %zz is not a valid escape
	red := newRedactor(secret)
	if got := red.Replace("rejected " + secret); got != "rejected [redacted]" {
		t.Errorf("Replace = %q, want %q", got, "rejected [redacted]")
	}
}

// TestRedactor_EmptySecret: a zero-pair Replacer must be usable (no panic)
// and must leave everything alone.
func TestRedactor_EmptySecret(t *testing.T) {
	red := newRedactor("")
	const s = "nothing to hide here"
	if got := red.Replace(s); got != s {
		t.Errorf("Replace(%q) = %q", s, got)
	}
}

// TestRedactWriter scrubs on the way through to the underlying writer and
// still reports the caller's byte count (an io.Writer that claims a short
// write makes fmt.Fprintf report a spurious error).
func TestRedactWriter(t *testing.T) {
	const secret = "postgres://app:SECRETPW@h/db"
	var buf bytes.Buffer
	w := redactWriter(&buf, newRedactor(secret))

	payload := []byte(`{"error":"rejected ` + secret + `"}`)
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("n = %d, want %d (a short-write report breaks fmt.Fprintf)", n, len(payload))
	}
	if strings.Contains(buf.String(), "SECRETPW") {
		t.Fatalf("underlying writer got the secret: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("underlying writer = %q, want a [redacted] marker", buf.String())
	}
}
