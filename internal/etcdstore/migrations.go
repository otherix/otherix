// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Migrations are written through one atomic transaction: the primary row, the
// per-VM active guard, the per-node and per-VM secondary indexes, and the
// backing task+job all commit together (CreateMigration). The per-node index
// leaf matches migrationsNodeIndexPrefix (nodes.go) so activeMigrationsOnNode
// resolves the rows this path writes; the read/cancel cascade lives in nodes.go.

// migrationActiveVMGuard is the at-most-one-active-migration-per-VM uniqueness
// key. A non-terminal migration holds it; CreateMigration's CAS on its
// CreateRevision==0 fails when a migration for the VM is already active.
func migrationActiveVMGuard(vmID uuid.UUID) string {
	return etcd.Key("uniq", "migration_active_vm", vmID.String())
}

// migrationNodeIndexKey is the per-node index leaf for a migration touching the
// node as source or target. Its prefix matches migrationsNodeIndexPrefix
// (nodes.go), and its value is the migration id, so activeMigrationsOnNode
// ranges the prefix and parses the value to resolve each primary.
func migrationNodeIndexKey(node, id uuid.UUID) string {
	return etcd.Key("index", "migrations", "node", node.String(), id.String())
}

// migrationVMIndexKey is the per-VM index leaf for a migration, valued with the
// migration id (the per-VM migration list reads it).
func migrationVMIndexKey(vm, id uuid.UUID) string {
	return etcd.Key("index", "migrations", "vm", vm.String(), id.String())
}

// migrationsVMIndexPrefix lists every migration for a VM (maintained by
// CreateMigration) - consumed by ListMigrations' VM filter, which reads each
// primary. Mirrors migrationsNodeIndexPrefix (nodes.go).
func migrationsVMIndexPrefix(vm uuid.UUID) string {
	return etcd.Key("index", "migrations", "vm", vm.String()) + "/"
}

// CreateMigration writes a pending migration row, its per-VM active guard, the
// per-node (source/target) and per-VM secondary indexes, and the backing
// task+job in one transaction. The CAS on the guard's CreateRevision==0 fails
// closed when the VM already has an active migration, returning
// store.ErrMigrationActiveExists. Source/target node index entries are written
// only for the node ids that are set.
func (s *Store) CreateMigration(ctx context.Context, p store.CreateMigrationParams, args queue.JobArgs) (store.Migration, error) {
	now := time.Now().UTC()
	m := store.Migration{
		ID:                p.ID,
		VmID:              p.VmID,
		SourceNodeID:      p.SourceNodeID,
		TargetNodeID:      p.TargetNodeID,
		InitiatedByUserID: p.InitiatedByUserID,
		Reason:            p.Reason,
		Phase:             store.MigrationPhasePending,
		MaxBandwidthBytes: p.MaxBandwidthBytes,
		MaxDowntimeMs:     p.MaxDowntimeMs,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	val, err := etcd.Marshal(m)
	if err != nil {
		return store.Migration{}, err
	}
	seq, jobOp, err := s.enqueueJobOp(ctx, args)
	if err != nil {
		return store.Migration{}, err
	}
	task := taskFromParams(p.Task, seq)
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return store.Migration{}, err
	}

	guard := migrationActiveVMGuard(p.VmID)
	ops := []clientv3.Op{
		clientv3.OpPut(guard, p.ID.String()),
		clientv3.OpPut(migrationKey(m.ID), string(val)),
		clientv3.OpPut(migrationVMIndexKey(p.VmID, m.ID), m.ID.String()),
		clientv3.OpPut(taskKey(task.ID), string(taskVal)),
		jobOp,
	}
	if p.SourceNodeID != nil {
		ops = append(ops, clientv3.OpPut(migrationNodeIndexKey(*p.SourceNodeID, m.ID), m.ID.String()))
	}
	if p.TargetNodeID != nil {
		ops = append(ops, clientv3.OpPut(migrationNodeIndexKey(*p.TargetNodeID, m.ID), m.ID.String()))
	}
	ops = append(ops, taskIndexOps(task)...)

	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.Migration{}, fmt.Errorf("create migration txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Migration{}, store.ErrMigrationActiveExists
	}
	return m, nil
}

// MigrationByID returns the migration row with the given id, or
// store.ErrNotFound. Migrations have no soft-delete column, so a present row is
// always visible.
func (s *Store) MigrationByID(ctx context.Context, id uuid.UUID) (store.Migration, error) {
	var m store.Migration
	found, err := s.c.GetJSON(ctx, migrationKey(id), &m)
	if err != nil {
		return store.Migration{}, err
	}
	if !found {
		return store.Migration{}, store.ErrNotFound
	}
	return m, nil
}

// ListMigrations returns migrations newest-first by (created_at, id), applying
// the optional VM or node filter (VM takes precedence), then the cursor and
// LimitCount. With a node filter it returns migrations touching the node as
// source OR target (CreateMigration indexes both). It honors LimitCount as
// given - the handler passes LimitCount+1 for next-page detection.
func (s *Store) ListMigrations(ctx context.Context, p store.ListMigrationsParams) ([]store.Migration, error) {
	var migs []store.Migration
	var err error
	switch {
	case p.VmID != nil:
		migs, err = s.migrationsByIndex(ctx, migrationsVMIndexPrefix(*p.VmID))
	case p.NodeID != nil:
		migs, err = s.migrationsByIndex(ctx, migrationsNodeIndexPrefix(*p.NodeID))
	default:
		migs, err = s.migrationsByPrimaryPrefix(ctx)
	}
	if err != nil {
		return nil, err
	}

	out := make([]store.Migration, 0, len(migs))
	for _, m := range migs {
		if !beforeCursor(m.CreatedAt, m.ID, p.CursorCreatedAt, p.CursorID) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if n := int(p.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// migrationsByIndex resolves each migration referenced by a secondary index
// prefix (per-VM or per-node), reading each primary. A dangling index entry
// (primary gone) is skipped.
func (s *Store) migrationsByIndex(ctx context.Context, prefix string) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]store.Migration, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt migration index %q: %v", kv.Key, perr)
		}
		m, gerr := s.MigrationByID(ctx, id)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				continue
			}
			return nil, gerr
		}
		out = append(out, m)
	}
	return out, nil
}

// migrationsByPrimaryPrefix ranges every migration primary row directly (the
// unfiltered list path).
func (s *Store) migrationsByPrimaryPrefix(ctx context.Context) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, etcd.Key("migrations")+"/")
	if err != nil {
		return nil, err
	}
	out := make([]store.Migration, 0, len(items))
	for _, kv := range items {
		var m store.Migration
		if err := json.Unmarshal(kv.Value, &m); err != nil {
			return nil, fmt.Errorf("unmarshal migration %q: %v", kv.Key, err)
		}
		out = append(out, m)
	}
	return out, nil
}
