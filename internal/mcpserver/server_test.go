package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgrun/internal/api"
)

// session drives one Server.Serve over real in-memory pipes (io.Pipe), the
// way a real MCP host would talk to the process over stdio, rather than
// pre-buffering every request into one []byte.
type session struct {
	t    *testing.T
	inW  *io.PipeWriter
	outR *bufio.Reader
	done chan error
}

func startSession(t *testing.T, client *api.Client) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := New(client, "test")

	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inR, outW) }()

	t.Cleanup(func() { inW.Close() })
	return &session{t: t, inW: inW, outR: bufio.NewReader(outR), done: done}
}

func (s *session) send(v any) {
	s.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	if _, err := s.inW.Write(append(data, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
}

// sendRaw writes a line verbatim — for malformed-input tests.
func (s *session) sendRaw(line string) {
	s.t.Helper()
	if _, err := s.inW.Write([]byte(line + "\n")); err != nil {
		s.t.Fatalf("write raw line: %v", err)
	}
}

func (s *session) recv() map[string]any {
	s.t.Helper()
	line, err := s.outR.ReadString('\n')
	if err != nil {
		s.t.Fatalf("read response: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		s.t.Fatalf("response is not valid JSON: %v (line=%q)", err, line)
	}
	return resp
}

// closeAndWait closes the input side and waits for Serve to return, failing
// the test if Serve errored or didn't finish in time.
func (s *session) closeAndWait() {
	s.t.Helper()
	s.inW.Close()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("Serve returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		s.t.Fatal("Serve did not return after input closed")
	}
}

func newTestAPIServer(t *testing.T, handler http.HandlerFunc) *api.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return api.New(srv.URL, "test-token")
}

func TestFullSession(t *testing.T) {
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"creating","is_base":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"branches":[{"id":"branch_1","name":"main","status":"ready","is_base":true}]}`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"ready","is_base":false,"connection_url":"postgres://u:p@host/feature-x"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"status":"deleting"}`))
		}
	})
	s := startSession(t, client)

	// initialize
	s.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	resp := s.recv()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize: no result: %+v", resp)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	serverInfo, _ := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "pgrun" {
		t.Fatalf("serverInfo.name = %v", serverInfo["name"])
	}

	// notifications/initialized — a notification (no id), must get no reply.
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	// tools/list — if the notification above wrongly produced output, this
	// read would pick up its stray bytes instead and fail to match id 2.
	s.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp = s.recv()
	if id, _ := resp["id"].(float64); id != 2 {
		t.Fatalf("tools/list: unexpected id %v (stray output from the notification?): %+v", resp["id"], resp)
	}
	toolsResult, _ := resp["result"].(map[string]any)
	tools, _ := toolsResult["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools/list: got %d tools, want 4: %+v", len(tools), toolsResult)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		m := tool.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"pgrun_create_branch", "pgrun_list_branches", "pgrun_get_branch", "pgrun_delete_branch"} {
		if !names[want] {
			t.Fatalf("tools/list missing %q: %+v", want, names)
		}
	}

	// tools/call: create with wait:false — immediate creating-status response.
	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_create_branch", "arguments": map[string]any{
			"project": "proj1", "name": "feature-x", "wait": false,
		}},
	})
	resp = s.recv()
	text := firstContentText(t, resp)
	if !strings.Contains(text, `"status":"creating"`) {
		t.Fatalf("create wait:false: unexpected content: %s", text)
	}

	// tools/call: list_branches
	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_list_branches", "arguments": map[string]any{"project": "proj1"}},
	})
	resp = s.recv()
	text = firstContentText(t, resp)
	if !strings.Contains(text, `"branches"`) || !strings.Contains(text, "main") {
		t.Fatalf("list_branches: unexpected content: %s", text)
	}

	// tools/call: get_branch — ready, includes connection_url verbatim.
	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_get_branch", "arguments": map[string]any{"project": "proj1", "name": "feature-x"}},
	})
	resp = s.recv()
	text = firstContentText(t, resp)
	if !strings.Contains(text, "postgres://u:p@host/feature-x") {
		t.Fatalf("get_branch: missing connection_url: %s", text)
	}

	// tools/call: delete_branch
	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_delete_branch", "arguments": map[string]any{"project": "proj1", "name": "feature-x"}},
	})
	resp = s.recv()
	text = firstContentText(t, resp)
	if !strings.Contains(text, "deleting") {
		t.Fatalf("delete_branch: unexpected content: %s", text)
	}

	s.closeAndWait()
}

func TestCreateBranch_Wait(t *testing.T) {
	old := api.PollInterval
	api.PollInterval = time.Millisecond
	t.Cleanup(func() { api.PollInterval = old })

	calls := 0
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"creating"}`))
			return
		}
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"creating"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"ready","connection_url":"postgres://u:p@host/feature-x"}`))
	})
	s := startSession(t, client)

	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_create_branch", "arguments": map[string]any{
			"project": "proj1", "name": "feature-x", "timeout_seconds": 5,
		}},
	})
	resp := s.recv()
	text := firstContentText(t, resp)
	if !strings.Contains(text, `"status":"ready"`) || !strings.Contains(text, "postgres://u:p@host/feature-x") {
		t.Fatalf("create wait (default true): unexpected content: %s", text)
	}
	// database_url is an agent-ergonomics alias for connection_url, added on
	// top of the raw response once the branch is ready — an agent shouldn't
	// need to know the API's own field name or make a second call for it.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("content is not valid JSON: %v (%s)", err, text)
	}
	if decoded["database_url"] != "postgres://u:p@host/feature-x" {
		t.Fatalf("database_url = %v, want the connection_url value: %s", decoded["database_url"], text)
	}
	if decoded["ready"] != true {
		t.Fatalf("ready = %v, want true: %s", decoded["ready"], text)
	}
	s.closeAndWait()
}

