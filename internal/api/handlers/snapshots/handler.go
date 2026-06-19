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
	"github.com/otherix/otherix/internal/api/handlers/replication"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the snapshots HTTP handlers depend on: the
// identifier-resolution contract (resolver.Querier, used to resolve the {id}
// path param to a VM), the VM-runtime read used to resolve vm_state_at_snapshot,
// and the snapshot domain methods (atomic create-with-task, by-id, list,
// fail-closed delete that enqueues the blob-GC task+job in the SAME transaction).
// *etcdstore.Store satisfies it; depending on the interface narrows the dependency
// to what the handlers use and lets tests substitute a fake. The vm.snapshot.*
// workers (run.go, Task 6) are consumer-side and hold the concrete store.
type Store interface {
	resolver.Querier

	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	UserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	ArtifactPoolByName(ctx context.Context, name string) (store.ArtifactPool, error)
	VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error)
	CreateSnapshot(ctx context.Context, p store.CreateSnapshotParams, args queue.JobArgs) (store.Snapshot, error)
	SnapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	ListSnapshots(ctx context.Context, p store.ListSnapshotsParams) ([]store.Snapshot, error)
	DeleteSnapshot(ctx context.Context, id uuid.UUID, taskParams store.CreateTaskParams, args queue.JobArgs) (store.Snapshot, error)

	// SnapshotsReferencingBlob, BlobHolders and AllNodes back the durability
	// projection (see viewWithDurability); together with SnapshotByID and
	// ArtifactPoolByName above they let *etcdstore.Store satisfy
	// replication.DurabilityStore.
	SnapshotsReferencingBlob(ctx context.Context, digest string) ([]uuid.UUID, error)
	BlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error)
	AllNodes(ctx context.Context) ([]store.Node, error)
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
	ID                string `json:"id"`
	VMID              string `json:"vm_id"`
	OwnerID           string `json:"owner_id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Status            string `json:"status"`
	WithMemory        bool   `json:"with_memory"`
	VMStateAtSnapshot string `json:"vm_state_at_snapshot"`
	// VMName is the source VM name captured at snapshot time, denormalised onto
	// the snapshot so it survives the source VM's deletion (provenance). Empty
	// only for legacy rows created before the field existed.
	VMName           string               `json:"vm_name"`
	Architecture     string               `json:"architecture"`
	FirmwareID       *string              `json:"firmware_id"`
	DiskSizeBytes    *int64               `json:"disk_size_bytes"`
	Disks            []vmSnapshotDiskView `json:"disks"`
	ErrorMessage     *string              `json:"error_message"`
	ArtifactPoolName *string              `json:"artifact_pool_name"`
	// Durability is the computed replication health of the snapshot's blobs:
	// "durable" once every disk has at least its desired replica count of live
	// holders, "replicating" while replicas are still being placed, "degraded"
	// when a blob is short with no progress possible, "unknown" before the
	// manifest is produced. desired_replicas/observed_replicas are the
	// max/min over the manifest disks.
	Durability       string `json:"durability"`
	DesiredReplicas  int    `json:"desired_replicas"`
	ObservedReplicas int    `json:"observed_replicas"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
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

	var firmwareID *string
	if s.SourceFirmwareID != nil {
		fw := s.SourceFirmwareID.String()
		firmwareID = &fw
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
		VMName:            s.SourceVMName,
		Architecture:      string(s.SourceArchitecture),
		FirmwareID:        firmwareID,
		DiskSizeBytes:     size,
		Disks:             disks,
		ErrorMessage:      s.ErrorMessage,
		ArtifactPoolName:  s.ArtifactPoolName,
		CreatedAt:         s.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:         s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// viewWithDurability projects a snapshot onto its public view and folds in the
// CP-computed durability status. Durability is best-effort: on a store error it
// renders "unknown" and logs, never failing the request.
func (h *Handler) viewWithDurability(ctx context.Context, s store.Snapshot) vmSnapshotView {
	v := toView(s)
	status, desired, observed, err := replication.SnapshotDurability(ctx, h.store, s)
	if err != nil {
		h.log.WarnContext(ctx, "compute snapshot durability",
			slog.String("snapshot_id", s.ID.String()), slog.Any("error", err))
		status, desired, observed = replication.DurabilityUnknown, 0, 0
	}
	v.Durability = status
	v.DesiredReplicas = desired
	v.ObservedReplicas = observed
	return v
}
