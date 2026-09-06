// This file adds the PGRun sources API (/api/v1/sources) to the Client
// defined in client.go: Connect Postgres → Protect Data → Create Safe Copy,
// the backend for `pgrun source ...`. Same conventions as the branches API
// above — (raw []byte, typed, err error) method shape, c.do for the HTTP
// round trip, validatePathSegment guarding every path segment before a
// request is ever built. The one rule specific to this file: a source's
// connection URL travels ONLY in a JSON request body under the key
// "connection_url" — never a query string, never a path segment, never a
// header — and no error message built here may contain it (messages come
// solely from the response body / status text, exactly like client.go).

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Source mirrors one source object (see the API contract). Only used for
// human formatting; --json always prints the raw bytes.
type Source struct {
	Name              string         `json:"name"`
	SafeCopyStatus    string         `json:"safe_copy_status"`
	Step              int            `json:"step"`
	PostgresVersion   string         `json:"postgres_version"`
	Provider          string         `json:"provider"`
	SizeBytes         int64          `json:"size_bytes"`
	Tables            int            `json:"tables"`
	SensitiveColumns  int            `json:"sensitive_columns"`
	UnresolvedCount   int            `json:"unresolved_count"`
	Protection        string         `json:"protection"`
	PolicyVersion     int            `json:"policy_version"` // null decodes to 0
	Sync              string         `json:"sync"`
	SafeCopyUpdatedAt string         `json:"safe_copy_updated_at"`
	Branches          int            `json:"branches"`
	LastCheckError    string         `json:"last_check_error"` // exception class name, "" when null
	SchemaChanged     bool           `json:"schema_changed"`
	SchemaChanges     *SchemaChanges `json:"schema_changes"`
}

type SchemaChanges struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Retyped []string `json:"retyped"`
}

// Safe Copy statuses (safe_copy_status), verbatim from the API.
const (
	SourceConnect        = "connect"
	SourceChecking       = "checking"
	SourceProtect        = "protect"
	SourceReadyToCopy    = "ready_to_copy"
	SourceCopying        = "copying"
	SourceReady          = "ready"
	SourceActionRequired = "action_required"
	SourceFailed         = "failed"
)

// CheckFailed: the last connection check failed (the API keeps such a source
// in "checking" so a new URL can be supplied; last_check_error carries the
// exception class). CheckSettled: the check has produced an answer either
// way. CopySettled: a Safe Copy creation is no longer in progress.
func CheckFailed(s Source) bool  { return s.SafeCopyStatus == SourceChecking && s.LastCheckError != "" }
func CheckSettled(s Source) bool { return s.SafeCopyStatus != SourceChecking || s.LastCheckError != "" }
func CopySettled(s Source) bool  { return s.SafeCopyStatus != SourceCopying }

type ProtectRequest struct {
	Decisions   map[string]string `json:"decisions,omitempty"`
	Acknowledge []string          `json:"acknowledge,omitempty"`
	Approve     bool              `json:"approve,omitempty"`
}

type UnresolvedColumn struct {
	Column    string   `json:"column"`
	Sensitive bool     `json:"sensitive"`
	Valid     []string `json:"valid"`
}

type ColumnRule struct {
	Column      string `json:"column"`
	Disposition string `json:"disposition"` // copy | fake | null
	Recommended bool   `json:"recommended"`
	Sensitive   bool   `json:"sensitive"`
}

type TableRule struct {
	Table       string `json:"table"`
	Disposition string `json:"disposition"` // copy_data | schema_only | exclude
}

type ProtectResult struct {
	Name            string             `json:"name"`
	SafeCopyStatus  string             `json:"safe_copy_status"`
	PolicyVersion   int                `json:"policy_version"`
	Activated       bool               `json:"activated"`
	UnresolvedCount int                `json:"unresolved_count"`
	Unresolved      []UnresolvedColumn `json:"unresolved"`
	Rules           []ColumnRule       `json:"rules"`
	TableRules      []TableRule        `json:"table_rules"`
}

type ReviewResult struct {
	Name       string   `json:"name"`
	Unresolved []string `json:"unresolved"`
	Requested  bool     `json:"requested"`
}

// createSourceRequest is the POST /api/v1/sources body.
type createSourceRequest struct {
	Name          string `json:"name"`
	ConnectionURL string `json:"connection_url"`
}

// updateSourceRequest is the PATCH /api/v1/sources/<name> body.
type updateSourceRequest struct {
	ConnectionURL string `json:"connection_url"`
}

// sourceList is the GET /api/v1/sources envelope.
type sourceList struct {
	Sources []Source `json:"sources"`
}

func sourcesPath() string {
	return "/api/v1/sources"
}

func sourcePath(name string) string {
	return "/api/v1/sources/" + url.PathEscape(name)
}

