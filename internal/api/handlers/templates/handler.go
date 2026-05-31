// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package templates hosts the /v1/templates/* HTTP handlers — five
// CRUD endpoints plus the dedicated `setVisibility` and `clone`
// sub-resources. Templates are an ownable resource:
// every row carries `owner_id`, and `auth.CheckOwnership` enforces
// the own-vs-any cut on PATCH / DELETE / private-template Get.
//
// Two parallel access axes interact:
//
//   - Visibility — `private` (default; only owner + admin/operator
//     see it) vs `public` (every authenticated user sees it). The
//     `template:read:public` permission gates the route; the
//     `template:read` permission (own scope for developer, any for
//     admin/operator) is consulted by the handler when the row is
//     `private` and the caller is not its owner. Cross-user private
//     access collapses to 404 (never 403) so existence cannot be
//     probed via differing status codes.
//   - Ownership — `owner_id` is server-assigned to the calling user
//     on Create / Clone and is immutable thereafter. `setVisibility`
//     never moves ownership: publishing a private template to public
//     does not transfer authorship to admin/operator.
//
// Templates are always created with `visibility = 'private'` (Variant
// C — see CLAUDE.md "Authorization (RBAC)"). The Create / Clone
// handlers reject `visibility` in the request body via
// `forbidden_fields`; the SQL INSERTs also hard-code the literal so a
// future call site cannot opt out by mistake. The only path to
// `public` is the dedicated `templates.setVisibility` endpoint, gated
// by the admin/operator-only `template:set_visibility` permission.
//
// PATCH bodies surface the 11 mutable fields documented in
// `TemplateUpdate` (api/openapi/control-plane.yaml). The image-source
// bundle (`image_url`, `image_checksum_sha256`, `image_format`,
// `image_size_bytes`) and identity columns (`architecture`,
// `os_family`, `owner_id`) are immutable — `forbidden_fields` rejects
// them. Use `templates.clone` followed by a PATCH to evolve.
//
// DELETE is conditional on no active VM rows referencing the template
// — surfaced as a 409 carrying `details.blocking_resources: {"vms":
// N}` via `response.BlockingResourcesError`. The block lives entirely
// in the API layer because three FKs reference templates with
// different ON DELETE semantics (vms: SET NULL, vm_disks: RESTRICT,
// node_image_cache: CASCADE) — soft-delete bypasses all of them, so
// this counter is the sole enforcement of "don't remove a template
// that's still in use".
package templates

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the templates handlers depend on: the
// template domain methods, the identifier-resolution contract
// (resolver.Querier) used to resolve template / pool path parameters,
// the node read used by the storage-image import precheck, and the
// backend-agnostic EnqueueTask producer seam. *store.Store satisfies
// it; depending on the interface rather than the concrete store is the
// Phase 2 seam that lets a second backend (Phase 3) be substituted
// under the same handler tests, and keeps river off the handler.
type Store interface {
	resolver.Querier

	CreateTemplate(ctx context.Context, arg store.CreateTemplateParams) (store.Template, error)
	UpdateTemplate(ctx context.Context, arg store.UpdateTemplateParams) (store.Template, error)
	CloneTemplate(ctx context.Context, arg store.CloneTemplateParams) (store.Template, error)
	SetTemplateVisibility(ctx context.Context, arg store.SetTemplateVisibilityParams) (store.Template, error)
	ListTemplates(ctx context.Context, arg store.ListTemplatesParams) ([]store.Template, error)
	DeleteTemplate(ctx context.Context, id uuid.UUID) error
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the templates routes. The
// storage_image.import sub-resource enqueues an async task through the
// store's backend-agnostic EnqueueTask seam, so the handler no longer
// holds a river client.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// templateView mirrors components/schemas/Template in
// api/openapi/control-plane.yaml. The image checksum is surfaced as
// lowercase hex (the storage column is `bytea`); `metadata` is a
// pass-through `json.RawMessage` so the JSONB document round-trips
// without re-marshalling.
type templateView struct {
	ID           string `json:"id"`
	OwnerID      string `json:"owner_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Visibility   string `json:"visibility"`
	Architecture string `json:"architecture"`
	OSFamily     string `json:"os_family"`
	OSVariant    string `json:"os_variant"`
	ImageURL     string `json:"image_url"`
	// Nullable - null indicates the template has not yet been
	// materialised in compute mode (back-propagation fills the column
	// on first successful import).
	ImageChecksumSHA256    *string         `json:"image_checksum_sha256"`
	ImageFormat            string          `json:"image_format"`
	ImageSizeBytes         *int64          `json:"image_size_bytes"`
	FirmwareType           string          `json:"firmware_type"`
	FirmwareID             *string         `json:"firmware_id"`
	DefaultCPUCores        int32           `json:"default_cpu_cores"`
	DefaultMemoryMiB       int32           `json:"default_memory_mib"`
	DefaultDiskGiB         int32           `json:"default_disk_gib"`
	CloudInitSupported     bool            `json:"cloud_init_supported"`
	CloudInitUserData      *string         `json:"cloud_init_user_data"`
	QemuGuestAgentExpected bool            `json:"qemu_guest_agent_expected"`
	Metadata               json.RawMessage `json:"metadata"`
	CreatedAt              string          `json:"created_at"`
	UpdatedAt              string          `json:"updated_at"`
}

// toView projects a store.Template onto its public templateView.
func toView(t store.Template) templateView {
	v := templateView{
		ID:                     t.ID.String(),
		OwnerID:                t.OwnerID.String(),
		Name:                   t.Name,
		Description:            t.Description,
		Visibility:             t.Visibility,
		Architecture:           string(t.Architecture),
		OSFamily:               string(t.OsFamily),
		OSVariant:              t.OsVariant,
		ImageURL:               t.ImageUrl,
		ImageChecksumSHA256:    optionalHexChecksum(t.ImageChecksumSha256),
		ImageFormat:            string(t.ImageFormat),
		ImageSizeBytes:         t.ImageSizeBytes,
		FirmwareType:           string(t.FirmwareType),
		DefaultCPUCores:        t.DefaultCpuCores,
		DefaultMemoryMiB:       t.DefaultMemoryMib,
		DefaultDiskGiB:         t.DefaultDiskGib,
		CloudInitSupported:     t.CloudInitSupported,
		CloudInitUserData:      t.CloudInitUserData,
		QemuGuestAgentExpected: t.QemuGuestAgentExpected,
		Metadata:               normaliseMetadataOut(t.Metadata),
		CreatedAt:              t.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:              t.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.FirmwareID != nil {
		s := t.FirmwareID.String()
		v.FirmwareID = &s
	}
	return v
}

// optionalHexChecksum lifts a possibly-null bytea checksum onto the
// wire shape: nil bytes (NULL column, compute-mode template not yet
// materialised) → nil *string (JSON null); non-nil bytes
// → pointer-to-lowercase-hex. The hex encoder produces the canonical
// 64-char form so the wire output matches the validator's pattern.
func optionalHexChecksum(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := hex.EncodeToString(b)
	return &s
}

// normaliseMetadataOut guards against an empty bytea slipping through
// as `null` on the wire. The schema column has `DEFAULT '{}'::jsonb
// NOT NULL`, so this is a belt-and-braces step.
func normaliseMetadataOut(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

// readBody reads the request body in full so the handler can both
// inspect raw JSON keys (for forbidden-field sweeps in Create / PATCH
// / Clone) and decode the typed shape. ok=false signals the response
// has already been written.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "could not read body", nil)
		return nil, false
	}
	return body, true
}

// scanAllowedKeys returns the keys present in the top-level JSON
// object of body that are NOT in allowed. Used by Clone, where the
// allowlist is the concise expression of what the body may carry.
func scanAllowedKeys(body []byte, allowed []string) []string {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var seen map[string]json.RawMessage
	if err := json.Unmarshal(body, &seen); err != nil || seen == nil {
		return nil
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		allowSet[k] = struct{}{}
	}
	var out []string
	for k := range seen {
		if _, ok := allowSet[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// decodeNullableString unwraps a tri-state RawMessage into an
// optional string. The literal `null` returns (nil, nil); a JSON
// string returns (&s, nil); anything else surfaces an error so the
// handler can issue a 400.
func decodeNullableString(raw json.RawMessage) (*string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// writeValidation is a thin shorthand around the validation_failed
// envelope.
func writeValidation(w http.ResponseWriter, r *http.Request, msg string) {
	response.WriteError(w, r, http.StatusBadRequest,
		response.CodeValidationFailed, msg, nil)
}

// writeForbiddenFields renders the canonical 400 envelope for a
// request body that contains keys the endpoint does not accept.
func writeForbiddenFields(w http.ResponseWriter, r *http.Request, keys []string) {
	response.WriteError(w, r, http.StatusBadRequest,
		response.CodeValidationFailed,
		"one or more fields cannot be modified",
		map[string]any{"forbidden_fields": keys})
}

// writeNotFound renders the canonical 404 envelope for both "row
// missing" and "exists but invisible to caller". The latter is the
// no-leak path used by Get/Update/Delete/Clone-source on private
// templates owned by another user.
func writeNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeNotFound, "template not found", nil)
}

// writeNeutralNameConflict renders the 409 envelope for a name
// collision (uq_templates_name). The message is intentionally neutral
// — name uniqueness is global, not per-owner, so revealing whose
// template owns the name would be a minor info leak.
func writeNeutralNameConflict(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusConflict,
		response.CodeConflict,
		"template with this name already exists",
		map[string]any{"field": "name"})
}
