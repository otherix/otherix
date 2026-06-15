// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// errTargetPoolNotReady signals that the bound target node does not (yet) have
// the migration's pool. It is retryable-as-pending (D4): the VM stays on
// source and the migration waits for the pool to appear, rather than failing.
var errTargetPoolNotReady = errors.New("target pool not ready on node")

// Run-form migration worker for the etcd job runtime. It drains a vm.migrate job
// and drives the migration to a terminal outcome against the agent two-phase
// handshake, implementing spec D2 (node-less placement), D3 (PinnedNodeID stays
// = source until the atomic cutover), D4 (pending retry-forever), and the
// Crash-semantics fail-safe-to-source rule (every pre-cutover failure leaves the
// VM on its source).
//
// The orchestration is keyed off the SOURCE outgoing task: the worker prepares
// the target (StartIncomingMigration), starts the source push (StartOutgoingMigration
// with agent_task_id resumption), then polls the source outgoing task to a
// terminal status. A terminal-success commits the atomic cutover; a
// terminal-failure marks the migration failed without ever re-pinning the VM.

// MigrationWorkerStore is the storage surface the migration worker depends on:
// task-lifecycle mutators, entity reads, and the migration transitions
// (placement bind, progress / terminal, atomic cutover). *etcdstore.Store
// satisfies it.
type MigrationWorkerStore interface {
	AcquirePlacementLock(ctx context.Context, lockKey int64) error
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	UpdateTaskAgentTaskID(ctx context.Context, arg store.UpdateTaskAgentTaskIDParams) error
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	MigrationByID(ctx context.Context, id uuid.UUID) (store.Migration, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	BindMigrationTarget(ctx context.Context, migID, targetNodeID uuid.UUID, poolName string) error
	UpdateMigrationProgress(ctx context.Context, migID uuid.UUID, upd store.MigrationProgressUpdate) error
	CommitMigrationCutover(ctx context.Context, migID uuid.UUID) error
	ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error)
	StoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error)
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
}

// MigrationAgentClient is the narrow agent-call seam the worker drives the
// two-phase peer-to-peer handshake through. *agentclient.Client satisfies it
// structurally; tests inject a fake.
type MigrationAgentClient interface {
	StartIncomingMigration(ctx context.Context, endpoint, vmName string, req agentapi.MigrationIncomingRequest) (agentapi.MigrationIncomingResponse, error)
	StartOutgoingMigration(ctx context.Context, endpoint, vmName string, req agentapi.MigrationOutgoingRequest) (string, error)
	PollTask(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error)
	// StartVMOnTarget starts the migrated guest on the target node after a
	// committed cutover (when the VM's desired phase is running).
	StartVMOnTarget(ctx context.Context, endpoint, vmName string) error
	// DeleteVMOnSource deletes the source's now-stale copy after a committed
	// cutover. Best-effort: a failure leaks a disk (recoverable), never destroys
	// a wanted VM.
	DeleteVMOnSource(ctx context.Context, endpoint, vmName string) error
}

// Placer is the placement seam (spec D2): a node-less migrate scores a target
// via the same scheduler.SchedulePlacement path used at create, excluding the
// source node. The production implementation wraps SchedulePlacement over the
// store's PlacementQuerier; tests inject a fake.
type Placer interface {
	Place(ctx context.Context, req scheduler.PlacementRequest) (scheduler.PlacementDecision, error)
}

// MigrateConfig threads the worker's non-placement knobs. DefaultPoolName is the
// cluster default the worker falls back to when a migration carries no explicit
// TargetPoolName (the placement algorithm + resource gating live on the Placer).
type MigrateConfig struct {
	DefaultPoolName string
}

// schedulerPlacer is the production Placer: it runs scheduler.SchedulePlacement
// against the store's read-only placement querier. The worker (not this type)
// holds store.LockKeyPlacement across the read-availability -> bind window so
// concurrent node-less migrations serialize their target choice; see placeAndBind.
type schedulerPlacer struct {
	q   scheduler.Querier
	cfg scheduler.PlacementConfig
}

// NewSchedulerPlacer returns the production Placer wrapping SchedulePlacement
// over q with the given config.
func NewSchedulerPlacer(q scheduler.Querier, cfg scheduler.PlacementConfig) Placer {
	return schedulerPlacer{q: q, cfg: cfg}
}

func (p schedulerPlacer) Place(ctx context.Context, req scheduler.PlacementRequest) (scheduler.PlacementDecision, error) {
	return scheduler.SchedulePlacement(ctx, p.q, req, p.cfg)
}

