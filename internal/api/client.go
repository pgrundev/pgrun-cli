// Package api is a minimal client for the PGRun branches API
// (/api/v1/projects/:project/branches). Stdlib only.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds a single HTTP round trip. Overall wait/poll timeouts
// are the caller's responsibility via ctx.
const requestTimeout = 15 * time.Second

// Client talks to one PGRun API base URL with one bearer token.
type Client struct {
	BaseURL string
	Token   string

	httpClient *http.Client // nil uses the package default
}

// New builds a Client. baseURL and token are used as given — trimming and
// validation happen at call sites (config), not here.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token}
}

func (c *Client) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return &http.Client{Timeout: requestTimeout}
}

// APIError is a non-2xx response the server explained with {"error": "..."}.
// Message never contains the request token — it is built solely from the
// response body.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (status %d)", e.Message, e.StatusCode)
}

// AuthError is the 401 case: the configured token is missing or rejected.
// Kept distinct so the CLI can map it to its own exit code and hint.
type AuthError struct {
	*APIError
}

func (e *AuthError) Unwrap() error { return e.APIError }

// Branch mirrors the JSON object the API returns for one branch. Fields the
// server omits simply decode to their zero value — this struct is only used
// for human-readable formatting; --json output always uses the raw bytes.
type Branch struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	IsBase          bool   `json:"is_base"`
	ParentBranchID  string `json:"parent_branch_id"` // e.g. "branch_3"; null decodes to ""
	PostgresVersion string `json:"postgres_version"`
	CreatedAt       string `json:"created_at"`
	ExpiresAt       string `json:"expires_at"`
	ConnectionURL   string `json:"connection_url"`
}

// Terminal statuses for the create --wait poll loop.
const (
	StatusReady  = "ready"
	StatusFailed = "failed"
)

// CreateBranchRequest is the POST body. TTL and ParentBranchID are omitted
// when empty so the server applies its own defaults.
type CreateBranchRequest struct {
	Name           string `json:"name"`
	TTL            string `json:"ttl,omitempty"`
	ParentBranchID string `json:"parent_branch_id,omitempty"`
}

// branchList is the GET .../branches envelope.
type branchList struct {
	Branches []Branch `json:"branches"`
}

// deleteResponse is the 202 DELETE body.
type deleteResponse struct {
	Status string `json:"status"`
}

// CreateBranch issues POST .../branches. raw is the response body regardless
// of success (useful for --json passthrough even on error); branch is only
// valid when err is nil.
func (c *Client) CreateBranch(ctx context.Context, project string, req CreateBranchRequest) (raw []byte, branch Branch, err error) {
	raw, err = c.do(ctx, http.MethodPost, branchesPath(project), req, http.StatusCreated)
	if err != nil {
		return raw, Branch{}, err
	}
	if err := json.Unmarshal(raw, &branch); err != nil {
		return raw, Branch{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, branch, nil
}

// ListBranches issues GET .../branches.
func (c *Client) ListBranches(ctx context.Context, project string) (raw []byte, branches []Branch, err error) {
	raw, err = c.do(ctx, http.MethodGet, branchesPath(project), nil, http.StatusOK)
	if err != nil {
		return raw, nil, err
	}
	var list branchList
	if err := json.Unmarshal(raw, &list); err != nil {
		return raw, nil, fmt.Errorf("decode response: %w", err)
	}
	return raw, list.Branches, nil
}

// GetBranch issues GET .../branches/<name>. connection_url is present only
// when the branch is ready and credentialed — the server decides, not us.
func (c *Client) GetBranch(ctx context.Context, project, name string) (raw []byte, branch Branch, err error) {
	raw, err = c.do(ctx, http.MethodGet, branchPath(project, name), nil, http.StatusOK)
	if err != nil {
		return raw, Branch{}, err
	}
	if err := json.Unmarshal(raw, &branch); err != nil {
		return raw, Branch{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, branch, nil
}

// DeleteBranch issues DELETE .../branches/<name>. Deleting a base branch
// (one with children) yields a 409 APIError.
func (c *Client) DeleteBranch(ctx context.Context, project, name string) (raw []byte, status string, err error) {
	raw, err = c.do(ctx, http.MethodDelete, branchPath(project, name), nil, http.StatusAccepted)
	if err != nil {
		return raw, "", err
	}
	var resp deleteResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return raw, "", fmt.Errorf("decode response: %w", err)
	}
	return raw, resp.Status, nil
}

func branchesPath(project string) string {
	return fmt.Sprintf("/api/v1/projects/%s/branches", url.PathEscape(project))
}

func branchPath(project, name string) string {
	return fmt.Sprintf("/api/v1/projects/%s/branches/%s", url.PathEscape(project), url.PathEscape(name))
}

// do performs one request and returns the raw response body. On any status
// other than want it returns a typed error (AuthError for 401, APIError
// otherwise) alongside the raw body, so callers can still surface it via
// --json. raw is nil only when no response was ever received.
func (c *Client) do(ctx context.Context, method, path string, body any, want int) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, joinURL(c.BaseURL, path), reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == want {
		return raw, nil
	}
	return raw, newAPIError(resp.StatusCode, raw)
}

// newAPIError decodes {"error": "..."} from an error body, falling back to
// the raw body text and finally the HTTP status text.
func newAPIError(status int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		msg = body.Error
	} else if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
		msg = trimmed
	} else {
		msg = http.StatusText(status)
	}

	apiErr := &APIError{StatusCode: status, Message: msg}
	if status == http.StatusUnauthorized {
		return &AuthError{APIError: apiErr}
	}
	return apiErr
}

// joinURL concatenates a base and a path with exactly one slash between them.
func joinURL(base, p string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(p, "/")
}
