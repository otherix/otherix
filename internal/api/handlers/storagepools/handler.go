// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package storagepools hosts the /v1/storage-pools/* HTTP handlers.
// CRUD is gated by `storage_pool:read` (every role) and
// `storage_pool:manage` (admin only) per docs/rbac.md. Storage pools
// have no owner column — the per-instance projection returned to
// every caller is identical, so no dual-shape view is needed at the
// instance level.
//
// Two columns are intentionally absent from the request surface:
// `capacity_bytes`, `available_bytes` and `reported_at` are
// agent-reported and populated through the future
// heartbeat / scan paths. POST and PATCH bodies that mention these
// keys are rejected with 400 forbidden_fields.
//
// Multi-instance model: there is no `is_default` per-row column -
// cluster default-pool is a single value held on the
// cluster_settings singleton, not a per-instance property. The
// GET-by-name path returns an aggregated PoolConceptView with every
// per-node instance plus the cluster-default flag; the GET-by-UUID
// path keeps returning a single StoragePool view (callers asking for
// a specific instance want the instance).
//
// Delete is conditional on no active vm_disks references; storage
// pools have no force-delete counterpart by design (the operator
// must remove or migrate the dependent disks first). The async
// `storage_pools.scan` operation runs over the etcd worker dispatcher.
package storagepools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the storage-pools handlers depend on:
// the pool domain methods, the identifier-resolution contract
// (resolver.Querier) used to resolve pool and node parameters, the node
// read used by the view projectors, the per-pool image inventory read
// (agent-reported observed state surfaced on pool-get), and the
// EnqueueTask producer seam used by Scan.
// *etcdstore.Store satisfies it; depending on the interface rather than the
// concrete store narrows the handler's storage dependency to the methods it
// uses, lets tests substitute a fake, and keeps the queue off the request
// handlers. The scan worker (jobs.go, scan_trigger.go) is consumer-side and
// holds the concrete store.
type Store interface {
	resolver.Querier

	CreateStoragePool(ctx context.Context, arg store.CreateStoragePoolParams) (store.StoragePool, error)
	UpdateStoragePool(ctx context.Context, arg store.UpdateStoragePoolParams) (store.StoragePool, error)
	PoolEffectiveByID(ctx context.Context, id uuid.UUID) (store.PoolEffectiveCapacity, error)
	ListPoolsEffective(ctx context.Context, arg store.ListPoolsEffectiveParams) ([]store.PoolEffectiveCapacity, error)
	ListPoolsEffectiveByName(ctx context.Context, name string) ([]store.PoolEffectiveCapacity, error)
	DeleteStoragePool(ctx context.Context, id uuid.UUID) error
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	PoolImageInventory(ctx context.Context, poolID uuid.UUID) ([]store.PoolImage, error)
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
}