// MigrateHandler returns the dispatcher handler for vm.migrate jobs.
func MigrateHandler(st MigrationWorkerStore, agent MigrationAgentClient, placer Placer, cfg MigrateConfig, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args MigrationRunArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.migrate args: %v", err)
		}
		return runMigration(ctx, st, agent, placer, cfg, log, args)
	}
}

func runMigration(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, placer Placer, cfg MigrateConfig, log *slog.Logger, args MigrationRunArgs) error {
	taskID := args.TaskID
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// Redelivery whose task already committed success/cancelled: do NOT contact
		// the agent. Return nil so the dispatcher CompleteJob-deletes the job.
		return nil
	}

	m, err := st.MigrationByID(ctx, args.MigrationID)
	if err != nil {
		return failTask(ctx, st, log, taskID, "not_found", fmt.Errorf("load migration: %v", err))
	}
	if isTerminalPhase(m.Phase) {
		// Already terminal (a prior delivery committed the cutover, or it was
		// cancelled/failed): idempotent reconcile-by-query. But a crash between the
		// migration terminal write (cutover / fail / cancel) and the task finalize
		// can leave the backing task stuck running forever - clients polling
		// /v1/tasks/{id} would never see terminal, and retention never reaps a
		// non-terminal task. Reconcile the task to the migration outcome before
		// returning. UpdateTaskRunning above already returned (alreadyTerminal) for a
		// committed-terminal task (success/cancelled), so a redelivery whose task is
		// already finalized never reaches here - no double-finalize. A failed task is
		// NOT committed-terminal, so it does reach here; finalizing it to match the
		// migration is the correction.
		return finalizeForTerminalMigration(ctx, st, log, taskID, m)
	}
	if m.SourceNodeID == nil {
		// A migration with no source is malformed - it can never be driven. Fail
		// terminally rather than burning the retry budget.
		return failTerminal(ctx, st, log, taskID, "internal", fmt.Errorf("migration %s has no source node", m.ID))
	}

	vm, err := st.VMByID(ctx, m.VmID)
	if err != nil {
		return failTask(ctx, st, log, taskID, "not_found", fmt.Errorf("load vm: %v", err))
	}

	// An omitted --pool defaults to the source VM's pool name (correction A):
	// a VM keeps its pool name across a move. placeAndBind (node-less) and the
	// explicit-node handshake both consume m.TargetPoolName below.
	if m.TargetPoolName == "" {
		spn, err := sourcePoolName(ctx, st, m.VmID)
		if err != nil {
			// Retryable: the VM is still on source, nothing durable moved.
			return failTask(ctx, st, log, taskID, "internal", fmt.Errorf("default target pool: %v", err))
		}
		m.TargetPoolName = spn
	}

	// Spec D2: node-less migrate picks a target via the scheduler (excluding the
	// source), then binds it. A bound migration skips straight to the handshake.
	if m.TargetNodeID == nil {
		bound, perr := placeAndBind(ctx, st, placer, cfg, log, m, vm)
		if perr != nil {
			return perr
		}
		m = bound
	}

	return driveHandshake(ctx, st, agent, log, taskID, m, vm)
}

