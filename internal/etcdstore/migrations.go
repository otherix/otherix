// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"
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
