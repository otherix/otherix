// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

// Template mirrors the response shape internal/api/handlers/templates.toView
// produces (server-side `templateView` in templates/handler.go). The
// image checksum surfaces as lowercase hex; metadata is a pass-through
// json.RawMessage so the JSONB document round-trips without
// re-marshalling.
//
// owner_id stays a UUID string here — name-based references narrow
// VM-referenced resources к names, but users are intentionally
// outside that scope. Template name itself is surfaced as Name;
// operators address templates by name through every CLI path.
type Template struct {
	ID           uuid.UUID `json:"id"`
	OwnerID      uuid.UUID `json:"owner_id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Visibility   string    `json:"visibility"`
	Architecture string    `json:"architecture"`
	OSFamily     string    `json:"os_family"`
	OSVariant    string    `json:"os_variant"`
	ImageURL     string    `json:"image_url"`
	// Nullable — null indicates the template was registered в
	// compute mode (no upstream checksum) и has not yet been
	// materialised. After the first successful import the CP
	// back-propagates the agent-computed value, so this field holds
	// а non-nil pointer in steady state.
	ImageChecksumSHA256 *string `json:"image_checksum_sha256"`
	ImageFormat         string  `json:"image_format"`
	ImageSizeBytes      *int64  `json:"image_size_bytes"`
	FirmwareType        string  `json:"firmware_type"`
	FirmwareID          *string `json:"firmware_id"`
	DefaultCPUCores     int32   `json:"default_cpu_cores"`
	DefaultMemoryMiB    int32   `json:"default_memory_mib"`
	DefaultDiskGiB      int32   `json:"default_disk_gib"`
	CloudInitSupported  bool    `json:"cloud_init_supported"`
	// CloudInitUserData carries the optional `#cloud-config` YAML the
	// operator baked into the template at create/update time (L3 /
	// operator UX iterations). Server returns nil when unset. Earlier
	// drift hid this column from the CLI even though the server и DB
	// persisted it correctly — operators using `otherix template get`
	// could not verify what cloud-init was actually baked.
	CloudInitUserData      *string         `json:"cloud_init_user_data"`
	QemuGuestAgentExpected bool            `json:"qemu_guest_agent_expected"`
	Metadata               json.RawMessage `json:"metadata"`
	CreatedAt              string          `json:"created_at"`
	UpdatedAt              string          `json:"updated_at"`
}

// TemplateList is the cursor-paginated envelope of GET /v1/templates.
type TemplateList struct {
	Data []Template `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// ListTemplatesParams collects the query knobs the list endpoint
// accepts. Owner is intentionally absent — the CLI exposes only the
// operator-friendly filters (visibility, architecture, os_family).
// Limit / Cursor drive cursor pagination.
type ListTemplatesParams struct {
	Limit        int
	Cursor       string
	Visibility   string
	Architecture string
	OSFamily     string
}

// ListTemplates fetches GET /v1/templates с the supplied query
// filters. Pagination is the caller's responsibility (re-issue с
// TemplateList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListTemplates(ctx context.Context, params ListTemplatesParams) (TemplateList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Visibility != "" {
		q.Set("visibility", params.Visibility)
	}
	if params.Architecture != "" {
		q.Set("architecture", params.Architecture)
	}
	if params.OSFamily != "" {
		q.Set("os_family", params.OSFamily)
	}

	path := "/v1/templates"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return TemplateList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return TemplateList{}, err
	}
	var out TemplateList
	if err := decodeJSON(body, &out); err != nil {
		return TemplateList{}, err
	}
	return out, nil
}

// GetTemplate fetches GET /v1/templates/{identifier}. The server
// resolves either a UUID literal or a template name; the CLI forwards
// the raw string verbatim. Cross-user private templates surface as
// 404 *APIError (no existence leak).
func (c *Client) GetTemplate(ctx context.Context, identifier string) (Template, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/templates/"+url.PathEscape(identifier), nil)
	if err != nil {
		return Template{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Template{}, err
	}
	var out Template
	if err := decodeJSON(body, &out); err != nil {
		return Template{}, err
	}
	return out, nil
}

// CreateTemplateRequest mirrors components/schemas/TemplateCreate в
// api/openapi/control-plane.yaml. Optional fields use pointer types so
// the encoder can omit them, letting the server apply spec defaults
// (e.g. image_format=qcow2, firmware_type=uefi, default_cpu_cores=2).
//
// `image_checksum_sha256` is optional. Two trust modes:
//
//   - Non-empty value (verify mode) — server stores the bytes и the
//     agent verifies the streamed download against them.
//   - Empty string (compute mode) — the server treats the field as
//     absent, agent computes the sha256 during streaming download и
//     CP back-propagates it onto the template row idempotently.
//
// The CLI flag layer normalises absent flags к empty string, so the
// wire body always carries the field — the server-side handler maps
// empty к "absent" before writing к the nullable bytea column.
//
// `visibility` is intentionally absent — POST /v1/templates rejects it
// via the forbidden-key sweep (templates always start `private`; flip
// later via the dedicated `templates.setVisibility` endpoint).
type CreateTemplateRequest struct {
	Name                string  `json:"name"`
	Description         *string `json:"description,omitempty"`
	Architecture        string  `json:"architecture"`
	OSFamily            string  `json:"os_family"`
	OSVariant           *string `json:"os_variant,omitempty"`
	ImageURL            string  `json:"image_url"`
	ImageChecksumSHA256 string  `json:"image_checksum_sha256"`
	ImageFormat         *string `json:"image_format,omitempty"`
	FirmwareType        *string `json:"firmware_type,omitempty"`
	DefaultCPUCores     *int32  `json:"default_cpu_cores,omitempty"`
	DefaultMemoryMiB    *int32  `json:"default_memory_mib,omitempty"`
	DefaultDiskGiB      *int32  `json:"default_disk_gib,omitempty"`
	CloudInitSupported  *bool   `json:"cloud_init_supported,omitempty"`
	// CloudInitUserData is the optional raw `#cloud-config` YAML the
	// operator wants baked into the template (L3 / operator UX
	// iteration). When set, the server stores it on
	// templates.cloud_init_user_data; on VM create без а per-VM
	// override the resolver falls back к this value. Pointer-typed
	// so omitting the flag leaves the JSON encoder skipping the
	// field — matching the spec's "omit = unchanged" PATCH semantics
	// и the create path's "omit = no pre-bake" default.
	CloudInitUserData      *string `json:"cloud_init_user_data,omitempty"`
	QemuGuestAgentExpected *bool   `json:"qemu_guest_agent_expected,omitempty"`
}

