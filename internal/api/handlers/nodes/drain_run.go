// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	migrations "github.com/otherix/otherix/internal/api/handlers/migrations"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// DrainWorkerStore is the store surface the drain saga needs.
type DrainWorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (bool, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	ListVMRefsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]store.NodeVMRef, error)
	ActiveSourceMigrationCount(ctx context.Context, nodeID uuid.UUID) (int, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error)
	ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error)
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
	CreateMigration(ctx context.Context, p store.CreateMigrationParams, args queue.JobArgs) (store.Migration, error)
	FinishNodeDrain(ctx context.Context, nodeID, taskID uuid.UUID, status store.TaskStatus, result []byte) error
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	DeleteDrainCancel(ctx context.Context, taskID uuid.UUID) error
	DrainCancelRequested(ctx context.Context, taskID uuid.UUID) (bool, error)
	DrainMaxConcurrentMigrations(ctx context.Context) (int32, error)
}

// Terminal drain failure codes, stored in the task result. Defined here (the
// saga is their only user) rather than in drain_jobs.go - the golangci `unused`
// linter is whole-program and would flag them as unused while only the contract
// file exists.
const (
	drainCodeTimeout         = "drain_timeout"
	drainCodeNodeUnreachable = "node_unreachable"
	// drainCodeReconciled marks a drain task the stuck-drain backstop finalized
	// because its backing job died (the saga never recorded its own outcome).
	drainCodeReconciled = "drain_reconciled"
)

// Placer decides a target for a VM without binding (read-only). It is the
// scheduler placer the migration saga already wraps; the drain saga reuses it
// for a dry-run target check before enqueuing a node-less migration. The alias
// (not a fresh interface) lets DrainHandler accept exactly what
// migrations.NewSchedulerPlacer returns, with no duplicate contract to keep in
// sync.
type Placer = migrations.Placer

// clock is the time seam the saga reads through so tests drive the deadline and
// dead-node staleness branches deterministically.
type clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// DrainConfig carries saga timing knobs.
type DrainConfig struct {
	PollInterval time.Duration // how often to re-poll the VM list; default 5s
	// DeadNodeGrace is how stale a node's last heartbeat may be before the drain
	// treats it as dead and finalizes (the heartbeat reconciler skips draining
	// nodes, so the saga owns liveness). Wire it from the reconciler's GoneGrace.
	DeadNodeGrace time.Duration
}

// DrainHandler returns the node.drain dispatcher handler. The placer is the
// scheduler placer (migrations.NewSchedulerPlacer in production); it backs the
// dry-run target check that gates each evacuation.
func DrainHandler(st DrainWorkerStore, placer Placer, cfg DrainConfig, log *slog.Logger) func(context.Context, []byte) error {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.DeadNodeGrace <= 0 {
		cfg.DeadNodeGrace = 2 * time.Minute
	}
	return func(ctx context.Context, raw []byte) error {
		var args NodeDrainRunArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal node.drain args: %v", err)
		}
		return runDrain(ctx, st, placer, realClock{}, cfg, log, args)
	}
}

// drainActive reports whether this job is still the active drain for the node.
// A node finalized (success/failed/cancelled) by a prior delivery is no longer
// draining and/or its DrainTaskID no longer points here.
func drainActive(node store.Node, taskID uuid.UUID) bool {
	return node.Status == store.NodeStatusDraining && node.DrainTaskID != nil && *node.DrainTaskID == taskID
}