func sourceActionPath(name, action string) string {
	return sourcePath(name) + "/" + action
}

// ListSources issues GET /api/v1/sources.
func (c *Client) ListSources(ctx context.Context) (raw []byte, sources []Source, err error) {
	raw, err = c.do(ctx, http.MethodGet, sourcesPath(), nil, http.StatusOK)
	if err != nil {
		return raw, nil, err
	}
	var list sourceList
	if err := json.Unmarshal(raw, &list); err != nil {
		return raw, nil, fmt.Errorf("decode response: %w", err)
	}
	return raw, list.Sources, nil
}

// GetSource issues GET /api/v1/sources/<name>.
func (c *Client) GetSource(ctx context.Context, name string) (raw []byte, src Source, err error) {
	if err := validatePathSegment("name", name); err != nil {
		return nil, Source{}, err
	}
	raw, err = c.do(ctx, http.MethodGet, sourcePath(name), nil, http.StatusOK)
	if err != nil {
		return raw, Source{}, err
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return raw, Source{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, src, nil
}

// CreateSource issues POST /api/v1/sources. connectionURL travels only in
// the JSON request body (key "connection_url") — never in the URL.
func (c *Client) CreateSource(ctx context.Context, name, connectionURL string) (raw []byte, src Source, err error) {
	if err := validatePathSegment("name", name); err != nil {
		return nil, Source{}, err
	}
	req := createSourceRequest{Name: name, ConnectionURL: connectionURL}
	raw, err = c.do(ctx, http.MethodPost, sourcesPath(), req, http.StatusCreated)
	if err != nil {
		return raw, Source{}, err
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return raw, Source{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, src, nil
}

// UpdateSourceURL issues PATCH /api/v1/sources/<name>. connectionURL travels
// only in the JSON request body (key "connection_url") — never in the URL.
// Only a source in "connect" or "checking" accepts a new URL; the server
// returns 409 otherwise.
func (c *Client) UpdateSourceURL(ctx context.Context, name, connectionURL string) (raw []byte, src Source, err error) {
	if err := validatePathSegment("name", name); err != nil {
		return nil, Source{}, err
	}
	req := updateSourceRequest{ConnectionURL: connectionURL}
	raw, err = c.do(ctx, http.MethodPatch, sourcePath(name), req, http.StatusOK)
	if err != nil {
		return raw, Source{}, err
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return raw, Source{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, src, nil
}

// ProtectSource issues POST /api/v1/sources/<name>/protect. req is always
// sent as a JSON body, even when zero-valued (an empty decision set is a
// legitimate request — e.g. approving with only defaults).
func (c *Client) ProtectSource(ctx context.Context, name string, req ProtectRequest) (raw []byte, res ProtectResult, err error) {
	if err := validatePathSegment("name", name); err != nil {
		return nil, ProtectResult{}, err
	}
	raw, err = c.do(ctx, http.MethodPost, sourceActionPath(name, "protect"), req, http.StatusOK)
	if err != nil {
		return raw, ProtectResult{}, err
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return raw, ProtectResult{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, res, nil
}

// ReviewSource issues POST /api/v1/sources/<name>/review with no request
// body.
func (c *Client) ReviewSource(ctx context.Context, name string) (raw []byte, res ReviewResult, err error) {
	if err := validatePathSegment("name", name); err != nil {
		return nil, ReviewResult{}, err
	}
	raw, err = c.do(ctx, http.MethodPost, sourceActionPath(name, "review"), nil, http.StatusOK)
	if err != nil {
		return raw, ReviewResult{}, err
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return raw, ReviewResult{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, res, nil
}

// CopySource issues POST /api/v1/sources/<name>/copy with no request body.
func (c *Client) CopySource(ctx context.Context, name string) (raw []byte, src Source, err error) {
	if err := validatePathSegment("name", name); err != nil {
		return nil, Source{}, err
	}
	raw, err = c.do(ctx, http.MethodPost, sourceActionPath(name, "copy"), nil, http.StatusAccepted)
	if err != nil {
		return raw, Source{}, err
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return raw, Source{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, src, nil
}

// WaitForSource polls GetSource every PollInterval until done(src) is true,
// ctx is done, or a request errors — always returning the latest raw body,
// exactly like WaitForTerminal.
func (c *Client) WaitForSource(ctx context.Context, name string, raw []byte, src Source, done func(Source) bool) ([]byte, Source, error) {
	for {
		if done(src) {
			return raw, src, nil
		}
		select {
		case <-ctx.Done():
			return raw, src, ctx.Err()
		case <-time.After(PollInterval):
		}
		var err error
		raw, src, err = c.GetSource(ctx, name)
		if err != nil {
			return raw, src, err
		}
	}
}