// TestCreateBranch_Wait_DeletedIsError is F1's scripted-poll case for MCP: a
// branch observed "deleted" mid-wait must stop the loop immediately (not
// run out timeout_seconds) and come back isError:true naming the deletion.
func TestCreateBranch_Wait_DeletedIsError(t *testing.T) {
	old := api.PollInterval
	api.PollInterval = time.Millisecond
	t.Cleanup(func() { api.PollInterval = old })

	calls := 0
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"creating"}`))
			return
		}
		calls++
		w.WriteHeader(http.StatusOK)
		if calls < 2 {
			w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"creating"}`))
			return
		}
		w.Write([]byte(`{"id":"branch_2","name":"feature-x","status":"deleted"}`))
	})
	s := startSession(t, client)

	start := time.Now()
	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_create_branch", "arguments": map[string]any{
			"project": "proj1", "name": "feature-x", "timeout_seconds": 10,
		}},
	})
	resp := s.recv()
	elapsed := time.Since(start)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result carrying isError, got: %+v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	text := firstContentText(t, resp)
	if !strings.Contains(text, "deleted") {
		t.Fatalf("message should name the deletion: %s", text)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s — looks like it waited out timeout_seconds instead of stopping on \"deleted\"", elapsed)
	}
	s.closeAndWait()
}

// TestCreateBranch_InvalidTTL_IsError is F2: a bad ttl must be rejected
// before any request is sent (the handler failing the test proves it), with
// isError naming the valid set.
func TestCreateBranch_InvalidTTL_IsError(t *testing.T) {
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	s := startSession(t, client)

	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_create_branch", "arguments": map[string]any{
			"project": "proj1", "name": "feature-x", "ttl": "2h",
		}},
	})
	resp := s.recv()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result carrying isError, got: %+v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	text := firstContentText(t, resp)
	for _, want := range []string{"1h", "6h", "24h", "7d"} {
		if !strings.Contains(text, want) {
			t.Fatalf("message should name the valid ttl set, missing %q: %s", want, text)
		}
	}
	s.closeAndWait()
}

// TestCreateBranch_InvalidProjectName_IsError is F3's MCP-level case: ".."
// must never reach the network.
func TestCreateBranch_InvalidProjectName_IsError(t *testing.T) {
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the server: %s %s", r.Method, r.URL.Path)
	})
	s := startSession(t, client)

	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_get_branch", "arguments": map[string]any{
			"project": "..", "name": "feature-x",
		}},
	})
	resp := s.recv()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result carrying isError, got: %+v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	s.closeAndWait()
}

func TestMalformedJSONLine_NoCrash(t *testing.T) {
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	s := startSession(t, client)

	s.sendRaw(`{not valid json`)
	resp := s.recv()
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error for malformed input: %+v", resp)
	}
	if code, _ := errObj["code"].(float64); code != codeParseError {
		t.Fatalf("error code = %v, want %d", errObj["code"], codeParseError)
	}

	// The server must still be alive and answer the next well-formed request.
	s.send(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "tools/list"})
	resp = s.recv()
	if _, ok := resp["result"]; !ok {
		t.Fatalf("server did not recover after malformed input: %+v", resp)
	}
	s.closeAndWait()
}

func TestUnknownTool_IsError(t *testing.T) {
	client := newTestAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	s := startSession(t, client)

	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_does_not_exist", "arguments": map[string]any{}},
	})
	resp := s.recv()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a normal JSON-RPC result carrying isError, got: %+v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("isError = %v, want true: %+v", result["isError"], result)
	}
	s.closeAndWait()
}

func TestUnknownMethod_JSONRPCError(t *testing.T) {
	s := startSession(t, nil)
	s.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "bogus/method"})
	resp := s.recv()
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got: %+v", resp)
	}
	if code, _ := errObj["code"].(float64); code != codeMethodNotFound {
		t.Fatalf("error code = %v, want %d", errObj["code"], codeMethodNotFound)
	}
	s.closeAndWait()
}

func TestMissingClient_ToolCallIsErrorWithHint(t *testing.T) {
	s := startSession(t, nil) // no client — simulates missing env config
	s.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "pgrun_list_branches", "arguments": map[string]any{"project": "proj1"}},
	})
	resp := s.recv()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a normal result carrying isError, got: %+v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	text := firstContentText(t, resp)
	if !strings.Contains(text, "PGRUN_API_TOKEN") {
		t.Fatalf("missing setup hint: %s", text)
	}
}

func TestNotificationNeverReplied(t *testing.T) {
	s := startSession(t, nil)
	// A request with no "id" field at all — a notification, even for a
	// method that would normally reply (initialize).
	s.sendRaw(`{"jsonrpc":"2.0","method":"initialize","params":{}}`)
	// Prove no output was produced for it: the next real request's response
	// must be the very next line, with the id we expect.
	s.send(map[string]any{"jsonrpc": "2.0", "id": 42, "method": "tools/list"})
	resp := s.recv()
	if id, _ := resp["id"].(float64); id != 42 {
		t.Fatalf("got id %v, want 42 — a notification produced stray output", resp["id"])
	}
	s.closeAndWait()
}

// firstContentText extracts result.content[0].text, failing the test if the
// shape doesn't match content:[{type:"text",text:<json>}].
func firstContentText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %+v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in result: %+v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok || first["type"] != "text" {
		t.Fatalf("content[0] is not {type:text,...}: %+v", content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		t.Fatalf("content[0].text is not a string: %+v", first)
	}
	return text
}
