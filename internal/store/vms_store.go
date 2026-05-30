// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/queue"
)

// The methods in this file are the VM domain entrypoints on *Store:
// they wrap the raw sqlc-generated *Queries primitives, translate
// driver errors into store sentinels, and assemble the placement-locked
// scheduled-create transaction. Handlers depend on these, not on
// *Queries.

// ErrVMNameInUse is returned when a VM with the same name already
// exists (uq_vms_name, case-insensitive).
var ErrVMNameInUse = errors.New("store: vm name already in use")

// translateVMNameUnique maps the VM name-uniqueness violation to
// ErrVMNameInUse; any other error passes through.
func translateVMNameUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_vms_name" {
		return ErrVMNameInUse
	}
	return err
}

// VMRuntimeByID returns the observed runtime row for a VM, or
// ErrNotFound when the worker has not upserted one yet.
func (s *Store) VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (VMRuntime, error) {
	rt, err := s.queries.GetVMRuntime(ctx, vmID)
	if err != nil {
		return VMRuntime{}, translateNoRows(err)
	}
	return rt, nil
}

// ListVMDisksByVM returns the disks attached to a VM, ordered by device
// slot.
func (s *Store) ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]VMDisk, error) {
	return s.queries.ListVMDisksByVM(ctx, vmID)
}

// ListVMs returns VMs matching the cursor-paginated filters in arg.
func (s *Store) ListVMs(ctx context.Context, arg ListVMsParams) ([]VM, error) {
	return s.queries.ListVMs(ctx, arg)
}

// UpdateVMRuntimePhase writes the observed runtime phase for a VM. Used
// by the synchronous lifecycle ops (pause / resume / reset) after the
// agent confirms the transition.
func (s *Store) UpdateVMRuntimePhase(ctx context.Context, arg UpdateVMRuntimePhaseParams) error {
	return s.queries.UpdateVMRuntimePhase(ctx, arg)
}

// PlacementReader is the transaction-scoped read surface a VM placement
// decision needs: the cluster-wide advisory lock plus the scheduler's
// candidate-enumeration queries. *Queries satisfies it. Its method set
// is a superset of the scheduler's own Querier interface, so a
// PlacementReader value is assignable to scheduler.Querier - the handler
// hands the tx-bound reader straight to the scheduler and the store
// never imports the scheduler package (which would be an import cycle).
type PlacementReader interface {
	AcquirePlacementLock(ctx context.Context, lockKey int64) error
	ListEligiblePoolsByName(ctx context.Context, name string) ([]ListEligiblePoolsByNameRow, error)
	ListMemoryPressuredCandidatesByName(ctx context.Context, name string) ([]ListMemoryPressuredCandidatesByNameRow, error)
	ListSystemDiskPressuredCandidatesByName(ctx context.Context, name string) ([]ListSystemDiskPressuredCandidatesByNameRow, error)
	ListDiskPressuredPoolsByName(ctx context.Context, name string) ([]ListDiskPressuredPoolsByNameRow, error)
	ListStoragePoolsByName(ctx context.Context, name string) ([]StoragePool, error)
	CountRunningVMsByNode(ctx context.Context, nodeID *uuid.UUID) (int64, error)
}

// VMCreateWrites bundles the rows a scheduled VM create persists. The
// handler builds them from the placement decision (returned by the plan
// callback); the store writes them inside the locked transaction.
type VMCreateWrites struct {
	VM   CreateVMParams
	Disk CreateVMDiskParams
	Task CreateTaskParams
	Job  queue.JobArgs
}

// CreateScheduledVM runs the atomic placement-locked critical section
// for a VM create: it opens one transaction, hands the tx-bound
// PlacementReader to plan (which acquires the advisory lock, scores
// candidates with the scheduler, and builds the vms / vm_disks / tasks
// rows + job args from the chosen instance), persists those rows,
// enqueues the background job, and stamps the job reference onto the
// task. The advisory lock releases on commit/rollback, so the
// scheduler's reads and the pinned-node write share a single
// transaction - concurrent api-server replicas cannot double-allocate.
// A uq_vms_name violation surfaces as ErrVMNameInUse; plan's own errors
// (scheduler sentinels, pool-not-writable) propagate verbatim. Returns
// the task id on success.
func (s *Store) CreateScheduledVM(ctx context.Context, plan func(PlacementReader) (VMCreateWrites, error)) (uuid.UUID, error) {
	var taskID uuid.UUID
	err := s.InTxEnqueue(ctx, func(q *Queries, enq queue.Enqueuer) error {
		writes, err := plan(q)
		if err != nil {
			return err
		}
		if _, err := q.CreateVM(ctx, writes.VM); err != nil {
			return err
		}
		if _, err := q.CreateVMDisk(ctx, writes.Disk); err != nil {
			return err
		}
		if _, err := q.CreateTask(ctx, writes.Task); err != nil {
			return err
		}
		ref, err := enq.Enqueue(ctx, writes.Job)
		if err != nil {
			return err
		}
		jobID := ref.ID
		if err := q.UpdateTaskRiverJobID(ctx, UpdateTaskRiverJobIDParams{
			ID:         writes.Task.ID,
			RiverJobID: &jobID,
		}); err != nil {
			return err
		}
		taskID = writes.Task.ID
		return nil
	})
	if err != nil {
		return uuid.Nil, translateVMNameUnique(err)
	}
	return taskID, nil
}