// placeAndBind scores a target for a node-less migration and binds it. On an
// unschedulable verdict it records the retryable scheduling_reason on the still-
// pending migration and returns a RETRYABLE error so the dispatcher requeues
// (spec D4: retry-forever, VM stays on source) - the agent is NEVER contacted.
// On success it binds the target and returns the reloaded migration.
func placeAndBind(ctx context.Context, st MigrationWorkerStore, placer Placer, cfg MigrateConfig, log *slog.Logger, m store.Migration, vm store.VM) (store.Migration, error) {
	// Hold store.LockKeyPlacement across the read-availability -> bind decision
	// window (placer.Place reads candidate availability; BindMigrationTarget pins
	// the choice). It mirrors the vm.create path, which holds the same lock across
	// SchedulePlacement + bind, and closes the TOCTOU where two concurrent node-less
	// migrations both score the same target before either binds and oversubscribe
	// it (Task-7 reservation only counts ALREADY-bound targets). The lock serializes
	// this selection->bind window across replicas; it does NOT guard the cutover
	// re-pin (that is its own atomic txn). On the single-node default it is a no-op
	// (etcd writes are linearizable); see store.LockKeyPlacement. There is no
	// explicit release because the no-op stub holds nothing; the HA implementation
	// will scope the etcd lock's lifetime to this transaction span when it lands.
	if err := st.AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
		// A lock-acquire failure is retryable: the VM is still on source.
		return store.Migration{}, fmt.Errorf("acquire placement lock: %v", err)
	}

	// TargetPoolName is pre-defaulted to the source VM's pool name in
	// runMigration; placeAndBind no longer falls back to cfg.DefaultPoolName.
	poolName := m.TargetPoolName
	src := *m.SourceNodeID
	decision, perr := placer.Place(ctx, scheduler.PlacementRequest{
		PoolName:      poolName,
		ExcludeNodeID: &src,
		VCPUs:         int(vm.CpuCores),
		MemoryMiB:     int(vm.MemoryMib),
	})
	if perr != nil {
		// RETRYABLE: record the pending envelope and return the cause so the
		// dispatcher requeues and the scheduler retry loop drives the next attempt.
		// The agent is not contacted.
		//
		// SLICE-1 LIMITATION (spec D4 retry-forever not yet fully honored): a pending
		// migration is re-driven ONLY by the vm.migrate job retry budget
		// (workerMaxAttempts = 25). Once that budget is exhausted the job is dropped
		// and the migration stays pending forever with no further attempts - the
		// opposite of D4 "retries forever". The VM stays safe on its source node
		// meanwhile (nothing durable moved; the agent was never contacted), BUT the
		// per-VM active-migration guard stays HELD while the migration is pending: a
		// dropped-pending migration leaves the VM un-migratable (every future
		// CreateMigration returns 409 ErrMigrationActiveExists) until an operator
		// `migration cancel` (or vm stop/delete) releases the guard via the terminal
		// transition. A periodic pending-migration re-driver (mirroring the
		// vms.schedule loop, which requeues unscheduled VMs indefinitely) is required
		// to honor D4 and is tracked in ROADMAP; it is out of scope for slice 1.
		return store.Migration{}, recordPending(ctx, st, log, m.ID, scheduleReasonFor(perr), perr)
	}

	winnerPool := decision.PoolInstance.Name
	if err := st.BindMigrationTarget(ctx, m.ID, decision.Node.ID, winnerPool); err != nil {
		if errors.Is(err, store.ErrMigrationTerminal) {
			// Raced to terminal (cancel / lifecycle supersede) between read and
			// bind: idempotent reconcile, nothing to do.
			return store.Migration{}, nil
		}
		// ErrConcurrentUpdate or a transient store error is retryable.
		return store.Migration{}, fmt.Errorf("bind migration target: %v", err)
	}
	reloaded, err := st.MigrationByID(ctx, m.ID)
	if err != nil {
		return store.Migration{}, fmt.Errorf("reload migration after bind: %v", err)
	}
	return reloaded, nil
}

// driveHandshake runs the two-phase peer-to-peer handshake against a bound
// migration and polls the SOURCE outgoing task to terminal. On success it commits
// the atomic cutover (re-pin source -> target); on failure it marks the migration
// failed WITHOUT ever re-pinning (fail-safe-to-source).
func driveHandshake(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, taskID uuid.UUID, m store.Migration, vm store.VM) error {
	if m.TargetNodeID == nil {
		// Defensive: a bound migration always has a target. A nil here is a bug, not
		// a retryable condition.
		return failTerminal(ctx, st, log, taskID, "internal", fmt.Errorf("migration %s has no target after bind", m.ID))
	}
	source, err := st.NodeByID(ctx, *m.SourceNodeID)
	if err != nil {
		return failTask(ctx, st, log, taskID, "not_found", fmt.Errorf("load source node: %v", err))
	}
	target, err := st.NodeByID(ctx, *m.TargetNodeID)
	if err != nil {
		return failTask(ctx, st, log, taskID, "not_found", fmt.Errorf("load target node: %v", err))
	}

	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	agentTaskID, err := startOrResume(ctx, st, agent, log, taskID, task, vm, m, source, target)
	if err != nil {
		if errors.Is(err, errTargetPoolNotReady) {
			// The bound target lacks the migration's pool (correction B): record
			// pending and requeue (D4), VM stays on source, agent never contacted.
			return recordPending(ctx, st, log, m.ID, ReasonPoolNotReady, err)
		}
		// A handshake-setup error (incoming prep, outgoing start) is retryable: the
		// VM is still on source, nothing durable moved. Record failed-as-retryable
		// envelope and return the cause so the dispatcher requeues against the
		// attempt budget.
		return failTask(ctx, st, log, taskID, ErrCodeTargetUnreachable, err)
	}

	// Advance the migration to active as the source push proceeds, then poll the
	// source outgoing task to terminal.
	advancePhase(ctx, st, log, m.ID, store.MigrationPhaseActive)

	terminal, perr := agent.PollTask(ctx, source.AdvertisedEndpoint, agentTaskID)
	if perr != nil {
		return failTask(ctx, st, log, taskID, ErrCodeTargetUnreachable, fmt.Errorf("poll outgoing task: %v", perr))
	}

	switch terminal.Status {
	case "success":
		if err := commitCutover(ctx, st, log, taskID, m.ID, terminal); err != nil {
			return err
		}
		// Post-cutover convergence. Reached ONLY after CommitMigrationCutover
		// returned nil (a committed cutover - a fail-closed input). Best-effort:
		// a failure here leaks (recoverable), never destroys, and must NOT fail
		// the already-committed migration. DeleteVMOnSource is unreachable on any
		// failed / aborted / pending path - it lives strictly on the success arm
		// after the commit.
		convergePostCutover(ctx, agent, log, m.ID, vm, source, target, m.Live)
		return nil
	case "failed", "cancelled":
		return failMigration(ctx, st, log, taskID, m.ID, terminal)
	default:
		return failTask(ctx, st, log, taskID, "internal", fmt.Errorf("unexpected agent terminal status %q", terminal.Status))
	}
}

