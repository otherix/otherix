// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"

	"github.com/google/uuid"
)

// HeartbeatTx is the transactional storage surface the agent
// heartbeat projection drives. The full projection runs inside a single
// transaction (InHeartbeatTx) so a partially applied heartbeat can never
// reach the database.
//
// NodeForHeartbeat, NodeByID, and LookupFirmwareByCatalog translate the
// missing-row driver error to ErrNotFound so the projection branches on
// the store sentinel rather than on pgx; the remaining methods are
// pass-throughs to the generated queries.
type HeartbeatTx interface {
	NodeForHeartbeat(ctx context.Context, nodeID uuid.UUID) (GetNodeForHeartbeatRow, error)
	NodeByID(ctx context.Context, id uuid.UUID) (Node, error)
	UpdateNodeHeartbeat(ctx context.Context, arg UpdateNodeHeartbeatParams) error
	UpdateNodeMemoryPressure(ctx context.Context, arg UpdateNodeMemoryPressureParams) error
	UpdateNodeSystemDiskPressure(ctx context.Context, arg UpdateNodeSystemDiskPressureParams) error
	LookupFirmwareByCatalog(ctx context.Context, arg LookupFirmwareByCatalogParams) (uuid.UUID, error)
	UpsertNodeFirmware(ctx context.Context, arg UpsertNodeFirmwareParams) error
	FilterExistingVMIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	UpsertVMRuntime(ctx context.Context, arg UpsertVMRuntimeParams) error
	UpdateStoragePoolReconciliation(ctx context.Context, arg UpdateStoragePoolReconciliationParams) error
	ListStoragePoolsByNode(ctx context.Context, nodeID uuid.UUID) ([]StoragePool, error)
	ListVMsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]ListVMsForNodeDeclaredRow, error)
}

// heartbeatTx adapts *Queries to HeartbeatTx, translating the not-found
// driver error on the three lookups the projection branches on.
type heartbeatTx struct{ q *Queries }

func (h heartbeatTx) NodeForHeartbeat(ctx context.Context, nodeID uuid.UUID) (GetNodeForHeartbeatRow, error) {
	row, err := h.q.GetNodeForHeartbeat(ctx, nodeID)
	if err != nil {
		return GetNodeForHeartbeatRow{}, translateNoRows(err)
	}
	return row, nil
}

func (h heartbeatTx) NodeByID(ctx context.Context, id uuid.UUID) (Node, error) {
	row, err := h.q.GetNodeByID(ctx, id)
	if err != nil {
		return Node{}, translateNoRows(err)
	}
	return row, nil
}

func (h heartbeatTx) LookupFirmwareByCatalog(ctx context.Context, arg LookupFirmwareByCatalogParams) (uuid.UUID, error) {
	id, err := h.q.LookupFirmwareByCatalog(ctx, arg)
	if err != nil {
		return uuid.Nil, translateNoRows(err)
	}
	return id, nil
}

func (h heartbeatTx) UpdateNodeHeartbeat(ctx context.Context, arg UpdateNodeHeartbeatParams) error {
	return h.q.UpdateNodeHeartbeat(ctx, arg)
}

func (h heartbeatTx) UpdateNodeMemoryPressure(ctx context.Context, arg UpdateNodeMemoryPressureParams) error {
	return h.q.UpdateNodeMemoryPressure(ctx, arg)
}

func (h heartbeatTx) UpdateNodeSystemDiskPressure(ctx context.Context, arg UpdateNodeSystemDiskPressureParams) error {
	return h.q.UpdateNodeSystemDiskPressure(ctx, arg)
}

func (h heartbeatTx) UpsertNodeFirmware(ctx context.Context, arg UpsertNodeFirmwareParams) error {
	return h.q.UpsertNodeFirmware(ctx, arg)
}

func (h heartbeatTx) FilterExistingVMIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	return h.q.FilterExistingVMIDs(ctx, ids)
}

func (h heartbeatTx) UpsertVMRuntime(ctx context.Context, arg UpsertVMRuntimeParams) error {
	return h.q.UpsertVMRuntime(ctx, arg)
}

func (h heartbeatTx) UpdateStoragePoolReconciliation(ctx context.Context, arg UpdateStoragePoolReconciliationParams) error {
	return h.q.UpdateStoragePoolReconciliation(ctx, arg)
}

func (h heartbeatTx) ListStoragePoolsByNode(ctx context.Context, nodeID uuid.UUID) ([]StoragePool, error) {
	return h.q.ListStoragePoolsByNode(ctx, nodeID)
}

func (h heartbeatTx) ListVMsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]ListVMsForNodeDeclaredRow, error) {
	return h.q.ListVMsForNodeDeclared(ctx, nodeID)
}

// InHeartbeatTx runs fn inside a single transaction, handing it the
// HeartbeatTx projection surface. The transaction commits when fn
// returns nil and rolls back on any error (or panic), so a partially
// applied heartbeat never lands.
func (s *Store) InHeartbeatTx(ctx context.Context, fn func(HeartbeatTx) error) error {
	return s.InTx(ctx, func(q *Queries) error {
		return fn(heartbeatTx{q: q})
	})
}
