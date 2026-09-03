// Package mcpserver implements pgrun's MCP server: a hand-rolled JSON-RPC
// 2.0 loop over stdio, one message per line (no Content-Length framing —
// the common lightweight stdio transport). No SDK, stdlib only.
//
// Four tools mirror the CLI's branch subcommands exactly:
// pgrun_create_branch, pgrun_list_branches, pgrun_get_branch,
// pgrun_delete_branch.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pgrundev/pgrun/internal/api"
)

const protocolVersion = "2024-11-05"

// Server serves one MCP session over a line-delimited JSON-RPC stream.
// Client is nil when the environment had no PGRUN_API_URL/PGRUN_API_TOKEN —
// initialize/tools/list still work (a host can discover the tools before
// credentials exist); every tools/call then returns isError with the setup
// hint instead of panicking on a nil client.
type Server struct {
	Client  *api.Client
	Version string
}

// New builds a Server. client may be nil (see Client's doc comment).
func New(client *api.Client, version string) *Server {
	return &Server{Client: client, Version: version}
}

// Serve reads one JSON-RPC message per line from in until EOF, dispatching
// each synchronously and writing any response to out before reading the
// next line. It never returns an error for a malformed line — that becomes
// a JSON-RPC parse-error reply instead — only for a genuine I/O failure on
// in, or if the initial buffer allocation is exceeded by an absurd line.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		s.handleLine(ctx, line, out)
	}
	return scanner.Err()
}

// rpcRequest's ID is raw JSON so it can be echoed back verbatim (string,
// number, or absent) without pgrun inventing its own ID type. Absence of the
// field (ID == nil after unmarshal) marks a notification: never replied to,
// per JSON-RPC 2.0 — that's how notifications/initialized is handled, and
// it generalizes to any method sent without an id.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErrorObj    `json:"error,omitempty"`
}

type rpcErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC 2.0 reserved error codes used here.
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

func (s *Server) handleLine(ctx context.Context, line []byte, out io.Writer) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(out, nil, codeParseError, "parse error")
		return
	}
	isNotification := req.ID == nil

	switch req.Method {
	case "initialize":
		if isNotification {
			return
		}
		s.writeResult(out, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"serverInfo":      map[string]any{"name": "pgrun", "version": s.Version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case "notifications/initialized":
		// Explicitly no reply — it's the client's own notification.
	case "tools/list":
		if isNotification {
			return
		}
		s.writeResult(out, req.ID, map[string]any{"tools": toolDefinitions})
	case "tools/call":
		result, rpcErr := s.callTool(ctx, req.Params)
		if isNotification {
			return
		}
		if rpcErr != nil {
			s.writeError(out, req.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		s.writeResult(out, req.ID, result)
	default:
		if isNotification {
			return
		}
		s.writeError(out, req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) writeResult(out io.Writer, id json.RawMessage, result any) {
	s.write(out, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeError(out io.Writer, id json.RawMessage, code int, msg string) {
	s.write(out, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcErrorObj{Code: code, Message: msg}})
}

func (s *Server) write(out io.Writer, resp rpcResponse) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // schema text like "branch_<id>" shouldn't become <id>
	if err := enc.Encode(resp); err != nil {
		// A response built entirely from our own maps/structs; this would be
		// a pgrun bug, not caller input. Nothing sane to do but drop it —
		// the server must never crash on a write it can't service either.
		return
	}
	out.Write(buf.Bytes()) // Encode already appended the trailing newline
}

// toolCallParams is tools/call's params shape: {name, arguments}.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolHandler runs one tool and returns its MCP content result (built by
// jsonContent or toolError — always a normal JSON-RPC *result*, since a
// business-logic failure is reported via isError, not a JSON-RPC error).
type toolHandler func(s *Server, ctx context.Context, args json.RawMessage) any

var toolHandlers = map[string]toolHandler{
	"pgrun_create_branch": (*Server).toolCreateBranch,
	"pgrun_list_branches": (*Server).toolListBranches,
	"pgrun_get_branch":    (*Server).toolGetBranch,
	"pgrun_delete_branch": (*Server).toolDeleteBranch,
}

// callTool dispatches tools/call. Only a malformed params envelope becomes a
// JSON-RPC protocol error (-32602); an unknown tool name or a missing
// client is a normal result with isError:true, per the MCP tool-error
// convention.
func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcErrorObj) {
	var call toolCallParams
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, &rpcErrorObj{Code: codeInvalidParams, Message: "invalid params"}
	}

	handler, ok := toolHandlers[call.Name]
	if !ok {
		return toolError(fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
	if s.Client == nil {
		return toolError("no PGRun API credentials — set PGRUN_API_URL and PGRUN_API_TOKEN in the MCP host's env for pgrun (see `pgrun auth set --token <TOKEN> [--url <URL>]` for the CLI equivalent)"), nil
	}
	return handler(s, ctx, call.Arguments), nil
}

// jsonContent wraps a raw API response as the MCP text content result.
func jsonContent(raw []byte) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(raw)}},
	}
}

// toolError wraps a sanitized message as an isError MCP result — never the
// token, never a raw error stack.
func toolError(msg string) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

// apiToolError maps a Client error to a sanitized isError result.
func apiToolError(err error) any {
	var nameErr *api.InvalidNameError
	if errors.As(err, &nameErr) {
		return toolError(err.Error())
	}
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		return toolError("authentication failed — the configured PGRUN_API_TOKEN was rejected")
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return toolError(apiErr.Message)
	}
	return toolError(err.Error()) // network/build failure — never contains the token
}

