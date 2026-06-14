// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the migrations HTTP handlers depend on:
// the identifier-resolution contract (resolver.Querier, used to resolve
// the {id} path param to a VM and the target_node name to a node), the
// VM / node reads the ownership and target-resolution paths need, and
// the migration domain methods (atomic create-with-task, by-id, list,
// cancel). *etcdstore.Store satisfies it; depending on the interface
// rather than the concrete store narrows the dependency to what the
// handlers use and lets tests substitute a fake. The vm.migrate worker
// (run.go) is consumer-side and holds the concrete store.
type Store interface {
	resolver.Querier

	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	NodeByName(ctx context.Context, name string) (store.Node, error)
	CreateMigration(ctx context.Context, p store.CreateMigrationParams, args queue.JobArgs) (store.Migration, error)
	MigrationByID(ctx context.Context, id uuid.UUID) (store.Migration, error)
	ListMigrations(ctx context.Context, p store.ListMigrationsParams) ([]store.Migration, error)
	CancelMigration(ctx context.Context, id uuid.UUID, reason string) (store.Migration, error)
}

// Handler bundles the dependencies for the /v1/vms/{id}/migrate +
// /v1/migrations/* routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *etcdstore.Store.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// migrationView mirrors components/schemas/Migration in
// api/openapi/control-plane.yaml. It is the authoritative migration
// record (Observability surface 1): phase, progress, the two failure
// channels (scheduling_reason / error_message), the echoed knobs, and
// timestamps. Internal columns (qmp_capabilities, the raw guard / index
// keys) are not surfaced; no secret fields.
//
// Stalled is a derived signal (a live migration whose bytes_transferred /
// progress_percent are flat while updated_at advances). Slice 1 has no
// data path, so it is always false; the field is kept for forward-compat
// so the wire shape does not change when the watchdog lands.
type migrationView struct {
	ID               string  `json:"id"`
	VMID             string  `json:"vm_id"`
	SourceNodeID     *string `json:"source_node_id"`
	TargetNodeID     *string `json:"target_node_id"`
	Reason           string  `json:"reason"`
	Phase            string  `json:"phase"`
	ProgressPercent  int     `json:"progress_percent"`
	BytesTotal       *int64  `json:"bytes_total"`
	BytesTransferred int64   `json:"bytes_transferred"`
	BytesRemaining   *int64  `json:"bytes_remaining"`
	SchedulingReason *string `json:"scheduling_reason"`
	ErrorMessage     *string `json:"error_message"`
	Live             bool    `json:"live"`
	AllowPostcopy    bool    `json:"allow_postcopy"`
	TargetPoolName   string  `json:"target_pool_name"`
	// Stalled is always false in slice 1 (no data path yet); see doc above.
	Stalled           bool    `json:"stalled"`
	MaxBandwidthBytes *int64  `json:"max_bandwidth_bytes"`
	MaxDowntimeMs     *int32  `json:"max_downtime_ms"`
	StartedAt         *string `json:"started_at"`
	CompletedAt       *string `json:"completed_at"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// toView projects a store.Migration onto its public migrationView. Nullable
// columns render as JSON null via pointer fields; timestamps are RFC 3339
// (UTC). Stalled is hard-wired false in slice 1.
func toView(m store.Migration) migrationView {
	v := migrationView{
		ID:                m.ID.String(),
		VMID:              m.VmID.String(),
		SourceNodeID:      uuidPtrString(m.SourceNodeID),
		TargetNodeID:      uuidPtrString(m.TargetNodeID),
		Reason:            string(m.Reason),
		Phase:             string(m.Phase),
		ProgressPercent:   int(m.ProgressPercent),
		BytesTotal:        m.BytesTotal,
		BytesTransferred:  m.BytesTransferred,
		BytesRemaining:    m.BytesRemaining,
		SchedulingReason:  m.SchedulingReason,
		ErrorMessage:      m.ErrorMessage,
		Live:              m.Live,
		AllowPostcopy:     m.AllowPostcopy,
		TargetPoolName:    m.TargetPoolName,
		Stalled:           false,
		MaxBandwidthBytes: m.MaxBandwidthBytes,
		MaxDowntimeMs:     m.MaxDowntimeMs,
		StartedAt:         timePtrString(m.StartedAt),
		CompletedAt:       timePtrString(m.CompletedAt),
		CreatedAt:         m.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:         m.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	return v
}

// uuidPtrString renders a *uuid.UUID as a *string, preserving nil.
func uuidPtrString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// timePtrString renders a *time.Time as a *string in RFC 3339 (UTC),
// preserving nil.
func timePtrString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}
