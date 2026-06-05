// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// resolveViewNames converts the FK / observed-state UUIDs carried on
// the vms + vm_disks + vm_runtime rows into the operator-facing name
// strings that the vmView surfaces. The function does a handful of small
// GetByID lookups (pool, optional node, and one per attached NIC's
// network) - N+1 per row in list endpoints, which is acceptable for the
// current inventory sizes; a later iteration may swap to a JOIN-based
// query.
//
// The lookups tolerate missing rows: a vm_runtime row without
// current_node_id (creating state) surfaces as nil node; a pool
// soft-deleted out from under a running vm preserves the pool's name on
// the view by surfacing the last-seen name and an error sentinel that
// the caller decides how to handle. Currently every caller surfaces such
// inconsistencies as 500 - they should not happen against the live
// schema.
func (h *Handler) resolveViewNames(ctx context.Context, vm store.VM, runtime *store.VMRuntime, disk store.VMDisk, includeOwner bool) (vmViewNames, error) {
	names := vmViewNames{}

	pool, err := h.store.StoragePoolByID(ctx, disk.StoragePoolID)
	if err != nil {
		return names, fmt.Errorf("load pool name: %v", err)
	}
	names.pool = pool.Name

	if includeOwner {
		owner, err := h.store.UserByID(ctx, vm.OwnerID)
		switch {
		case err == nil:
			label := ownerLabel(owner)
			names.owner = &label
		case errors.Is(err, store.ErrNotFound):
			// Owner soft-deleted (ON DELETE RESTRICT blocks this in
			// practice); leave nil so owner_id still carries the UUID.
		default:
			return names, fmt.Errorf("load owner name: %v", err)
		}
	}

	if nodeID := observedNodeID(vm, runtime); nodeID != nil {
		node, err := h.store.NodeByID(ctx, *nodeID)
		switch {
		case err == nil:
			n := node.Name
			names.node = &n
		case errors.Is(err, store.ErrNotFound):
			// Node was soft-deleted (force-delete path orphans
			// runtime rows but leaves a stale pinned_node_id).
			// Surface as nil; the row's status will already read
			// 'orphaned' through projectStatus.
		default:
			return names, fmt.Errorf("load node name: %v", err)
		}
	}

	networks, err := h.resolveNetworkNames(ctx, vm.ID)
	if err != nil {
		return names, err
	}
	names.networks = networks

	return names, nil
}

// resolveNetworkNames returns the VM's attached network names ordered
// by NIC device_order (ListVMNicsByVM sorts on it), one extra small
// lookup per NIC. A network soft-deleted out from under a NIC is
// skipped rather than surfaced as an error - network delete is blocked
// while NICs reference it, so this only guards an out-of-band mutation.
func (h *Handler) resolveNetworkNames(ctx context.Context, vmID uuid.UUID) ([]string, error) {
	nics, err := h.store.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("list vm nics: %v", err)
	}
	if len(nics) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(nics))
	for _, nic := range nics {
		net, err := h.store.NetworkByID(ctx, nic.NetworkID)
		switch {
		case err == nil:
			names = append(names, net.Name)
		case errors.Is(err, store.ErrNotFound):
			continue
		default:
			return nil, fmt.Errorf("load network name: %v", err)
		}
	}
	return names, nil
}

// ownerLabel picks the best human-readable identifier for a VM owner:
// the display_name when set, falling back to the email (a NOT NULL
// column) so the resolved owner field is never an empty string - a
// bootstrap-seeded admin, for instance, has no display_name. Both
// fields sit behind the user:read gate, so the email fallback widens
// what an already-privileged caller sees, not who can see it.
func ownerLabel(u store.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Email
}

// callerCanReadUsers reports whether the request principal holds
// user:read - the gate for resolving an owner's display_name onto the VM
// view. A missing principal (Authn not applied) reads as no, so the
// owner name never leaks on an unauthenticated path.
func callerCanReadUsers(ctx context.Context) bool {
	u := auth.UserFromContext(ctx)
	return u != nil && auth.Has(u.Role, auth.PermUserRead)
}

// observedNodeID returns the node the VM is currently *located on* per
// D6: vm_runtime.current_node_id wins (real-time agent-reported
// state). When no runtime row exists yet, falls back to
// vms.pinned_node_id so a 'creating' VM still shows where the
// scheduler placed it; returns nil otherwise.
func observedNodeID(vm store.VM, runtime *store.VMRuntime) *uuid.UUID {
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