// convergePostCutover runs the best-effort post-cutover steps: start the guest
// on the target when its desired phase is running AND the migration was offline,
// then delete the source's stale copy. Both are best-effort and reached ONLY
// after a committed cutover; neither failure fails the (already committed)
// migration - a start failure leaves the guest stopped on target, a delete
// failure leaks the source disk. Leak, never destroy.
// The start is gated on !live: for a LIVE migration the target QEMU already
// resumed the guest itself at switchover, so a start here would be a
// no-op-or-error. For an OFFLINE migration the target adopted a stopped VM and
// the CP starts it.
// Known limitation: when the migrated VM's desired phase is NOT running (a cold
// migration that stays stopped on the target), no start is dispatched, so the
// agent's start-path teardown of the incoming qemu-nbd (releaseIncomingNBD) does
// not run here - the target's reserved migration port and the idle qemu-nbd
// (still holding the disk write lock) are reclaimed lazily on the VM's first
// start or on agent restart. A leak, never a destroy; acceptable for this slice
// since offline migration overwhelmingly targets running VMs.
func convergePostCutover(ctx context.Context, agent MigrationAgentClient, log *slog.Logger, migID uuid.UUID, vm store.VM, source, target store.Node, live bool) {
	if vm.DesiredPhase == store.VmDesiredPhaseRunning && !live {
		if err := agent.StartVMOnTarget(ctx, target.AdvertisedEndpoint, vm.Name); err != nil {
			log.WarnContext(ctx, "post-cutover start on target failed",
				slog.String("migration_id", migID.String()), slog.String("target", target.AdvertisedEndpoint), slog.String("error", err.Error()))
		}
	}
	if err := agent.DeleteVMOnSource(ctx, source.AdvertisedEndpoint, vm.Name); err != nil {
		log.WarnContext(ctx, "post-cutover source cleanup failed (disk leaked)",
			slog.String("migration_id", migID.String()), slog.String("source", source.AdvertisedEndpoint), slog.String("error", err.Error()))
	}
}

// startOrResume performs the two-phase handshake setup with agent_task_id
// resumption on the OUTGOING start. When the task already carries an agent task
// id (a redelivery after the source push began), it skips BOTH agent POSTs and
// resumes polling the persisted id - a redelivered job must not double-POST the
// outgoing start. Otherwise it prepares the target (incoming), starts the source
// push (outgoing), and persists the returned id.
func startOrResume(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, taskID uuid.UUID, task store.Task, vm store.VM, m store.Migration, source, target store.Node) (uuid.UUID, error) {
	if task.AgentTaskID != nil {
		return *task.AgentTaskID, nil
	}

	advancePhase(ctx, st, log, m.ID, store.MigrationPhaseSetup)

	spec, disks, err := incomingVMSpec(ctx, st, m, vm)
	if err != nil {
		return uuid.Nil, err
	}

	mode := agentapi.MigrationIncomingRequestMode(agentapi.MigrationModeLive)
	if !m.Live {
		mode = agentapi.MigrationIncomingRequestMode(agentapi.MigrationModeOffline)
	}
	userData, networkConfig := migrationCloudInit(vm)
	incoming, err := agent.StartIncomingMigration(ctx, target.AdvertisedEndpoint, vm.Name, agentapi.MigrationIncomingRequest{
		MigrationID:        m.ID,
		Mode:               mode,
		SourceNodeIdentity: ptrString(sourceIdentity(source.Name)),
		VMSpec:             spec,
		Disks:              &disks,
		UserData:           userData,
		NetworkConfig:      networkConfig,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("start incoming migration: %v", err)
	}

	outMode := agentapi.MigrationOutgoingRequestMode(agentapi.MigrationModeLive)
	if !m.Live {
		outMode = agentapi.MigrationOutgoingRequestMode(agentapi.MigrationModeOffline)
	}
	agentTaskStr, err := agent.StartOutgoingMigration(ctx, source.AdvertisedEndpoint, vm.Name, agentapi.MigrationOutgoingRequest{
		MigrationID:        m.ID,
		Mode:               outMode,
		TargetEndpoint:     incoming.ListenEndpoint,
		TargetNodeIdentity: ptrString(targetIdentity(target.Name)),
		AuthToken:          incoming.AuthToken,
		MaxBandwidthBytes:  m.MaxBandwidthBytes,
		MaxDowntimeMs:      int32PtrToInt(m.MaxDowntimeMs),
		// Relay the target's NBD disk-export listener to the source so a live push
		// dials the right endpoint. Nil for offline migrations (the target leaves it
		// unset and the single RAM endpoint doubles as the qemu-nbd server).
		NbdEndpoint: incoming.NbdEndpoint,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("start outgoing migration: %v", err)
	}
	agentTaskID, perr := uuid.Parse(agentTaskStr)
	if perr != nil {
		return uuid.Nil, fmt.Errorf("parse agent task id %q: %v", agentTaskStr, perr)
	}
	if err := st.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &agentTaskID}); err != nil {
		return uuid.Nil, fmt.Errorf("persist agent_task_id: %v", err)
	}
	return agentTaskID, nil
}