func runDrain(ctx context.Context, st DrainWorkerStore, placer Placer, clk clock, cfg DrainConfig, log *slog.Logger, args NodeDrainRunArgs) error {
	// Pre-flight BEFORE UpdateTaskRunning. A `failed` task is NOT committed-terminal,
	// so UpdateTaskRunning would regress a finalized timed-out drain back to running
	// and re-run it. Guard on node state instead: if this drain already finalized
	// (or the node was force-deleted), do nothing - leave the task terminal.
	node, err := st.NodeByID(ctx, args.NodeID)
	if errors.Is(err, store.ErrNotFound) {
		// Node force-deleted mid-drain (DELETE ?force cancels our migrations and soft-
		// deletes the row). Nothing to flip; finalize the task only.
		return finalizeTaskOnly(ctx, st, args.TaskID, store.TaskStatusCancelled, DrainResult{Code: drainCodeNodeUnreachable})
	}
	if err != nil {
		return fmt.Errorf("load node: %v", err)
	}
	if !drainActive(node, args.TaskID) {
		return nil // already finalized/superseded by a prior delivery
	}
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, args.TaskID)
	if err != nil {
		return fmt.Errorf("update drain task running: %v", err)
	}
	if alreadyTerminal {
		return nil
	}

	initial := -1 // VM count on the first poll, for an honest `migrated` at finalize
	for {
		vms, done, err := drainEvaluate(ctx, st, clk, cfg, args, &initial)
		if done || err != nil {
			return err
		}

		if err := drainSweep(ctx, st, placer, args.NodeID, vms, log); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// drainEvaluate runs one poll's read-and-terminal-check pass: it reloads the
// node, samples the VM set, and finalizes the saga on any terminal condition
// (node gone, superseded, dead, cancelled, drained, timed out). When it
// finalizes (or hits an error) it returns done=true so the caller stops; on a
// non-terminal poll it returns the current VM set for the evacuation sweep.
// initial is the first-poll VM count, sampled here so `migrated` is honest at
// finalize.
func drainEvaluate(ctx context.Context, st DrainWorkerStore, clk clock, cfg DrainConfig, args NodeDrainRunArgs, initial *int) ([]store.NodeVMRef, bool, error) {
	node, err := st.NodeByID(ctx, args.NodeID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, true, finalizeTaskOnly(ctx, st, args.TaskID, store.TaskStatusCancelled,
			DrainResult{Code: drainCodeNodeUnreachable, Migrated: migratedFrom(*initial, 0)})
	}
	if err != nil {
		return nil, true, fmt.Errorf("load node: %v", err)
	}
	if !drainActive(node, args.TaskID) {
		return nil, true, nil
	}

	vms, err := st.ListVMRefsForNodeDeclared(ctx, args.NodeID)
	if err != nil {
		return nil, true, fmt.Errorf("list vms on node: %v", err)
	}
	if *initial < 0 {
		*initial = len(vms)
	}

	// An empty node IS drained: success takes priority over dead/cancel/timeout, so
	// a node whose last VM leaves exactly as its agent dies reports success, not
	// failed{node_unreachable}.
	if len(vms) == 0 {
		return nil, true, finalize(ctx, st, args, store.TaskStatusSuccess, DrainResult{Migrated: *initial, Remaining: 0})
	}

	// Liveness: the heartbeat reconciler SKIPS draining nodes (it never marks a
	// draining node unreachable/gone), so the saga must detect a dead node itself.
	// A node stale beyond DeadNodeGrace (the reconciler's gone threshold) is dead;
	// stop and cordon it (non-destructive) rather than burning the full timeout.
	if node.LastHeartbeatAt != nil && clk.Now().Sub(*node.LastHeartbeatAt) > cfg.DeadNodeGrace {
		inFlight, _ := st.ActiveSourceMigrationCount(ctx, args.NodeID) // informational metadata; ignore a read error
		return nil, true, finalize(ctx, st, args, store.TaskStatusFailed, DrainResult{
			Code: drainCodeNodeUnreachable, Remaining: len(vms), Migrated: migratedFrom(*initial, len(vms)), InFlight: inFlight,
		})
	}

	// The cancel read is best-effort: a transient error is dropped here and the
	// cancel is re-checked on the next poll.
	if cancelled, cerr := st.DrainCancelRequested(ctx, args.TaskID); cerr == nil && cancelled {
		return nil, true, finalize(ctx, st, args, store.TaskStatusCancelled, DrainResult{
			Migrated: migratedFrom(*initial, len(vms)), Remaining: len(vms),
		})
	}
	if clk.Now().Unix() >= args.DeadlineUnix {
		inFlight, _ := st.ActiveSourceMigrationCount(ctx, args.NodeID)
		stuck := make([]string, 0, len(vms))
		for _, v := range vms {
			stuck = append(stuck, v.Name)
		}
		return nil, true, finalize(ctx, st, args, store.TaskStatusFailed, DrainResult{
			Code: drainCodeTimeout, Migrated: migratedFrom(*initial, len(vms)),
			Remaining: len(vms), InFlight: inFlight, Stuck: stuck,
		})
	}

	return vms, false, nil
}

// drainSweep tries to evacuate each VM up to the concurrency cap. A VM with no
// target is left running (counted as remaining next poll); an already-migrating
// one is skipped. `migrated` is NOT incremented here - it is derived from
// initial-remaining at finalize, because an enqueued migration may still fail
// and leave the VM.
func drainSweep(ctx context.Context, st DrainWorkerStore, placer Placer, nodeID uuid.UUID, vms []store.NodeVMRef, log *slog.Logger) error {
	cap32, err := st.DrainMaxConcurrentMigrations(ctx)
	if err != nil {
		return fmt.Errorf("read drain concurrency cap: %v", err)
	}
	maxConc := int(cap32)
	active, err := st.ActiveSourceMigrationCount(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("count active migrations: %v", err)
	}
	for _, v := range vms {
		if active >= maxConc {
			break
		}
		enqueued, err := tryEvacuate(ctx, st, placer, nodeID, v, log)
		if err != nil {
			log.WarnContext(ctx, "drain: evacuate attempt failed", "vm", v.Name, "error", err)
			continue
		}
		if enqueued {
			active++
		}
	}
	return nil
}

// migratedFrom reports how many VMs left the node: initial count minus what
// remains, clamped to >= 0. Returns 0 when the initial count was never sampled.
func migratedFrom(initial, remaining int) int {
	if initial < 0 || initial < remaining {
		return 0
	}
	return initial - remaining
}

// finalizeTaskOnly finalizes the drain task without touching the node (used when
// the node was force-deleted mid-drain - there is no node row to flip). It also
// deletes the cooperative cancel marker: this path bypasses FinishNodeDrain
// (which owns the marker delete on the normal path), so the marker would leak
// without an explicit delete here. The delete is best-effort - a failure does not
// abort the finalize (the marker is a harmless content-free, fresh-UUID key).
func finalizeTaskOnly(ctx context.Context, st DrainWorkerStore, taskID uuid.UUID, status store.TaskStatus, res DrainResult) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal drain result: %v", err)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: status, Result: raw}); err != nil {
		return err
	}
	_ = st.DeleteDrainCancel(ctx, taskID)
	return nil
}