// Handler bundles the dependencies for the storage-pools routes. Scan
// enqueues through the store's EnqueueTask seam, so the handler holds no
// queue client.
type Handler struct {
	store Store
	cfg   config.StoragePoolsConfig
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *etcdstore.Store.
// cfg pins the operator-facing pool knobs (allowlist prefixes).
func New(s Store, cfg config.StoragePoolsConfig, log *slog.Logger) *Handler {
	return &Handler{
		store: s,
		cfg:   cfg,
		log:   log,
	}
}

// storagePoolView mirrors components/schemas/StoragePool in
// api/openapi/control-plane.yaml. The referenced node is exposed by
// its name string (operator-facing identifier rather than UUID); the
// soft-delete timestamp is intentionally absent.
//
// There is no per-row `is_default` - cluster default-pool semantics
// live in cluster_settings. `is_cluster_default` here is a derived
// boolean: true when this instance's pool name matches
// cluster_settings.default_pool_name (case-insensitive). All
// instances sharing a name return the same flag value -
// operator-facing view, not row-level state.
type storagePoolView struct {
	ID                      string            `json:"id"`
	Node                    string            `json:"node"`
	NodeStatus              *string           `json:"node_status"`
	Name                    string            `json:"name"`
	Type                    string            `json:"type"`
	Path                    string            `json:"path"`
	CapacityBytes           *int64            `json:"capacity_bytes"`
	AvailableBytes          *int64            `json:"available_bytes"`
	AvailableBytesEffective *int64            `json:"available_bytes_effective"`
	ReportedAt              *string           `json:"reported_at"`
	IsClusterDefault        bool              `json:"is_cluster_default"`
	Config                  json.RawMessage   `json:"config"`
	DiskPressure            *diskPressureView `json:"disk_pressure"`
	ReconciliationStatus    string            `json:"reconciliation_status"`
	LastReconciledAt        *string           `json:"last_reconciled_at"`
	ReconciliationError     *string           `json:"reconciliation_error"`
	CreatedAt               string            `json:"created_at"`
	UpdatedAt               string            `json:"updated_at"`
	Images                  []poolImageView   `json:"images"`
}

// diskPressureView mirrors components/schemas/DiskPressureCondition.
// Nullable on the wire — a pool without
// active pressure has the parent `disk_pressure` field set to JSON
// null. Same shape as the node-side MemoryPressureCondition /
// SystemDiskPressureCondition.
type diskPressureView struct {
	Since            string `json:"since"`
	ConsecutiveCount int32  `json:"consecutive_count"`
}

// poolConceptView is the aggregated cluster-wide projection of a pool
// name, returned by GET /v1/storage-pools/{name}. Mirrors
// components/schemas/PoolConceptView in api/openapi/control-plane.yaml.
type poolConceptView struct {
	Name             string            `json:"name"`
	Type             string            `json:"type"`
	IsClusterDefault bool              `json:"is_cluster_default"`
	Instances        []storagePoolView `json:"instances"`
}

// toViewEffective projects a pool_effective_capacity row onto its
// public storagePoolView, substituting the pre-resolved node name. All
// read paths (Get / List / GetByName) and write-path responses (Create /
// Update routed through projectView) consume view rows so callers
// always see `available_bytes_effective` alongside raw — operators
// can spot a divergence (pending-VM-disk pressure) at a glance.
// The Images slice is initialised empty (non-nil) so the wire field
// renders `[]` rather than null on the read paths that do not embed the
// agent-reported inventory (list, create/update echo). The pool-get paths
// (projectView, projectInstanceForConcept) overwrite it with the live
// PoolImageInventory read.
func toViewEffective(p store.PoolEffectiveCapacity, nodeName string, nodeStatus store.NodeStatus, isClusterDefault bool) storagePoolView {
	view := storagePoolView{
		Images:                  []poolImageView{},
		ID:                      p.ID.String(),
		Node:                    nodeName,
		NodeStatus:              nodeStatusOrNil(nodeStatus),
		Name:                    p.Name,
		Type:                    p.Type,
		Path:                    p.Path,
		CapacityBytes:           p.CapacityBytes,
		AvailableBytes:          p.AvailableBytes,
		AvailableBytesEffective: p.AvailableBytesEffective,
		IsClusterDefault:        isClusterDefault,
		Config:                  rawJSONOrEmpty(p.Config),
		DiskPressure:            toDiskPressureView(p.DiskPressureSince, p.DiskPressureCount),
		ReconciliationStatus:    p.ReconciliationStatus,
		ReconciliationError:     p.ReconciliationError,
		CreatedAt:               p.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:               p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if p.ReportedAt != nil {
		s := p.ReportedAt.UTC().Format(time.RFC3339Nano)
		view.ReportedAt = &s
	}
	if p.LastReconciledAt != nil {
		s := p.LastReconciledAt.UTC().Format(time.RFC3339Nano)
		view.LastReconciledAt = &s
	}
	return view
}

// nodeStatusOrNil projects the owning node's status enum onto the
// nullable wire field. Returns nil when the node row was soft-
// deleted out from under the pool (orphaned-pool path in projectView /
// projectListViews substitutes `store.Node{}`, which carries an
// empty NodeStatus). Operators see `node_status: null` and can dig
// into the FK inconsistency via /v1/nodes.
func nodeStatusOrNil(s store.NodeStatus) *string {
	if s == "" {
		return nil
	}
	v := string(s)
	return &v
}

// toDiskPressureView projects the (since, count) pair on a view-backed
// pool row into the nullable wire shape.
// Returns nil when the condition is not active — `since == nil`.
func toDiskPressureView(since *time.Time, count int32) *diskPressureView {
	if since == nil {
		return nil
	}
	return &diskPressureView{
		Since:            since.UTC().Format(time.RFC3339Nano),
		ConsecutiveCount: count,
	}
}

// projectView re-fetches the pool row through pool_effective_capacity
// (so the response surfaces `available_bytes_effective`), looks up the
// owning node's name plus the cluster-default flag, and folds them
// into the public storagePoolView. The caller supplies a store.StoragePool
// — typically the one resolver.Pool returned — purely for the ID and
// the not-found fallback; the actual rendered fields come from the
// view row.
func (h *Handler) projectView(ctx context.Context, p store.StoragePool) (storagePoolView, error) {
	row, err := h.store.PoolEffectiveByID(ctx, p.ID)
	if err != nil {
		return storagePoolView{}, err
	}
	node, err := h.store.NodeByID(ctx, row.NodeID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return storagePoolView{}, err
		}
		// Owning node was soft-deleted out from under the pool —
		// surface as an empty string so the wire field stays
		// well-typed; the operator can dig into the inconsistency
		// via /v1/nodes if needed.
		node = store.Node{}
	}
	settings, err := h.store.ClusterSettings(ctx)
	if err != nil {
		return storagePoolView{}, err
	}
	isDefault := settings.DefaultPoolName != nil &&
		strings.EqualFold(*settings.DefaultPoolName, row.Name)
	view := toViewEffective(row, node.Name, node.Status, isDefault)
	images, err := h.poolImages(ctx, p.ID)
	if err != nil {
		return storagePoolView{}, err
	}
	view.Images = images
	return view, nil
}

// poolImages reads the agent-reported image inventory for a pool and maps
// it onto the public wire shape. Images are an agent-owned cache (observed
// state reported through heartbeat) with no CP-side row; the slice is
// always non-nil so the wire field renders `[]` rather than null when the
// pool has no cached images.
func (h *Handler) poolImages(ctx context.Context, poolID uuid.UUID) ([]poolImageView, error) {
	inventory, err := h.store.PoolImageInventory(ctx, poolID)
	if err != nil {
		return nil, err
	}
	views := make([]poolImageView, 0, len(inventory))
	for _, im := range inventory {
		views = append(views, poolImageView{
			Name:             im.Basename,
			SHA256:           im.ChecksumSha256,
			SizeBytes:        im.SizeBytes,
			VirtualSizeBytes: im.VirtualSizeBytes,
			Format:           im.Format,
			ImportedAt:       im.ImportedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return views, nil
}

// rawJSONOrEmpty returns raw if non-empty, otherwise the JSON object
// literal `{}`. storage_pools.config is NOT NULL with a `'{}'`
// default; the helper keeps the wire format honest if the bytes ever
// come back empty (post-restore, unusual rollback paths, etc).
func rawJSONOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}