// commitCutover commits the atomic source -> target re-pin. ErrConcurrentUpdate
// triggers a reconcile-by-query: re-read the migration; if a concurrent writer
// already completed it (the cutover landed), treat as success. Then finalize the
// task success.
func commitCutover(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, taskID, migID uuid.UUID, terminal agentclient.TaskTerminal) error {
	err := st.CommitMigrationCutover(ctx, migID)
	if errors.Is(err, store.ErrConcurrentUpdate) {
		reloaded, rerr := st.MigrationByID(ctx, migID)
		if rerr != nil {
			return fmt.Errorf("reload migration after cutover CAS loss: %v", rerr)
		}
		if reloaded.Phase != store.MigrationPhaseCompleted {
			// Lost the CAS to a non-completing writer (progress update). Retry the
			// whole drive: the migration is still mid-flight on source.
			return fmt.Errorf("cutover CAS lost for migration %s (phase %q); retry", migID, reloaded.Phase)
		}
		// Already completed by a concurrent commit: idempotent success.
	} else if err != nil {
		// A non-CAS cutover error (terminal migration, store fault) is retryable so
		// the worker reconciles on the next delivery.
		return fmt.Errorf("commit cutover: %v", err)
	}

	result := terminal.Result
	if result == nil {
		result = map[string]any{"migration_id": migID.String()}
	}
	resultJSON, merr := json.Marshal(result)
	if merr != nil {
		resultJSON = []byte(`{}`)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON}); err != nil {
		return fmt.Errorf("finalize task success: %v", err)
	}
	log.InfoContext(ctx, "migration completed (cutover committed)", slog.String("migration_id", migID.String()))
	return nil
}

// failMigration marks the migration failed from the source task's terminal-failure
// envelope WITHOUT ever calling cutover - the VM stays on source (fail-safe). It
// finalizes the task failed and returns nil so the dispatcher does NOT requeue
// (the failure is terminal; the source push reported a definitive failure).
func failMigration(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, taskID, migID uuid.UUID, terminal agentclient.TaskTerminal) error {
	msg := "migration failed on source agent"
	code := ErrCodeConvergenceFailed
	if terminal.Error != nil {
		if terminal.Error.Message != "" {
			msg = terminal.Error.Message
		}
		if terminal.Error.Code != "" {
			code = terminal.Error.Code
		}
	}
	failedPhase := store.MigrationPhaseFailed
	errMsg := msg
	if uerr := st.UpdateMigrationProgress(ctx, migID, store.MigrationProgressUpdate{
		Phase:        &failedPhase,
		ErrorMessage: &errMsg,
	}); uerr != nil {
		// A migration-write failure is retryable so the terminal state eventually
		// persists.
		return fmt.Errorf("mark migration failed: %v", uerr)
	}
	if err := finalizeFailed(ctx, st, log, taskID, code, errors.New(msg)); err != nil {
		return err
	}
	log.WarnContext(ctx, "migration failed pre-cutover (vm stays on source)",
		slog.String("migration_id", migID.String()), slog.String("code", code), slog.String("error", msg))
	return nil
}