// ErrTemplateExists is the sentinel returned by CreateTemplate when the
// server responds 409 conflict (template name collision). The CLI uses
// `errors.Is(err, ErrTemplateExists)` to drive idempotent retry — a
// re-run that hits an already-registered template falls through to the
// materialisation step instead of failing. Detection is purely
// status-based (StatusCode == 409): the CP currently emits the generic
// `conflict` code on uq_templates_name violations, not a specialised
// `template_exists` code.
var ErrTemplateExists = errors.New("template already exists")

// CreateTemplate submits POST /v1/templates. 201 returns the parsed
// Template; 409 collapses к ErrTemplateExists; any other non-2xx
// surfaces as *APIError.
func (c *Client) CreateTemplate(ctx context.Context, req CreateTemplateRequest) (Template, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/templates", req)
	if err != nil {
		return Template{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return Template{}, ErrTemplateExists
		}
		return Template{}, err
	}
	var out Template
	if err := decodeJSON(body, &out); err != nil {
		return Template{}, err
	}
	return out, nil
}

// ErrTemplateBlocked is the typed error DeleteTemplate returns when the
// CP refuses the delete because the template still has dependents.
// Mirrors response.BlockingResourcesError on the wire side:
//
//   - Code is the server-set `error.code` — `resource_in_use` when
//     storage_images are present, generic `conflict` for the
//     VM-only branch.
//   - Resources maps dependent-resource-type → reference count. Known
//     keys: "storage_images", "vms".
type ErrTemplateBlocked struct {
	Code      string
	Resources map[string]int64
}

// Error renders the operator-facing message, listing resource types
// and counts sorted by key для stable output.
func (e *ErrTemplateBlocked) Error() string {
	return fmt.Sprintf("template delete blocked: %s: %v", e.Code, e.Resources)
}

// DeleteTemplate submits DELETE /v1/templates/{name}. 204 returns nil;
// 409 collapses к *ErrTemplateBlocked carrying the parsed blocking_resources
// map; any other non-2xx (404 / 5xx) surfaces as *APIError.
//
// The handler enforces name-only addressing; UUID literals return
// 400 validation_failed.
func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/templates/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		if blocked := decodeBlockingResources(apiErr); blocked != nil {
			return blocked
		}
	}
	return err
}

// decodeBlockingResources lifts the `details.blocking_resources` map
// off an APIError into the typed ErrTemplateBlocked shape. Returns nil
// when the details payload is absent or malformed — the caller falls
// back к the raw *APIError so the operator at least sees the status code.
func decodeBlockingResources(apiErr *APIError) *ErrTemplateBlocked {
	if apiErr.Details == nil {
		return nil
	}
	raw, ok := apiErr.Details["blocking_resources"]
	if !ok {
		return nil
	}
	mapAny, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	resources := make(map[string]int64, len(mapAny))
	for k, v := range mapAny {
		switch n := v.(type) {
		case float64:
			resources[k] = int64(n)
		case int64:
			resources[k] = n
		}
	}
	if len(resources) == 0 {
		return nil
	}
	return &ErrTemplateBlocked{
		Code:      apiErr.Code,
		Resources: resources,
	}
}

// ImportTemplateImageRequest mirrors
// components/schemas/StorageImageImportRequest. The single `pool` field
// accepts either a UUID literal or a pool name — multi-instance pools
// keep addressing polymorphic so operators can target a specific
// per-node instance when multiple instances share a name.
type ImportTemplateImageRequest struct {
	Pool string `json:"pool"`
}

// ImportTemplateImage submits POST /v1/templates/{name}/images. 202
// returns the parsed TaskAccepted (clients poll /v1/tasks/{id}); any
// non-2xx surfaces as *APIError.
//
// `templateName` MUST be the template's name — the handler enforces
// name-only addressing (UUID literals return 400 validation_failed
// via the resolver layer).
func (c *Client) ImportTemplateImage(ctx context.Context, templateName string, req ImportTemplateImageRequest) (TaskAccepted, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/templates/"+url.PathEscape(templateName)+"/images", req)
	if err != nil {
		return TaskAccepted{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return TaskAccepted{}, err
	}
	var out TaskAccepted
	if err := decodeJSON(body, &out); err != nil {
		return TaskAccepted{}, err
	}
	return out, nil
}
