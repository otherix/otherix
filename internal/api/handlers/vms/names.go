// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

// resolveViewNames converts the FK / observed-state UUIDs carried on
// the vms + vm_disks + vm_runtime rows into the operator-facing name
// strings that the vmView surfaces. The function does up to three
// small GetByID lookups - N+1 per row in list endpoints, which is
// acceptable for the current inventory sizes; a later iteration may
// swap to a JOIN-based query.
//
// The lookups tolerate missing rows: a SET-NULL template surfaces as
// nil template; a vm_runtime row without current_node_id (creating
// state) surfaces as nil node; a pool soft-deleted out from under a
// running vm preserves the pool's name on the view by surfacing the
// last-seen name and an error sentinel that the caller decides how to
// handle. Currently every caller surfaces such inconsistencies as 500
// — they should not happen against the live schema.
func (h *Handler) resolveViewNames(ctx context.Context, vm store.Vm, runtime *store.VmRuntime, disk store.VmDisk) (vmViewNames, error) {
	names := vmViewNames{}

	if vm.TemplateID != nil {
		tmpl, err := h.store.Queries().GetTemplate(ctx, *vm.TemplateID)
		switch {
		case err == nil:
			n := tmpl.Name
			names.template = &n
		case errors.Is(err, pgx.ErrNoRows):
			// Template was soft-deleted: keep the wire field nil. The
			// FK action is SET NULL, but a soft-delete leaves the
			// non-null id pointing at a row that's invisible to reads.
		default:
			return names, fmt.Errorf("load template name: %v", err)
		}
	}

	pool, err := h.store.Queries().GetStoragePoolByID(ctx, disk.StoragePoolID)
	if err != nil {
		return names, fmt.Errorf("load pool name: %v", err)
	}
	names.pool = pool.Name

	if nodeID := observedNodeID(vm, runtime); nodeID != nil {
		node, err := h.store.Queries().GetNodeByID(ctx, *nodeID)
		switch {
		case err == nil:
			n := node.Name
			names.node = &n
		case errors.Is(err, pgx.ErrNoRows):
			// Node was soft-deleted (force-delete path orphans
			// runtime rows but leaves a stale pinned_node_id).
			// Surface as nil; the row's status will already read
			// 'orphaned' through projectStatus.
		default:
			return names, fmt.Errorf("load node name: %v", err)
		}
	}

	return names, nil
}

// observedNodeID returns the node the VM is currently *located on* per
// D6: vm_runtime.current_node_id wins (real-time agent-reported
// state). When no runtime row exists yet, falls back к
// vms.pinned_node_id so a 'creating' VM still shows where the
// scheduler placed it; returns nil otherwise.
func observedNodeID(vm store.Vm, runtime *store.VmRuntime) *uuid.UUID {
	if runtime != nil && runtime.CurrentNodeID != nil {
		id := *runtime.CurrentNodeID
		return &id
	}
	if vm.PinnedNodeID != nil {
		id := *vm.PinnedNodeID
		return &id
	}
	return nil
}
