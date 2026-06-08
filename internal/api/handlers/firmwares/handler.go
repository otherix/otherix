// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package firmwares hosts the /v1/firmwares/* HTTP handlers. CRUD is gated by
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
// Delete is conditional on no active vms referencing the firmware: the
// operator must remove or repoint the dependent VMs first. Firmwares have
// no force-delete counterpart by design.
package firmwares

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the firmwares handlers depend on: the
// firmware domain methods. *etcdstore.Store satisfies it; depending on the
// interface rather than the concrete store narrows the handler's storage
// dependency to the methods it uses and lets tests substitute a fake.
type Store interface {
	FirmwareByID(ctx context.Context, id uuid.UUID) (store.Firmware, error)
	DefaultFirmwareForArchType(ctx context.Context, arch store.CPUArch, ftype store.FirmwareType) (store.Firmware, error)
	CreateFirmware(ctx context.Context, arg store.CreateFirmwareParams) (store.Firmware, error)
	UpdateFirmware(ctx context.Context, arg store.UpdateFirmwareParams) (store.Firmware, error)
	ListFirmwares(ctx context.Context, arg store.ListFirmwaresParams) ([]store.Firmware, error)
	DeleteFirmware(ctx context.Context, id uuid.UUID) error
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the firmwares routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
func New(s Store, log *slog.Logger) *Handler {
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
