// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// VMs are user-owned, cluster-wide, addressed by UUID with a global
// case-insensitive name guard (uq_vms_name, partial on deleted_at). Desired
// state (the vms row) and observed runtime (vm_runtime) are separate keys.
// ListVMs filters by pool (a disk on the pool) and node (the runtime's current
// node). The write path (CreateScheduledVM) lands with the queue slice; this
// file is the read surface plus VMByName (the last resolver lookup).

func vmPrefix() string { return etcd.Key("vms") + "/" }

func vmNameGuard(name string) string {
	return etcd.Key("uniq", "vms", "name", strings.ToLower(name))
}

func vmDisksVMIndexPrefix(vmID uuid.UUID) string {
	return etcd.Key("index", "vm_disks", "vm", vmID.String()) + "/"
}

// VMByID returns the non-deleted vms row, or store.ErrNotFound.
func (s *Store) VMByID(ctx context.Context, id uuid.UUID) (store.VM, error) {
	var v store.VM
	found, err := s.c.GetJSON(ctx, vmKey(id), &v)
	if err != nil {
		return store.VM{}, err
	}
	if !found || v.DeletedAt != nil {
		return store.VM{}, store.ErrNotFound
	}
	return v, nil
}

// VMByName returns the non-deleted vms row with the given name
// (case-insensitive) via the name guard, or store.ErrNotFound.
func (s *Store) VMByName(ctx context.Context, name string) (store.VM, error) {
	id, found, err := s.resolveGuard(ctx, vmNameGuard(name))
	if err != nil {
		return store.VM{}, err
	}
	if !found {
		return store.VM{}, store.ErrNotFound
	}
	return s.VMByID(ctx, id)
}

// VMRuntimeByID returns the observed runtime row for a VM, or store.ErrNotFound
// when the worker has not upserted one yet.
func (s *Store) VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error) {
	var rt store.VMRuntime
	found, err := s.c.GetJSON(ctx, vmRuntimeKey(vmID), &rt)
	if err != nil {
		return store.VMRuntime{}, err
	}
	if !found {
		return store.VMRuntime{}, store.ErrNotFound
	}
	return rt, nil
}

// ListVMDisksByVM returns the active disks attached to a VM, ordered by
// device_order ascending.
func (s *Store) ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error) {
	disks, err := s.disksOfVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].DeviceOrder < disks[j].DeviceOrder })
	return disks, nil
}

// ListVMs returns the non-deleted VMs matching the optional pool/node filters,
// ordered by (created_at, id) descending, after the cursor, capped at
// LimitCount.
func (s *Store) ListVMs(ctx context.Context, arg store.ListVMsParams) ([]store.VM, error) {
	items, err := s.c.Range(ctx, vmPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.VM, 0, len(items))
	for _, kv := range items {
		var v store.VM
		if err := json.Unmarshal(kv.Value, &v); err != nil {
			return nil, fmt.Errorf("unmarshal vm %q: %v", kv.Key, err)
		}
		if v.DeletedAt != nil {
			continue
		}
		if !beforeCursor(v.CreatedAt, v.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		match, err := s.vmMatchesFilters(ctx, v, arg)
		if err != nil {
			return nil, err
		}
		if !match {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// UpdateVMRuntimePhase writes the observed runtime phase for a VM, stamping
// last_observed_at. A missing runtime row is a no-op (matching the SQL update
// affecting zero rows), so the synchronous lifecycle handlers do not error
// before the worker has upserted.
func (s *Store) UpdateVMRuntimePhase(ctx context.Context, arg store.UpdateVMRuntimePhaseParams) error {
	var rt store.VMRuntime
	found, err := s.c.GetJSON(ctx, vmRuntimeKey(arg.VmID), &rt)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	rt.Phase = arg.Phase
	now := time.Now().UTC()
	rt.LastObservedAt = &now
	return s.c.PutJSON(ctx, vmRuntimeKey(arg.VmID), rt)
}

// vmMatchesFilters applies the pool (a disk on the pool) and node (the runtime's
// current node) EXISTS filters of ListVMs.
func (s *Store) vmMatchesFilters(ctx context.Context, v store.VM, arg store.ListVMsParams) (bool, error) {
	if arg.PoolIDFilter != nil {
		disks, err := s.disksOfVM(ctx, v.ID)
		if err != nil {
			return false, err
		}
		var onPool bool
		for _, d := range disks {
			if d.StoragePoolID == *arg.PoolIDFilter {
				onPool = true
				break
			}
		}
		if !onPool {
			return false, nil
		}
	}
	if arg.NodeIDFilter != nil {
		rt, err := s.VMRuntimeByID(ctx, v.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if rt.CurrentNodeID == nil || *rt.CurrentNodeID != *arg.NodeIDFilter {
			return false, nil
		}
	}
	return true, nil
}

// disksOfVM loads the active disks attached to a VM via the vm disk index.
func (s *Store) disksOfVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error) {
	items, err := s.c.Range(ctx, vmDisksVMIndexPrefix(vmID))
	if err != nil {
		return nil, err
	}
	out := make([]store.VMDisk, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt vm disk index %q: %v", kv.Key, perr)
		}
		var d store.VMDisk
		found, gerr := s.c.GetJSON(ctx, vmDiskKey(id), &d)
		if gerr != nil {
			return nil, gerr
		}
		if !found || d.DeletedAt != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// beforeCursor reports whether (createdAt, id) sorts strictly before the DESC
// cursor tuple, matching `(created_at, id) < (cursor_created_at, cursor_id)`. A
// nil cursor (first page) admits every row.
func beforeCursor(createdAt time.Time, id uuid.UUID, cursorCreatedAt *time.Time, cursorID *uuid.UUID) bool {
	if cursorCreatedAt == nil || cursorID == nil {
		return true
	}
	if !createdAt.Equal(*cursorCreatedAt) {
		return createdAt.Before(*cursorCreatedAt)
	}
	return id.String() < cursorID.String()
}