type createBranchArgs struct {
	Project        string   `json:"project"`
	Name           string   `json:"name"`
	TTL            string   `json:"ttl"`
	Parent         string   `json:"parent"`
	Wait           *bool    `json:"wait"`
	TimeoutSeconds *float64 `json:"timeout_seconds"`
}

func (s *Server) toolCreateBranch(ctx context.Context, raw json.RawMessage) any {
	var a createBranchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolError("invalid arguments")
	}
	if a.Project == "" || a.Name == "" {
		return toolError("project and name are required")
	}
	if a.TTL != "" && !api.ValidTTL(a.TTL) {
		return toolError(fmt.Sprintf("ttl must be one of 1h, 6h, 24h, 7d (got %q)", a.TTL))
	}

	wait := true
	if a.Wait != nil {
		wait = *a.Wait
	}
	timeout := 300 * time.Second
	if a.TimeoutSeconds != nil {
		timeout = time.Duration(*a.TimeoutSeconds * float64(time.Second))
	}

	respRaw, branch, err := s.Client.CreateBranch(ctx, a.Project, api.CreateBranchRequest{
		Name: a.Name, TTL: a.TTL, ParentBranchID: a.Parent,
	})
	if err != nil {
		return apiToolError(err)
	}
	if !wait {
		return jsonContent(respRaw)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	respRaw, branch, err = s.Client.WaitForTerminal(waitCtx, a.Project, respRaw, branch)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return toolError(fmt.Sprintf("timed out waiting for branch %s to become ready (last status: %s)", branch.Name, branch.Status))
		}
		return apiToolError(err)
	}
	// branch.Status is Terminal here — ready is the one success; any other
	// terminal status (failed/deleted/stopped/unhealthy) is a wait failure.
	if branch.Status != api.StatusReady {
		return toolError(api.WaitFailureReason(branch.Name, branch.Status))
	}
	if branch.ConnectionURL != "" {
		// Agent ergonomics: add a "database_url" alias for connection_url so
		// the tool result carries id/name/status/database_url from this one
		// call — no second pgrun_get_branch just to learn the field name.
		respRaw = api.WithDatabaseURL(respRaw, branch.ConnectionURL)
	}
	return jsonContent(respRaw)
}

type projectArgs struct {
	Project string `json:"project"`
}

func (s *Server) toolListBranches(ctx context.Context, raw json.RawMessage) any {
	var a projectArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.Project == "" {
		return toolError("project is required")
	}
	respRaw, _, err := s.Client.ListBranches(ctx, a.Project)
	if err != nil {
		return apiToolError(err)
	}
	return jsonContent(respRaw)
}

type projectAndNameArgs struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

func (s *Server) toolGetBranch(ctx context.Context, raw json.RawMessage) any {
	var a projectAndNameArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.Project == "" || a.Name == "" {
		return toolError("project and name are required")
	}
	respRaw, _, err := s.Client.GetBranch(ctx, a.Project, a.Name)
	if err != nil {
		return apiToolError(err)
	}
	return jsonContent(respRaw)
}

func (s *Server) toolDeleteBranch(ctx context.Context, raw json.RawMessage) any {
	var a projectAndNameArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.Project == "" || a.Name == "" {
		return toolError("project and name are required")
	}
	respRaw, _, err := s.Client.DeleteBranch(ctx, a.Project, a.Name)
	if err != nil {
		return apiToolError(err)
	}
	return jsonContent(respRaw)
}
