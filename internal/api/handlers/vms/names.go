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
func (h *Handler) resolveViewNames(ctx context.Context, vm store.VM, runtime *store.VMRuntime, disk store.VMDisk, includeOwner bool, activeMig *store.Migration) (vmViewNames, error) {
	names := vmViewNames{}

	// An unscheduled (pending) VM has no disk row yet, so disk is the
	// zero value (StoragePoolID == uuid.Nil). Skip the pool lookup; toView
	// falls back to the SchedulingSpec for the display pool in that case.
	if disk.StoragePoolID != uuid.Nil {
		pool, err := h.store.StoragePoolByID(ctx, disk.StoragePoolID)
		if err != nil {
			return names, fmt.Errorf("load pool name: %v", err)
		}
		names.pool = pool.Name
	}

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

	nodeID := observedNodeID(vm, runtime)
	if activeMig != nil && activeMig.SourceNodeID != nil {
		// A VM in flight stays displayed on its source node (where the guest
		// runs until cutover); the target agent overwrites vm_runtime.current_node_id
		// to itself before cutover, so observedNodeID would otherwise show the
		// target early. The migration record holds the authoritative source.
		nodeID = activeMig.SourceNodeID
	}
	if nodeID != nil {
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

	networks, nics, err := h.resolveNICs(ctx, vm.ID)
	if err != nil {
		return names, err
	}
	names.networks = networks
	names.nics = nics

	return names, nil
}

// resolveNICs loads the VM's NICs once (ordered by device_order, primary first)
// and resolves them into two parallel views: the attached network names and the
// compact per-NIC nicView (network name + MAC + allocated IPv4). One NIC load
// feeds both, so the per-NIC IPv4 surface costs no extra store round-trip beyond
// the per-NIC NetworkByID lookups the names already required. A network
// soft-deleted out from under a NIC is skipped (in both slices, keeping them
// aligned) rather than surfaced as an error - network delete is blocked while
// NICs reference it, so this only guards an out-of-band mutation.
func (h *Handler) resolveNICs(ctx context.Context, vmID uuid.UUID) ([]string, []nicView, error) {
	nics, err := h.store.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return nil, nil, fmt.Errorf("list vm nics: %v", err)
	}
	if len(nics) == 0 {
		return nil, nil, nil
	}
	names := make([]string, 0, len(nics))
	views := make([]nicView, 0, len(nics))
	for _, nic := range nics {
		net, err := h.store.NetworkByID(ctx, nic.NetworkID)
		switch {
		case err == nil:
			names = append(names, net.Name)
			views = append(views, nicView{
				Network:     net.Name,
				MAC:         nic.MacAddress.String(),
				Ipv4Address: nicIPv4(nic),
			})
		case errors.Is(err, store.ErrNotFound):
			continue
		default:
			return nil, nil, fmt.Errorf("load network name: %v", err)
		}
	}
	return names, views, nil
}

// nicIPv4 renders a NIC's CP-IPAM-allocated IPv4 as a *string for the wire view,
// returning nil when the NIC has no allocation (e.g. a plain bridge network).
func nicIPv4(nic store.VMNic) *string {
	if nic.Ipv4Address == nil {
		return nil
	}
	s := nic.Ipv4Address.String()
	return &s
}

// ownerLabel picks the best human-readable identifier for a VM owner:
// the display_name when set, falling back to the username (always
// present) so the resolved owner field is never an empty string - a
// bootstrap-seeded admin, for instance, has no display_name and an
// optional email may be absent. Both fields sit behind the user:read
// gate, so the username fallback widens what an already-privileged
// caller sees, not who can see it.
func ownerLabel(u store.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// callerCanReadUsers reports whether the request principal holds
// user:read - the gate for resolving an owner's display_name onto the VM
// view. A missing principal (Authn not applied) reads as no, so the
// owner name never leaks on an unauthenticated path.
func callerCanReadUsers(ctx context.Context) bool {
	u := auth.UserFromContext(ctx)
	return u != nil && auth.Has(u.Role, auth.PermUserRead)
}

// callerCanReadVMSecrets reports whether the request principal may see the secret-bearing VM
// fields (user_data, network_config, image_url). The gate is vm:console on THIS VM: a caller who
// can console into the guest can already extract these, so the view reveals nothing new. admin /
// operator hold console at any scope; a developer holds it for own VMs; a viewer holds none. A
// missing principal reads as false, so secrets never leak on an unauthenticated path.
func callerCanReadVMSecrets(ctx context.Context, ownerID uuid.UUID) bool {
	return auth.CheckOwnership(auth.UserFromContext(ctx), &ownerID, auth.PermVMConsole) == nil
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
