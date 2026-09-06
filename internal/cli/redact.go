// This file is the secret-handling core of `pgrun source add` and `pgrun
// source update`. A customer's production connection URL is the most
// sensitive value this CLI ever touches, and the rule for it is absolute:
// it travels in a JSON request body and nowhere else — never a terminal,
// never a log, never a --json stream.
//
// Redaction here is structural rather than case-by-case. As soon as a
// command holds a URL it replaces its own stdout and stderr with writers
// that scrub it, BEFORE the first API call, so every later write — an API
// error built from a server response that echoed the request back, a
// --json passthrough of that same body, a timeout message — goes through
// the scrubber whether or not whoever wrote that line remembered the rule.
// Being right by construction beats being right by review.
//
// One known limit: scrubbing happens per Write call, so a secret split
// across two writes would slip through. Every writer in this package emits
// one whole message per Write (fmt.Fprint* formats first, dumpJSON writes
// the body in one call), which is what makes that safe.

package cli

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// minStandalonePassword is the length below which a password is scrubbed
// only as part of a URL, never on its own. A one- or two-character
// password would otherwise replace those characters everywhere they appear
// in ordinary text — mangling a human message and, worse, corrupting the
// --json body a machine consumer is parsing. Short passwords still travel
// inside the full and password-stripped URLs, which is how a server echoes
// a request back in practice.
const minStandalonePassword = 4

// jsonTokenPassword reports whether a password is, on its own, a bare JSON
// token: true, false, null, or a run of digits. Such a password is scrubbed
// only inside a URL — replacing it standalone would rewrite an unrelated
// `"policy_version":null` or `"size_bytes":12345` into
// `"policy_version":[redacted]`, handing a machine consumer of --json a body
// it cannot parse. Same trade-off as minStandalonePassword: the full and
// stripped URLs still carry it, which is how a server echoes a request back
// in practice.
func jsonTokenPassword(pw string) bool {
	switch pw {
	case "true", "false", "null":
		return true
	}
	// Short all-digit passwords collide with numbers in a JSON body; a long
	// generated numeric one is worth scrubbing standalone.
	if pw == "" || len(pw) > 8 {
		return false
	}
	for _, r := range pw {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// newRedactor scrubs a connection URL from anything the CLI prints. It
// replaces the full URL, the URL with its password stripped, and — for a
// password of at least minStandalonePassword characters that is not itself a
// JSON token — the raw password and its percent-encoded forms with
// "[redacted]", so even an API error that echoes the request back cannot
// leak the secret.
func newRedactor(secretURL string) *strings.Replacer {
	pairs := []string{}
	add := func(s string) {
		if s != "" {
			pairs = append(pairs, s, "[redacted]")
		}
	}
	add(secretURL)
	if u, err := url.Parse(secretURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && len(pw) >= minStandalonePassword && !jsonTokenPassword(pw) {
			add(pw)
			add(url.QueryEscape(pw))
			add(url.PathEscape(pw))
		}
		stripped := *u
		stripped.User = url.User(u.User.Username())
		add(stripped.String())
	}
	return strings.NewReplacer(pairs...)
}

type redactingWriter struct {
	w io.Writer
	r *strings.Replacer
}

func (rw redactingWriter) Write(p []byte) (int, error) {
	if _, err := rw.r.WriteString(rw.w, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// redactWriter wraps w so everything written through it is scrubbed by r.
// It reports the caller's own byte count rather than the (shorter, once
// something was replaced) count actually written downstream — an io.Writer
// that claims a short write makes fmt.Fprintf report a spurious error.
func redactWriter(w io.Writer, r *strings.Replacer) io.Writer { return redactingWriter{w: w, r: r} }

// redactedArg renders an unexpected argument for a usage message. Usage
// errors are printed before any redactor exists (there is no accepted URL
// yet), and the argument a user is most likely to get wrong here is a bare
// connection URL typed where a name, a subcommand or a disposition belongs —
// so a value shaped like one is reported by shape, never by value. Every
// usage message of the source commands (and the top-level unknown-command
// error, the first thing a user typing a URL by mistake can hit) that would
// otherwise print an argument with %q goes through this function; a format
// string that echoes user input is a print site for a secret whether or not
// its author was thinking about one. The other command families still use
// %q — they never take a connection URL.
//
// It asks containsConnectionURL, not validConnectionURL: a refusal that
// failed open on "POSTGRES://…" — or on a URL embedded in a longer argument
// such as "public.users.email=postgres://…" — would leak exactly the value
// it exists to hide.
func redactedArg(s string) string {
	if containsConnectionURL(s) {
		return `"[redacted]"`
	}
	return fmt.Sprintf("%q", s)
}
