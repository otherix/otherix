// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package snapshots hosts the /v1/vms/{id}/snapshots + /v1/snapshots/* HTTP
// handlers - the operator-facing CP surface for snapshot create / list / get /
// delete. A snapshot is a content-addressed, disk-only VM snapshot (ADR slice
// A): create is async (202 + a vm.snapshot.create task); the worker (Task 6)
// drives the agent blob produce and fills the manifest. Get / list project the
// store row; delete is fail-closed CP-side (refuses a snapshot with live
// children) then enqueues a best-effort blob-GC task.
//
// RBAC composition: snapshot:read / snapshot:create / snapshot:delete gate the
// routes at the RequirePermission middleware; ownership is enforced inside the
// handlers via auth.CheckOwnership against the parent VM owner. A caller who
// holds the capability but does not own the VM sees a 404 (no existence leak),
// the canonical 403-vs-404 rule.
package snapshots

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the snapshots HTTP handlers depend on: the
// identifier-resolution contract (resolver.Querier, used to resolve the {id}
// path param to a VM), the VM-runtime read used to resolve vm_state_at_snapshot,
// and the snapshot domain methods (atomic create-with-task, by-id, list,
// fail-closed delete) plus the EnqueueTask producer seam the delete handler uses
// for the async blob GC. *etcdstore.Store satisfies it; depending on the
// interface narrows the dependency to what the handlers use and lets tests
// substitute a fake. The vm.snapshot.* workers (run.go, Task 6) are
// consumer-side and hold the concrete store.
type Store interface {
	resolver.Querier

	VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error)
	CreateSnapshot(ctx context.Context, p store.CreateSnapshotParams, args queue.JobArgs) (store.Snapshot, error)
	SnapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	ListSnapshots(ctx context.Context, p store.ListSnapshotsParams) ([]store.Snapshot, error)
	DeleteSnapshot(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
}

// Handler bundles the dependencies for the /v1/vms/{id}/snapshots +
// /v1/snapshots/* routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any conforming
// backend can be wired in; production passes *etcdstore.Store.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// vmSnapshotView mirrors components/schemas/VmSnapshot in
// api/openapi/control-plane.yaml. It is the public projection of a snapshot
// row: identity + ownership, the disk-only manifest (disks[] + the summed
// disk_size_bytes), the captured VM state, and timestamps. No secret fields.
type vmSnapshotView struct {
	ID                string               `json:"id"`
	VMID              string               `json:"vm_id"`
	OwnerID           string               `json:"owner_id"`
	Name              string               `json:"name"`
	Description       string               `json:"description"`
	Status            string               `json:"status"`
	WithMemory        bool                 `json:"with_memory"`
	VMStateAtSnapshot string               `json:"vm_state_at_snapshot"`
	DiskSizeBytes     *int64               `json:"disk_size_bytes"`
	Disks             []vmSnapshotDiskView `json:"disks"`
	ErrorMessage      *string              `json:"error_message"`
	CreatedAt         string               `json:"created_at"`
	UpdatedAt         string               `json:"updated_at"`
}

// vmSnapshotDiskView mirrors the per-disk blob descriptor in the manifest.
type vmSnapshotDiskView struct {
	Index     int32  `json:"index"`
	Device    string `json:"device"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// toView projects a store.Snapshot onto its public vmSnapshotView. disk_size_bytes
// renders the stored summed size when present, else the live sum of the manifest
// disks (nil when the manifest is still empty, i.e. the snapshot has not yet been
// produced). Nullable columns render as JSON null via pointer fields; timestamps
// are RFC 3339 (UTC).
func toView(s store.Snapshot) vmSnapshotView {
	disks := make([]vmSnapshotDiskView, 0, len(s.Disks))
	var sum int64
	for _, d := range s.Disks {
		disks = append(disks, vmSnapshotDiskView{
			Index:     d.Index,
			Device:    d.Device,
			SHA256:    d.SHA256,
			SizeBytes: d.SizeBytes,
		})
		sum += d.SizeBytes
	}

	size := s.DiskSizeBytes
	if size == nil && len(s.Disks) > 0 {
		size = &sum
	}

	return vmSnapshotView{
		ID:                s.ID.String(),
		VMID:              s.VmID.String(),
		OwnerID:           s.OwnerID.String(),
		Name:              s.Name,
		Description:       s.Description,
		Status:            string(s.Status),
		WithMemory:        s.WithMemory,
		VMStateAtSnapshot: string(s.VMStateAtSnapshot),
		DiskSizeBytes:     size,
		Disks:             disks,
		ErrorMessage:      s.ErrorMessage,
		CreatedAt:         s.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:         s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
