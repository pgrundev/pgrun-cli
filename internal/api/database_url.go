package api

import "encoding/json"

// WithDatabaseURL returns raw (a JSON object) with an added "database_url"
// field set to databaseURL — an agent-ergonomics alias for connection_url —
// and "ready": true, since callers only reach for this once a branch is
// ready and credentialed.
// Added specifically for `branch create --wait` and the MCP
// pgrun_create_branch tool so an agent gets id/name/status/database_url
// back from one call, without needing to know the "connection_url" field
// name or make a second GetBranch call just to learn it.
//
// Every other field's bytes are preserved exactly as the server sent them
// (decoded as json.RawMessage, not reinterpreted) — only the new field and
// the enclosing object are freshly encoded. Falls back to raw unmodified if
// it doesn't decode as a JSON object; that should never happen against a
// well-formed API response, but this must never corrupt --json output.
func WithDatabaseURL(raw []byte, databaseURL string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	encoded, err := json.Marshal(databaseURL)
	if err != nil {
		return raw
	}
	obj["database_url"] = encoded
	// A ready branch is the only time this is called, so say so explicitly:
	// an agent scripting against --json checks one boolean instead of
	// comparing status strings.
	obj["ready"] = json.RawMessage("true")
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}