// finalizeForTerminalMigration reconciles a still-non-terminal backing task to a
// migration that is already terminal (the redelivery-after-crash window between
// the migration terminal write and the task finalize). It maps the migration
// phase to the task's terminal status: completed -> success, failed -> failed,
// cancelled -> cancelled. The caller has established the migration is terminal
// and that UpdateTaskRunning did NOT report alreadyTerminal, so the task is not
// committed-terminal (it cannot be success/cancelled) - this is the first and
// only finalize on this delivery, never a double-finalize. Returns nil so the
// dispatcher CompleteJob-deletes the job; a finalize-write error is wrapped and
// returned (retryable) so the envelope eventually persists.
func finalizeForTerminalMigration(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, taskID uuid.UUID, m store.Migration) error {
	switch m.Phase {
	case store.MigrationPhaseCompleted:
		result := []byte(fmt.Sprintf(`{"migration_id":%q}`, m.ID.String()))
		if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: result}); err != nil {
			return fmt.Errorf("finalize task success for completed migration %s: %v", m.ID, err)
		}
		log.InfoContext(ctx, "reconciled dangling task to success for completed migration",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
	case store.MigrationPhaseCancelled:
		if err := finalizeTask(ctx, st, taskID, store.TaskStatusCancelled, ErrCodeMigrationCancelled, m.ErrorMessage); err != nil {
			return fmt.Errorf("finalize task cancelled for cancelled migration %s: %v", m.ID, err)
		}
		log.InfoContext(ctx, "reconciled dangling task to cancelled for cancelled migration",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
	default: // store.MigrationPhaseFailed
		if err := finalizeTask(ctx, st, taskID, store.TaskStatusFailed, ErrCodeConvergenceFailed, m.ErrorMessage); err != nil {
			return fmt.Errorf("finalize task failed for failed migration %s: %v", m.ID, err)
		}
		log.WarnContext(ctx, "reconciled dangling task to failed for failed migration",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
	}
	return nil
}

// finalizeTask writes a non-success terminal envelope (failed / cancelled) onto
// the task, carrying the given error code and the migration's recorded message
// (falling back to a generic message when absent).
func finalizeTask(ctx context.Context, st MigrationWorkerStore, taskID uuid.UUID, status store.TaskStatus, code string, msg *string) error {
	m := "migration " + string(status)
	if msg != nil && *msg != "" {
		m = *msg
	}
	envelope, merr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: m})
	if merr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
	}
	return st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: status, Error: envelope})
}

// advancePhase best-effort advances the migration to phase. A CAS loss / terminal
// migration is benign (a concurrent cancel or a prior advance); progress phase is
// observational and never gates correctness, so the error is logged and swallowed.
func advancePhase(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, migID uuid.UUID, phase store.MigrationPhase) {
	if err := st.UpdateMigrationProgress(ctx, migID, store.MigrationProgressUpdate{Phase: &phase}); err != nil {
		if errors.Is(err, store.ErrMigrationTerminal) || errors.Is(err, store.ErrConcurrentUpdate) {
			return
		}
		log.WarnContext(ctx, "advance migration phase failed (continuing)",
			slog.String("migration_id", migID.String()), slog.String("phase", string(phase)), slog.String("error", err.Error()))
	}
}

// incomingVMSpec builds the VMSpec the target agent's StartIncoming handler
// requires. It threads the VM identity (name, architecture, cpu, memory) AND a
// single boot disk carrying the two fields the agent actually reads:
// Disks[0].SizeGib (to size the destination disk - the under-size guard that
// makes the source-side qemu-img push fail if too small) and
// Disks[0].StoragePoolPath (the destination pool's filesystem root on the
// target node, which the agent maps back to a configured pool). The bound
// migration's TargetPoolName + TargetNodeID resolve that path. The disk's
// virtual size is the VM's boot-disk row size; the guest itself comes up from
// the in-band migration stream, not from this spec.
func incomingVMSpec(ctx context.Context, st MigrationWorkerStore, m store.Migration, vm store.VM) (agentapi.VMSpec, []agentapi.MigrationDisk, error) {
	disks, err := st.ListVMDisksByVM(ctx, vm.ID)
	if err != nil {
		return agentapi.VMSpec{}, nil, fmt.Errorf("list vm disks: %v", err)
	}
	if len(disks) == 0 {
		return agentapi.VMSpec{}, nil, fmt.Errorf("vm %s has no boot disk", vm.ID)
	}
	boot := disks[0]

	poolPath, err := targetPoolPath(ctx, st, m)
	if err != nil {
		return agentapi.VMSpec{}, nil, err
	}

	spec := agentapi.VMSpec{
		VMUUID:       vm.ID,
		Name:         vm.Name,
		Architecture: agentapi.VMSpecArchitecture(vm.Architecture),
		CPUCores:     int(vm.CpuCores),
		MemoryMib:    int64(vm.MemoryMib),
		Disks: []agentapi.VMSpecDisk{{
			DeviceOrder:     int(boot.DeviceOrder),
			Bus:             agentapi.VMSpecDiskBus(boot.Bus),
			Format:          agentapi.VMSpecDiskFormat(boot.Format),
			SizeGib:         int(boot.SizeGib),
			StoragePoolPath: poolPath,
			Source:          agentapi.VMSpecDiskSource{Kind: agentapi.VMSpecDiskSourceKind(boot.SourceKind)},
		}},
	}
	manifest := migrationDisks(vm, int64(boot.SizeGib)*gibBytes, string(boot.Format))
	return spec, manifest, nil
}

