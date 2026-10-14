package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Template mirrors one CI template: a project's schema (no application rows)
// plus optional seed SQL, sealed once and cloned per branch. Like Branch,
// only used for human-readable output; --json passes raw bytes through.
type Template struct {
	Name              string `json:"name"`
	Mode              string `json:"mode"`
	Status            string `json:"status"` // building | ready | failed | stale
	Version           int    `json:"version"`
	SeedSHA256        string `json:"seed_sha256"`
	SchemaFingerprint string `json:"schema_fingerprint"`
	SizeBytes         int64  `json:"size_bytes"`
	BuildMs           int64  `json:"build_ms"`
	Error             string `json:"error"`
	BuiltAt           string `json:"built_at"`
}

// Template statuses. building moves on its own; ready and failed are where a
// wait stops; stale means sealed but built from an older schema or seed.
const (
	TemplateBuilding = "building"
	TemplateReady    = "ready"
	TemplateFailed   = "failed"
	TemplateStale    = "stale"
)

// SchemaOnlyTemplate is the template `--schema-only` uses: schema only, no
// seed — defined on demand, so a project needs no setup to get one.
const SchemaOnlyTemplate = "schema-only"

// PutTemplateRequest is the PUT body: defining a template is idempotent, and
// the server rebuilds only when the definition or the source schema changed.
type PutTemplateRequest struct {
	SchemaOnly bool   `json:"schema_only"`
	SeedSQL    string `json:"seed_sql,omitempty"`
}

type templateList struct {
	Templates []Template `json:"templates"`
}

// ListTemplates issues GET .../templates.
func (c *Client) ListTemplates(ctx context.Context, project string) (raw []byte, templates []Template, err error) {
	if err := validatePathSegment("project", project); err != nil {
		return nil, nil, err
	}
	raw, err = c.do(ctx, http.MethodGet, templatesPath(project), nil, http.StatusOK)
	if err != nil {
		return raw, nil, err
	}
	var list templateList
	if err := json.Unmarshal(raw, &list); err != nil {
		return raw, nil, fmt.Errorf("decode response: %w", err)
	}
	return raw, list.Templates, nil
}

// GetTemplate issues GET .../templates/<name>.
func (c *Client) GetTemplate(ctx context.Context, project, name string) ([]byte, Template, error) {
	return c.templateCall(ctx, http.MethodGet, project, name, "", nil, http.StatusOK)
}

// PutTemplate issues PUT .../templates/<name>: 200 when the template is
// already ready and fresh, 202 when a build was enqueued.
func (c *Client) PutTemplate(ctx context.Context, project, name string, req PutTemplateRequest) ([]byte, Template, error) {
	return c.templateCall(ctx, http.MethodPut, project, name, "", req, http.StatusOK, http.StatusAccepted)
}

// BuildTemplate issues POST .../templates/<name>/build (a forced rebuild).
func (c *Client) BuildTemplate(ctx context.Context, project, name string) ([]byte, Template, error) {
	return c.templateCall(ctx, http.MethodPost, project, name, "/build", nil, http.StatusAccepted)
}

// DeleteTemplate issues DELETE .../templates/<name>. Branches already
// minted from it are independent copies and keep working.
func (c *Client) DeleteTemplate(ctx context.Context, project, name string) (raw []byte, status string, err error) {
	if err := validatePathSegment("project", project); err != nil {
		return nil, "", err
	}
	if err := validatePathSegment("name", name); err != nil {
		return nil, "", err
	}
	raw, err = c.do(ctx, http.MethodDelete, templatePath(project, name), nil, http.StatusAccepted)
	if err != nil {
		return raw, "", err
	}
	var resp deleteResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return raw, "", fmt.Errorf("decode response: %w", err)
	}
	return raw, resp.Status, nil
}

func (c *Client) templateCall(ctx context.Context, method, project, name, suffix string, body any, want ...int) ([]byte, Template, error) {
	if err := validatePathSegment("project", project); err != nil {
		return nil, Template{}, err
	}
	if err := validatePathSegment("name", name); err != nil {
		return nil, Template{}, err
	}
	status, raw, err := c.send(ctx, method, templatePath(project, name)+suffix, body)
	if err != nil {
		return nil, Template{}, err
	}
	ok := false
	for _, w := range want {
		ok = ok || status == w
	}
	if !ok {
		return raw, Template{}, newAPIError(status, raw)
	}
	var t Template
	if err := json.Unmarshal(raw, &t); err != nil {
		return raw, Template{}, fmt.Errorf("decode response: %w", err)
	}
	return raw, t, nil
}

// WaitForTemplate polls until the template is ready or failed, ctx is done,
// or a request errors. A stale template is rebuilt (the server would refuse
// to mint from it), then waited for like any other build.
func (c *Client) WaitForTemplate(ctx context.Context, project string, raw []byte, t Template) ([]byte, Template, error) {
	var err error
	for {
		switch t.Status {
		case TemplateReady, TemplateFailed:
			return raw, t, nil
		case TemplateStale:
			if raw, t, err = c.BuildTemplate(ctx, project, t.Name); err != nil {
				return raw, t, err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return raw, t, ctx.Err()
		case <-time.After(PollInterval):
		}
		if raw, t, err = c.GetTemplate(ctx, project, t.Name); err != nil {
			return raw, t, err
		}
	}
}

func templatesPath(project string) string {
	return fmt.Sprintf("/api/v1/projects/%s/templates", url.PathEscape(project))
}

func templatePath(project, name string) string {
	return fmt.Sprintf("/api/v1/projects/%s/templates/%s", url.PathEscape(project), url.PathEscape(name))
}