// tryEvacuate enqueues a node-less vm.migrate (reason=drain) for v IF a target
// exists (dry-run SchedulePlacement). Returns enqueued=false when there is no
// target (the VM stays, counted as remaining) or a migration is already active.
func tryEvacuate(ctx context.Context, st DrainWorkerStore, placer Placer, nodeID uuid.UUID, v store.NodeVMRef, log *slog.Logger) (bool, error) {
	vm, err := st.VMByID(ctx, v.ID)
	if err != nil {
		return false, fmt.Errorf("load vm %s: %v", v.Name, err)
	}
	// The VM keeps its pool name across a move; store.VM has no direct pool field,
	// so resolve the source pool the way the migrate saga does (boot disk -> pool).
	poolName, err := sourcePoolName(ctx, st, vm.ID)
	if err != nil {
		return false, fmt.Errorf("resolve source pool for vm %s: %v", v.Name, err)
	}
	// Build the migration directly node-less: the vm.migrate saga does the real
	// placement+bind under the placement lock (avoiding oversubscription). We do a
	// cheap pre-check there is SOME target before enqueuing, to avoid dead pending
	// migrations - but the precheck is just go/no-go, not a binding. NOTE: the
	// precheck and the saga's real bind are separate evaluations, so a target that
	// existed here can fail to bind later; that migration then sits `pending`,
	// holding the per-VM guard (the migrate saga's documented retry behaviour). That
	// is acceptable (visible, recoverable, fail-toward-inaction) - drain does not
	// guarantee zero pending migrations, only that it never enqueues for a VM with
	// NO target at all.
	if !hasEligibleTarget(ctx, st, placer, nodeID, vm, poolName) {
		return false, nil
	}

	migID := uuid.New()
	taskID := uuid.New()
	src := nodeID
	_, err = st.CreateMigration(ctx, store.CreateMigrationParams{
		ID:           migID,
		VmID:         vm.ID,
		SourceNodeID: &src,
		Reason:       store.MigrationReasonDrain,
		Live:         true,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.migrate", Status: store.TaskStatusPending,
			ResourceType: "migration", ResourceID: &migID, MaxAttempts: 25,
		},
	}, migrations.MigrationRunArgs{TaskID: taskID, MigrationID: migID})
	if errors.Is(err, store.ErrMigrationActiveExists) {
		return false, nil // already migrating; leave it
	}
	if err != nil {
		return false, fmt.Errorf("enqueue drain migration: %v", err)
	}
	return true, nil
}

