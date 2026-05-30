// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNodeNameExists is returned when a node with the same name already
// exists (uq_nodes_name).
var ErrNodeNameExists = errors.New("store: node name already in use")

// translateNodeUnique maps a unique-constraint violation on the nodes
// table to ErrNodeNameExists; any other error passes through.
func translateNodeUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrNodeNameExists
	}
	return err
}

// NodeDeleteOutcome carries the observability counters produced by a
// force DeleteNode. The non-force path leaves both zero.
type NodeDeleteOutcome struct {
	VMsOrphaned         int64
	MigrationsCancelled int64
}

// NodeEffectiveByID returns the node joined with its effective
// availability view, or ErrNotFound.
func (s *Store) NodeEffectiveByID(ctx context.Context, id uuid.UUID) (NodeEffectiveAvailability, error) {
	n, err := s.queries.GetNodeEffectiveByID(ctx, id)
	if err != nil {
		return NodeEffectiveAvailability{}, translateNoRows(err)
	}
	return n, nil
}

// CreateNode inserts a node, translating the name-uniqueness violation
// to ErrNodeNameExists.
func (s *Store) CreateNode(ctx context.Context, arg CreateNodeParams) (Node, error) {
	n, err := s.queries.CreateNode(ctx, arg)
	if err != nil {
		return Node{}, translateNodeUnique(err)
	}
	return n, nil
}

// CordonNode marks the node cordoned and returns the updated row.
func (s *Store) CordonNode(ctx context.Context, id uuid.UUID) (Node, error) {
	return s.queries.CordonNode(ctx, id)
}

// UncordonNode returns the node to ready and returns the updated row.
func (s *Store) UncordonNode(ctx context.Context, id uuid.UUID) (Node, error) {
	return s.queries.UncordonNode(ctx, id)
}

// ListNodesEffective returns nodes joined with their effective
// availability view, cursor-paginated.
func (s *Store) ListNodesEffective(ctx context.Context, arg ListNodesEffectiveParams) ([]NodeEffectiveAvailability, error) {
	return s.queries.ListNodesEffective(ctx, arg)
}

// DeleteNode soft-deletes the node in a single transaction. Without
// force it refuses with *ResourceInUseError (keys "vms" and
// "active_migrations", both always present) when the node still hosts
// vm_runtime rows or active migrations. With force it first cancels
// every active migration touching the node and orphans every
// vm_runtime row on it (clearing current_node_id, leaving
// vms.desired_phase untouched per migration 00003), recording the
// counts in the returned outcome. callerID is recorded in the
// migration cancel reason for audit.
func (s *Store) DeleteNode(ctx context.Context, id uuid.UUID, force bool, callerID uuid.UUID) (NodeDeleteOutcome, error) {
	var out NodeDeleteOutcome
	err := s.InTx(ctx, func(q *Queries) error {
		vmCount, err := q.CountVMRuntimeOnNode(ctx, id)
		if err != nil {
			return err
		}
		migCount, err := q.CountActiveMigrationsOnNode(ctx, id)
		if err != nil {
			return err
		}

		if !force && (vmCount > 0 || migCount > 0) {
			return &ResourceInUseError{Resources: map[string]int64{
				"vms":               vmCount,
				"active_migrations": migCount,
			}}
		}

		if force {
			reason := fmt.Sprintf("source/target node %s force-deleted by user %s", id, callerID)
			cancelled, err := q.CancelActiveMigrationsOnNode(ctx, CancelActiveMigrationsOnNodeParams{
				Reason: reason,
				NodeID: id,
			})
			if err != nil {
				return err
			}
			out.MigrationsCancelled = cancelled

			orphaned, err := q.OrphanVMRuntimeOnNode(ctx, id)
			if err != nil {
				return err
			}
			out.VMsOrphaned = orphaned
		}

		return q.SoftDeleteNode(ctx, id)
	})
	if err != nil {
		return NodeDeleteOutcome{}, err
	}
	return out, nil
}