// gibBytes is the GiB->bytes multiplier. The boot disk row advertises its size
// in GiB; the migration manifest size_bytes is the virtual byte size. Matches
// the agent handler's gibBytes (internal/agent/handlers/vms/migrations.go) so
// the manifest size and the target's boot-disk sizing agree.
const gibBytes = 1 << 30

// cloudinitDefaultDiskSize is the fixed virtual size of every NoCloud cidata
// ISO. It MUST equal cloudinit.DefaultDiskSize (the agent's builder always
// creates the ISO at this size and the VM-create path never overrides it); a
// drift-guard test asserts the equality. Duplicated as a const here to avoid a
// control-plane -> agent package import.
const cloudinitDefaultDiskSize int64 = 10 * 1024 * 1024

// vmHasCidata reports whether the VM has a NoCloud cidata ISO disk. It MUST
// mirror resolveCloudInitUserData / resolveCloudInitNetworkConfig in package
// internal/api/handlers/vms (the create-time source of truth the agent's
// needsCidata consumes): cidata exists iff cloud-init is enabled and at least
// one channel is set. If those resolvers change, change this too.
func vmHasCidata(vm store.VM) bool {
	if vm.CloudInitDisabled {
		return false
	}
	return (vm.UserData != nil && *vm.UserData != "") ||
		(vm.NetworkConfig != nil && *vm.NetworkConfig != "")
}

// migrationCloudInit returns the cloud-init blobs the target needs to rebuild
// the read-only cidata ISO, or (nil, nil) when the VM has no cidata seed. The
// gate MUST match vmHasCidata (the same source of truth as the cidata disk in
// the manifest): when the VM has cidata, vm.UserData / vm.NetworkConfig hold the
// resolved blobs the agent built the seed from at create time.
func migrationCloudInit(vm store.VM) (userData, networkConfig *string) {
	if !vmHasCidata(vm) {
		return nil, nil
	}
	return vm.UserData, vm.NetworkConfig
}

// migrationDisks builds the ordered disk manifest the target replicates: the
// boot disk (index 0, the VM's real boot-disk format and size), then the
// fixed-size raw cidata ISO (index 1) when the VM has cloud-init. The set is
// deterministic from desired config, mirroring how the agent builds a VM's
// disks at create time.
func migrationDisks(vm store.VM, bootSizeBytes int64, bootFormat string) []agentapi.MigrationDisk {
	disks := []agentapi.MigrationDisk{{
		Index:     0,
		SizeBytes: bootSizeBytes,
		Format:    agentapi.MigrationDiskFormat(bootFormat),
		ReadOnly:  false,
	}}
	if vmHasCidata(vm) {
		disks = append(disks, agentapi.MigrationDisk{
			Index:     1,
			SizeBytes: cloudinitDefaultDiskSize,
			Format:    agentapi.MigrationDiskFormat("raw"),
			ReadOnly:  true,
		})
	}
	return disks
}

// targetPoolPath resolves the bound migration's TargetPoolName to the storage
// pool's absolute filesystem root on the TARGET node. Pool names are scoped per
// (node_id, name), so the name index can return rows on several nodes; the row
// on m.TargetNodeID is the destination. A missing pool on the target is NOT a
// hard failure: it wraps errTargetPoolNotReady so the caller records the
// migration pending (D4, retryable) and waits for the pool to appear, rather
// than failing the migration.
func targetPoolPath(ctx context.Context, st MigrationWorkerStore, m store.Migration) (string, error) {
	if m.TargetNodeID == nil {
		return "", fmt.Errorf("migration %s has no target node; cannot resolve pool path", m.ID)
	}
	pools, err := st.StoragePoolsByName(ctx, m.TargetPoolName)
	if err != nil {
		return "", fmt.Errorf("resolve target pool %q: %v", m.TargetPoolName, err)
	}
	for _, p := range pools {
		if p.NodeID == *m.TargetNodeID {
			return p.Path, nil
		}
	}
	return "", fmt.Errorf("%w: pool %q on node %s", errTargetPoolNotReady, m.TargetPoolName, *m.TargetNodeID)
}

