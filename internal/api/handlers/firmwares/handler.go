// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package firmwares hosts the /v1/firmwares/* HTTP handlers and the
// read-only GET /v1/nodes/{id}/firmwares projection. CRUD is gated by
// `firmware:read` (every role) and `firmware:manage` (admin only) per
// docs/rbac.md. Firmwares are cluster-wide metadata records with no
// owner column — the projection returned to every caller is identical.
//
// PATCH bodies reject the API-immutable identity fields
// (`architecture`, `type`) plus the system-managed timestamps with
// 400 forbidden_fields; mutable fields are name, version, secure_boot,
// is_default.
//
// Default-firmware conflict policy: at most one firmware per
// (architecture, type) pair may carry `is_default = true` (enforced
// both in code via a precheck and in the schema via the partial
// unique index uq_firmwares_default). The Create / Update handlers
// REJECT a second default with a 409 conflict carrying
// `details.code = "default_already_set"` and
// `details.existing_firmware_id` — operators must explicitly clear
// the previous default before promoting another firmware. Mirrors
// storage_pools.
//
// Delete is conditional on no active vms or templates referencing the
// firmware. Even though templates.firmware_id is ON DELETE SET NULL
// at the schema level, the CP-level policy is to require operators to
// explicitly clear the pin first; the alternative would silently null
// out template firmware references on delete. Firmwares have no
// force-delete counterpart by design.
//
// Per-node firmware availability (GET /v1/nodes/{id}/firmwares) is a
// read-only projection of node_firmwares populated by the future
// agent heartbeat receiver. Until that receiver lands the table is
// empty and the endpoint returns an empty list — clients can wire
// against the contract today and the response shape "fills in" once
// agents start reporting.
package firmwares

import (
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/store"
)

// Handler bundles the dependencies for the firmwares routes.
type Handler struct {
	store *store.Store
	log   *slog.Logger
}

// New constructs a Handler.
func New(s *store.Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// firmwareView mirrors components/schemas/Firmware in
// api/openapi/control-plane.yaml.
type firmwareView struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Architecture string  `json:"architecture"`
	Type         string  `json:"type"`
	Version      *string `json:"version"`
	SecureBoot   bool    `json:"secure_boot"`
	IsDefault    bool    `json:"is_default"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// toView projects a store.Firmware onto its public firmwareView.
func toView(f store.Firmware) firmwareView {
	return firmwareView{
		ID:           f.ID.String(),
		Name:         f.Name,
		Architecture: string(f.Architecture),
		Type:         string(f.Type),
		Version:      f.Version,
		SecureBoot:   f.SecureBoot,
		IsDefault:    f.IsDefault,
		CreatedAt:    f.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    f.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// nodeFirmwareView mirrors components/schemas/NodeFirmware. The
// nested firmware projection reuses firmwareView verbatim.
type nodeFirmwareView struct {
	Firmware   firmwareView `json:"firmware"`
	CodePath   string       `json:"code_path"`
	VarsPath   *string      `json:"vars_path"`
	Available  bool         `json:"available"`
	ReportedAt string       `json:"reported_at"`
}

// toNodeFirmwareView projects a (NodeFirmware, Firmware) pair into the
// public response shape.
func toNodeFirmwareView(nf store.NodeFirmware, f store.Firmware) nodeFirmwareView {
	return nodeFirmwareView{
		Firmware:   toView(f),
		CodePath:   nf.CodePath,
		VarsPath:   nf.VarsPath,
		Available:  nf.Available,
		ReportedAt: nf.ReportedAt.UTC().Format(time.RFC3339Nano),
	}
}
