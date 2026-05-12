// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package response

import (
	"fmt"
	"net/http"
)

// BlockingResourcesError signals that a delete cannot proceed because
// the target is still referenced by one or more dependent resources.
// It is the canonical "resource in use" signal across the
// infrastructure-resource handlers (networks, storage pools,
// firmwares); the keys of Resources name the dependent resource type
// (matching the JSON key used in `details.blocking_resources`) and
// the values carry the live reference count.
//
// The Nodes handler does NOT use this type — Nodes carry an additional
// `force_available: true` flag and a richer cancel-then-orphan flow
// that warrants its own typed error. Networks / storage pools /
// firmwares have no force-delete counterpart by design (deletion is
// always conditional on the operator removing the dependent resources
// first), so the simpler shape suffices.
type BlockingResourcesError struct {
	// Code overrides the wire `error.code`. Zero value falls back to
	// CodeConflict so existing callers (networks, firmwares, the
	// vm-only branch of templates / storage_pools) keep emitting the
	// generic `conflict` code unchanged. Endpoint-specific overrides
	// (e.g. `resource_in_use` for the storage_images refusal) set
	// this field explicitly.
	Code ErrorCode

	// Kind, when non-empty, surfaces the blocked resource's category
	// in `details.kind` (e.g. "template", "pool"). Paired with
	// CodeResourceInUse so callers can disambiguate a single error
	// code across resource families without parsing the message.
	Kind string

	// Message is the human-readable summary written into
	// `error.message`. Subject + actionable hint, e.g. "network is in
	// use by virtual machine NICs; remove or migrate them first".
	Message string

	// Resources maps dependent-resource-type → reference count. The
	// JSON keys are stable (clients use them for programmatic
	// dispatch) and snake_case to match `Network.id` etc.
	Resources map[string]int64
}

// Error implements the error interface; the rendered string is for
// logs only — wire surface goes through WriteBlockingResources.
func (e *BlockingResourcesError) Error() string {
	return fmt.Sprintf("resource in use: %v", e.Resources)
}

// WriteBlockingResources renders e as a 409 conflict envelope with
// `details.blocking_resources` carrying the per-type counts. The
// helper is the standard 409 path for infrastructure-resource DELETE
// handlers.
func WriteBlockingResources(w http.ResponseWriter, r *http.Request, e *BlockingResourcesError) {
	code := e.Code
	if code == "" {
		code = CodeConflict
	}
	details := map[string]any{"blocking_resources": e.Resources}
	if e.Kind != "" {
		details["kind"] = e.Kind
	}
	WriteError(w, r, http.StatusConflict, code, e.Message, details)
}
