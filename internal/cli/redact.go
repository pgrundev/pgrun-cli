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

// newRedactor scrubs a connection URL from anything the CLI prints. It
// replaces the full URL, the URL with its password stripped, the raw
// password, and the password's percent-encoded form with "[redacted]", so
// even an API error that echoes the request back cannot leak the secret.
func newRedactor(secretURL string) *strings.Replacer {
	pairs := []string{}
	add := func(s string) {
		if s != "" {
			pairs = append(pairs, s, "[redacted]")
		}
	}
	add(secretURL)
	if u, err := url.Parse(secretURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
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

// redactedArg renders an unexpected positional argument for a usage
// message. Usage errors are printed before any redactor exists (there is
// no accepted URL yet), and the argument a user is most likely to get
// wrong here is a bare connection URL typed where a name belongs — so a
// value shaped like one is reported by shape, never by value.
func redactedArg(s string) string {
	if validConnectionURL(s) {
		return `"[redacted]"`
	}
	return fmt.Sprintf("%q", s)
}