// sourcePoolName returns the storage-pool NAME the VM's boot disk lives in on
// the source node. An omitted target pool defaults to this so a VM keeps its
// pool name across a move (resolved to the target node's instance of that
// name), rather than silently landing in the cluster default pool.
func sourcePoolName(ctx context.Context, st MigrationWorkerStore, vmID uuid.UUID) (string, error) {
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

// recordPending writes the retryable pending envelope (phase=pending +
// scheduling_reason) on a migration that cannot bind/advance right now, and
// returns a retryable error so the dispatcher requeues. The VM stays on
// source; the agent is never contacted.
func recordPending(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, migID uuid.UUID, reason string, cause error) error {
	now := time.Now().UTC()
	pending := store.MigrationPhasePending
	if uerr := st.UpdateMigrationProgress(ctx, migID, store.MigrationProgressUpdate{
		Phase:                 &pending,
		SchedulingReason:      &reason,
		LastScheduleAttemptAt: &now,
	}); uerr != nil {
		// A progress-write failure is itself retryable; surface it so the envelope
		// eventually persists.
		return fmt.Errorf("record scheduling reason: %v (cause: %v)", uerr, cause)
	}
	log.InfoContext(ctx, "migration pending",
		slog.String("migration_id", migID.String()), slog.String("scheduling_reason", reason))
	return fmt.Errorf("migration %s pending: %v", migID, cause)
}

// sourceIdentity is the source node leaf-cert DN used for the target's
// tls-authz pin (Subject is CN-only "node-<name>", per internal/auth/csr.go).
func sourceIdentity(nodeName string) string { return "CN=node-" + nodeName }

// targetIdentity is the target node SAN name used for the source's
// tls-hostname pin.
func targetIdentity(nodeName string) string { return "node-" + nodeName + ".agents.otherix.local" }

// ptrString returns a pointer to s, for the optional *string wire fields.
func ptrString(s string) *string { return &s }

// scheduleReasonFor maps a placement error to the migration's machine-readable
// scheduling_reason (mirrors the VM schedule loop). Insufficient-resources maps
// to no_capacity; every other unschedulable verdict (no eligible node, pool not
// found / not on node) maps to no_eligible_target - the operator-visible "leave
// this node" intent is unsatisfiable right now.
func scheduleReasonFor(err error) string {
	switch {
	case errors.Is(err, scheduler.ErrInsufficientResources):
		return ReasonNoCapacity
	case errors.Is(err, scheduler.ErrPoolNotFound), errors.Is(err, scheduler.ErrPoolNotOnNode):
		return ReasonPoolNotReady
	default:
		return ReasonNoEligibleTarget
	}
}

// isTerminalPhase reports whether a migration phase is committed-terminal
// (completed) or terminal-failure (failed / cancelled). The worker treats all
// three as "nothing to drive".
func isTerminalPhase(p store.MigrationPhase) bool {
	switch p {
	case store.MigrationPhaseCompleted, store.MigrationPhaseFailed, store.MigrationPhaseCancelled:
		return true
	default:
		return false
	}
}

// int32PtrToInt converts a *int32 (store width) to the *int the agent request
// type expects, preserving nil.
func int32PtrToInt(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// failTask writes the failed envelope and returns the cause so the dispatcher
// RETRIES against the attempt budget (the failure may be transient - target
// blip, store fault). A finalize-write error preempts the cause and is returned
// so the envelope persists.
func failTask(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, taskID uuid.UUID, code string, cause error) error {
	if err := finalizeFailed(ctx, st, log, taskID, code, cause); err != nil {
		return err
	}
	return cause
}

// failTerminal writes the failed envelope and returns nil so the dispatcher
// COMPLETES (deletes) the job - use it for conditions that cannot become
// satisfiable on retry (a malformed migration with no source/target).
func failTerminal(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, taskID uuid.UUID, code string, cause error) error {
	return finalizeFailed(ctx, st, log, taskID, code, cause)
}

// finalizeFailed writes the terminal failed envelope for taskID. nil on success;
// a wrapped error when the finalize write itself failed (the caller retries so
// the envelope eventually persists).
func finalizeFailed(ctx context.Context, st MigrationWorkerStore, log *slog.Logger, taskID uuid.UUID, code string, cause error) error {
	envelope, merr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: cause.Error()})
	if merr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		log.ErrorContext(ctx, "vm.migrate marshal error envelope failed", slog.String("task_id", taskID.String()), slog.String("code", code))
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusFailed, Error: envelope}); err != nil {
		log.ErrorContext(ctx, "vm.migrate finalize-failed write failed", slog.String("task_id", taskID.String()), slog.String("code", code), slog.String("error", err.Error()))
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return nil
}
