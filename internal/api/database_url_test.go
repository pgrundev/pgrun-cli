package api

import (
	"encoding/json"
	"testing"
)

func TestWithDatabaseURL_AddsAliasAndReady(t *testing.T) {
	raw := []byte(`{"id":"b1","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/db","expires_at":null}`)
	out := WithDatabaseURL(raw, "postgres://u:p@host/db")

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if decoded["database_url"] != "postgres://u:p@host/db" {
		t.Fatalf("database_url = %v", decoded["database_url"])
	}
	if decoded["ready"] != true {
		t.Fatalf("ready = %v, want true", decoded["ready"])
	}
	// Original fields survive byte-for-byte in meaning (null stays null).
	if decoded["id"] != "b1" || decoded["status"] != "ready" || decoded["connection_url"] != "postgres://u:p@host/db" {
		t.Fatalf("original fields altered: %s", out)
	}
	if v, present := decoded["expires_at"]; !present || v != nil {
		t.Fatalf("expires_at should still be an explicit null: %s", out)
	}
}

func TestWithDatabaseURL_NonObjectIsReturnedUnchanged(t *testing.T) {
	raw := []byte(`[1,2,3]`)
	if got := string(WithDatabaseURL(raw, "postgres://x")); got != string(raw) {
		t.Fatalf("non-object input should pass through unchanged, got %s", got)
	}
}