// hasEligibleTarget is the dry-run target check: it mirrors the migrate saga's
// placement-request construction minus the bind, calling the placer with the
// source node excluded. A nil error means SOME node can host the VM; the
// precheck fails closed (skip this loop, retry next poll) on a read error.
func hasEligibleTarget(ctx context.Context, st DrainWorkerStore, placer Placer, nodeID uuid.UUID, vm store.VM, poolName string) bool {
	nics, err := st.ListVMNicsByVM(ctx, vm.ID)
	if err != nil {
		return false // fail-closed for the precheck: skip this loop, retry next
	}
	seen := map[uuid.UUID]struct{}{}
	netIDs := make([]uuid.UUID, 0, len(nics))
	for _, n := range nics {
		if _, ok := seen[n.NetworkID]; ok {
			continue
		}
		seen[n.NetworkID] = struct{}{}
		netIDs = append(netIDs, n.NetworkID)
	}
	_, err = placer.Place(ctx, scheduler.PlacementRequest{
		PoolName:      poolName,
		ExcludeNodeID: &nodeID,
		VCPUs:         int(vm.CpuCores),
		MemoryMiB:     int(vm.MemoryMib),
		NetworkIDs:    netIDs,
	})
	return err == nil
}

// sourcePoolName resolves the pool the VM currently lives in: store.VM has no
// pool field, so the boot disk's storage pool is the source of truth (the same
// resolution the migrate saga uses).
func sourcePoolName(ctx context.Context, st DrainWorkerStore, vmID uuid.UUID) (string, error) {
	disks, err := st.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		return "", fmt.Errorf("list vm disks: %v", err)
	}
	if len(disks) == 0 {
		return "", fmt.Errorf("vm %s has no boot disk", vmID)
	}
	pool, err := st.StoragePoolByID(ctx, disks[0].StoragePoolID)
	if err != nil {
		return "", fmt.Errorf("resolve source pool %s: %v", disks[0].StoragePoolID, err)
	}
	return pool.Name, nil
}

func finalize(ctx context.Context, st DrainWorkerStore, args NodeDrainRunArgs, status store.TaskStatus, res DrainResult) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal drain result: %v", err)
	}
	return st.FinishNodeDrain(ctx, args.NodeID, args.TaskID, status, raw)
}
