// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Firmwares are a bounded, cluster-wide collection addressed by UUID with two
// uniqueness guards mirroring the SQL partial indexes: uq_firmwares_name_arch
// (name + architecture) and uq_firmwares_default (one default per architecture +
// type, only when is_default). Firmwares hard-delete (no deleted_at); delete is
// blocked when vms still reference the row.

func firmwareKey(id uuid.UUID) string { return etcd.Key("firmwares", id.String()) }

func firmwarePrefix() string { return etcd.Key("firmwares") + "/" }

func firmwareNameArchGuard(arch store.CPUArch, name string) string {
	return etcd.Key("uniq", "firmwares", "name_arch", string(arch), strings.ToLower(name))
}

func firmwareDefaultGuard(arch store.CPUArch, ftype store.FirmwareType) string {
	return etcd.Key("uniq", "firmwares", "default", string(arch), string(ftype))
}

// firmwareVMIndexPrefix is the prefix under which vms record their firmware
// reference (written by the vm slice). DeleteFirmware counts the keys here to
// block deletion.
func firmwareVMIndexPrefix(id uuid.UUID) string {
	return etcd.Key("index", "vms", "firmware", id.String()) + "/"
}

// FirmwareByID returns the firmware with the given id, or store.ErrNotFound.
func (s *Store) FirmwareByID(ctx context.Context, id uuid.UUID) (store.Firmware, error) {
	var f store.Firmware
	found, err := s.c.GetJSON(ctx, firmwareKey(id), &f)
	if err != nil {
		return store.Firmware{}, err
	}
	if !found {
		return store.Firmware{}, store.ErrNotFound
	}
	return f, nil
}

// DefaultFirmwareForArchType returns the default firmware for the
// (architecture, type) pair via the default guard, or store.ErrNotFound.
func (s *Store) DefaultFirmwareForArchType(ctx context.Context, arch store.CPUArch, ftype store.FirmwareType) (store.Firmware, error) {
	idBytes, found, err := s.c.Get(ctx, firmwareDefaultGuard(arch, ftype))
	if err != nil {
		return store.Firmware{}, err
	}
	if !found {
		return store.Firmware{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(idBytes))
	if err != nil {
		return store.Firmware{}, fmt.Errorf("corrupt firmware default guard: %v", err)
	}
	return s.FirmwareByID(ctx, id)
}

// CreateFirmware inserts a firmware, stamping created_at/updated_at and writing
// the name+arch guard (plus the default guard when is_default) atomically.
// Returns store.ErrFirmwareNameExists or store.ErrFirmwareDefaultExists on the
// respective uniqueness conflict.
func (s *Store) CreateFirmware(ctx context.Context, arg store.CreateFirmwareParams) (store.Firmware, error) {
	now := time.Now().UTC()
	f := store.Firmware{
		ID:           arg.ID,
		Name:         arg.Name,
		Architecture: arg.Architecture,
		Type:         arg.Type,
		Version:      arg.Version,
		SecureBoot:   arg.SecureBoot,
		IsDefault:    arg.IsDefault,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	val, err := etcd.Marshal(f)
	if err != nil {
		return store.Firmware{}, err
	}
	nameGuard := firmwareNameArchGuard(f.Architecture, f.Name)
	conds := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(nameGuard), "=", 0)}
	ops := []clientv3.Op{
		clientv3.OpPut(nameGuard, f.ID.String()),
		clientv3.OpPut(firmwareKey(f.ID), string(val)),
	}
	if f.IsDefault {
		defGuard := firmwareDefaultGuard(f.Architecture, f.Type)
		conds = append(conds, clientv3.Compare(clientv3.CreateRevision(defGuard), "=", 0))
		ops = append(ops, clientv3.OpPut(defGuard, f.ID.String()))
	}
	resp, err := s.c.Raw().Txn(ctx).If(conds...).Then(ops...).Commit()
	if err != nil {
		return store.Firmware{}, fmt.Errorf("create firmware txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Firmware{}, s.firmwareConflict(ctx, f.Architecture, f.Name, f.Type, true, f.IsDefault)
	}
	return f, nil
}

// UpdateFirmware rewrites the mutable fields (name, version, secure_boot,
// is_default; architecture and type are immutable), bumps updated_at, and moves
// the name guard / default guard to match. Returns store.ErrNotFound when the
// row is missing, or the uniqueness sentinels on conflict.
func (s *Store) UpdateFirmware(ctx context.Context, arg store.UpdateFirmwareParams) (store.Firmware, error) {
	existing, err := s.FirmwareByID(ctx, arg.ID)
	if err != nil {
		return store.Firmware{}, err
	}
	updated := existing
	updated.Name = arg.Name
	updated.Version = arg.Version
	updated.SecureBoot = arg.SecureBoot
	updated.IsDefault = arg.IsDefault
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.Firmware{}, err
	}

	var conds []clientv3.Cmp
	ops := []clientv3.Op{clientv3.OpPut(firmwareKey(arg.ID), string(val))}

	oldNameGuard := firmwareNameArchGuard(existing.Architecture, existing.Name)
	newNameGuard := firmwareNameArchGuard(existing.Architecture, arg.Name)
	if oldNameGuard != newNameGuard {
		conds = append(conds, clientv3.Compare(clientv3.CreateRevision(newNameGuard), "=", 0))
		ops = append(ops, clientv3.OpDelete(oldNameGuard), clientv3.OpPut(newNameGuard, arg.ID.String()))
	}

	defGuard := firmwareDefaultGuard(existing.Architecture, existing.Type)
	switch {
	case !existing.IsDefault && arg.IsDefault:
		conds = append(conds, clientv3.Compare(clientv3.CreateRevision(defGuard), "=", 0))
		ops = append(ops, clientv3.OpPut(defGuard, arg.ID.String()))
	case existing.IsDefault && !arg.IsDefault:
		ops = append(ops, clientv3.OpDelete(defGuard))
	}

	if len(conds) == 0 {
		if err := s.c.Put(ctx, firmwareKey(arg.ID), val); err != nil {
			return store.Firmware{}, err
		}
		return updated, nil
	}
	resp, err := s.c.Raw().Txn(ctx).If(conds...).Then(ops...).Commit()
	if err != nil {
		return store.Firmware{}, fmt.Errorf("update firmware txn: %v", err)
	}
	if !resp.Succeeded {
		renamed := oldNameGuard != newNameGuard
		return store.Firmware{}, s.firmwareConflict(ctx, existing.Architecture, arg.Name, existing.Type, renamed, !existing.IsDefault && arg.IsDefault)
	}
	return updated, nil
}

// firmwareConflict resolves a failed create/update txn into the specific
// uniqueness sentinel by probing only the guards the txn actually asserted
// absent: checkedName means a name+arch guard was being claimed (create, or a
// rename), checkedDefault means the default guard was being claimed. Probing
// only the asserted guards avoids mis-blaming a guard the row already owns (an
// update that keeps its name still has a live name guard).
func (s *Store) firmwareConflict(ctx context.Context, arch store.CPUArch, name string, ftype store.FirmwareType, checkedName, checkedDefault bool) error {
	if checkedName {
		if _, found, err := s.c.Get(ctx, firmwareNameArchGuard(arch, name)); err != nil {
			return err
		} else if found {
			return store.ErrFirmwareNameExists
		}
	}
	if checkedDefault {
		if _, found, err := s.c.Get(ctx, firmwareDefaultGuard(arch, ftype)); err != nil {
			return err
		} else if found {
			return store.ErrFirmwareDefaultExists
		}
	}
	return store.ErrFirmwareNameExists
}

// ListFirmwares returns firmwares matching the optional architecture/type
// filters, ordered by (created_at, id) ascending, after the cursor, capped at
// LimitCount.
func (s *Store) ListFirmwares(ctx context.Context, arg store.ListFirmwaresParams) ([]store.Firmware, error) {
	items, err := s.c.Range(ctx, firmwarePrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.Firmware, 0, len(items))
	for _, kv := range items {
		var f store.Firmware
		if err := json.Unmarshal(kv.Value, &f); err != nil {
			return nil, fmt.Errorf("unmarshal firmware %q: %v", kv.Key, err)
		}
		if arg.Architecture != nil && f.Architecture != *arg.Architecture {
			continue
		}
		if arg.Type != nil && f.Type != *arg.Type {
			continue
		}
		if !afterCursor(f.CreatedAt, f.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		out = append(out, f)
	}
	sortByCreatedAtID(out, func(f store.Firmware) (time.Time, uuid.UUID) { return f.CreatedAt, f.ID })
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// DeleteFirmware hard-deletes the firmware after verifying it exists and is
// unreferenced. Returns store.ErrNotFound when missing, or
// *store.ResourceInUseError (key "vms") when referenced.
func (s *Store) DeleteFirmware(ctx context.Context, id uuid.UUID) error {
	f, err := s.FirmwareByID(ctx, id)
	if err != nil {
		return err
	}
	// Set the delete-intent FIRST so no new VM can reference this firmware past
	// this point (CreateVM guards on firmwareDeletingKey), then count - the count
	// is authoritative. The finalize CASes on our intent rev so a reaper sweep or
	// a racing delete cannot re-open the window. See deleting_intent.go.
	intentKey := firmwareDeletingKey(id)
	myRev, err := s.setDeleteIntent(ctx, intentKey, time.Now())
	if err != nil {
		return err
	}
	vmCount, err := s.countPrefix(ctx, firmwareVMIndexPrefix(id))
	if err != nil {
		return err
	}
	if vmCount > 0 {
		s.clearDeleteIntent(ctx, intentKey, myRev)
		return &store.ResourceInUseError{Resources: map[string]int64{"vms": vmCount}}
	}
	ops := []clientv3.Op{
		clientv3.OpDelete(firmwareKey(id)),
		clientv3.OpDelete(firmwareNameArchGuard(f.Architecture, f.Name)),
		clientv3.OpDelete(intentKey),
	}
	if f.IsDefault {
		ops = append(ops, clientv3.OpDelete(firmwareDefaultGuard(f.Architecture, f.Type)))
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(deleteIntentGuard(intentKey, myRev)).
		Then(ops...).
		Commit()
	if err != nil {
		return fmt.Errorf("delete firmware txn: %v", err)
	}
	if !resp.Succeeded {
		// Our intent was severed (reaper on a hung delete, or a racing delete that
		// finalized first). If the row is already gone, treat as done (idempotent);
		// otherwise ask the caller to retry rather than delete past a lapsed guard.
		if _, gerr := s.FirmwareByID(ctx, id); errors.Is(gerr, store.ErrNotFound) {
			return store.ErrNotFound
		}
		return store.ErrConcurrentUpdate
	}
	return nil
}
