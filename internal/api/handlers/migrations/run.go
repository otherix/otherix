// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// errTargetPoolNotReady signals that the bound target node does not (yet) have
// the migration's pool. It is retryable-as-pending: the VM stays on
// source and the migration waits for the pool to appear, rather than failing.
var errTargetPoolNotReady = errors.New("target pool not ready on node")

// Run-form migration worker for the etcd job runtime. It drains a vm.migrate job
// and drives the migration to a terminal outcome against the agent two-phase
// handshake, implementing node-less placement, PinnedNodeID staying
// = source until the atomic cutover, pending retry-forever, and the
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
	AcquirePlacementLock(ctx context.Context, lockKey int64) (func(), error)
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
	UpdateMigrationStats(ctx context.Context, migID uuid.UUID, stats store.MigrationStats) error
	ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error)
	ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error)
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	// OverlayPeerNodesForVM returns the nodes with a local VM on any overlay VNI
	// this VM is attached to, excluding the VM's own current node - the FDB peer
	// set the worker nudges after a cutover (fast-push).
	OverlayPeerNodesForVM(ctx context.Context, vmID uuid.UUID) ([]store.Node, error)
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
	// NudgeHeartbeat triggers an immediate heartbeat on the agent at endpoint
	// (CP fast-push after an overlay cutover so peers re-pull their FDB without
	// waiting for the next periodic tick). Best-effort.
	NudgeHeartbeat(ctx context.Context, endpoint string) error
	// VM fetches the agent's view of a VM (used to read the source boot disk's
	// real virtual size for destination sizing).
	VM(ctx context.Context, endpoint, vmName string) (agentclient.AgentVM, error)
	// CancelMigration tells an agent (the bound TARGET on a terminal-failure /
	// cancel of a live migration) to reap its incoming setup. Best-effort.
	CancelMigration(ctx context.Context, endpoint, vmName, migrationID string) (agentapi.Migration, error)
	// GetMigration reads an agent's migration record by migration_id (read-only).
	// Used to arbitrate a cancelled migration whose agent_task_id was never
	// persisted (CP crash between the outgoing POST and the persist): a 404
	// AgentError means the source truly never started, any 200 carries the
	// source's phase to arbitrate on.
	GetMigration(ctx context.Context, endpoint, vmName, migrationID string) (agentapi.Migration, error)
}

// Placer is the placement seam: a node-less migrate scores a target
// via the same scheduler.SchedulePlacement path used at create, excluding the
// source node. The production implementation wraps SchedulePlacement over the
// store's PlacementQuerier; tests inject a fake.
type Placer interface {
	Place(ctx context.Context, req scheduler.PlacementRequest) (scheduler.PlacementDecision, error)
}

// defaultMigrationMaxPollDuration bounds how long the worker will loop-poll a
// single outgoing task before freeing its dispatcher slot with a retryable
// requeue. It is deliberately generous - comfortably longer than any real
// migration and the agent's live ConvergenceTimeout - so it fires ONLY on a
// genuinely stuck source task (e.g. a hung offline qemu-img convert, which has
// no source-side timeout), never on a healthy long migration.
const defaultMigrationMaxPollDuration = 24 * time.Hour

// MigrateConfig threads the worker's non-placement knobs. DefaultPoolName is the
// cluster default the worker falls back to when a migration carries no explicit
// TargetPoolName (the placement algorithm + resource gating live on the Placer).
// MaxPollDuration bounds the outgoing-task loop-poll (zero -> the default); Now
// is the clock seam for that bound (nil -> time.Now), overridden only by tests.
type MigrateConfig struct {
	DefaultPoolName string
	MaxPollDuration time.Duration
	Now             func() time.Time
}

// maxPollDuration returns the configured loop-poll bound or the default.
func (c MigrateConfig) maxPollDuration() time.Duration {
	if c.MaxPollDuration > 0 {
		return c.MaxPollDuration
	}
	return defaultMigrationMaxPollDuration
}

// now returns the configured clock or time.Now.
func (c MigrateConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
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
		return finalizeForTerminalMigration(ctx, st, agent, log, taskID, m)
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

	// An omitted --pool defaults to the source VM's pool name:
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

	// A node-less migrate picks a target via the scheduler (excluding the
	// source), then binds it. A bound migration skips straight to the handshake.
	if m.TargetNodeID == nil {
		bound, perr := placeAndBind(ctx, st, placer, cfg, log, m, vm)
		if perr != nil {
			return perr
		}
		m = bound
	}

	return driveHandshake(ctx, st, agent, cfg, log, taskID, m, vm)
}

// placeAndBind scores a target for a node-less migration and binds it. On an
// unschedulable verdict it records the retryable scheduling_reason on the still-
// pending migration and returns a RETRYABLE error so the dispatcher requeues
// (retry-forever, VM stays on source) - the agent is NEVER contacted.
// On success it binds the target and returns the reloaded migration.
func placeAndBind(ctx context.Context, st MigrationWorkerStore, placer Placer, cfg MigrateConfig, log *slog.Logger, m store.Migration, vm store.VM) (store.Migration, error) {
	// Hold store.LockKeyPlacement across the read-availability -> bind decision
	// window (placer.Place reads candidate availability; BindMigrationTarget pins
	// the choice). It closes the TOCTOU where two concurrent node-less migrations
	// both score the same target before either binds and co-locate (reservation
	// only counts ALREADY-bound targets). On the single-control-plane default this
	// is a process-local keyed mutex; the HA path takes an etcd lock keyed by
	// lockKey with the same contract. The defer below releases it after the bind
	// commits; it does NOT guard the cutover re-pin (that is its own atomic txn).
	release, err := st.AcquirePlacementLock(ctx, store.LockKeyPlacement)
	if err != nil {
		// A lock-acquire failure is retryable: the VM is still on source.
		return store.Migration{}, fmt.Errorf("acquire placement lock: %v", err)
	}
	defer release()

	// TargetPoolName is pre-defaulted to the source VM's pool name in
	// runMigration; placeAndBind no longer falls back to cfg.DefaultPoolName.
	poolName := m.TargetPoolName
	src := *m.SourceNodeID

	// Pass the VM's network ids so the scheduler's network-readiness filter
	// excludes any candidate where a requested bridge network is
	// not reconciled-ready, exactly as vm create does. Otherwise a node-less
	// migration could land on a node whose bridge is not ready and materialiseNICs
	// would fail on the target. Empty netIDs (a NIC-less VM) keeps the filter a
	// no-op, so such migrations are unaffected.
	vmNics, err := st.ListVMNicsByVM(ctx, vm.ID)
	if err != nil {
		return store.Migration{}, fmt.Errorf("list vm nics: %v", err)
	}
	seen := make(map[uuid.UUID]struct{}, len(vmNics))
	netIDs := make([]uuid.UUID, 0, len(vmNics))
	for _, n := range vmNics {
		if _, ok := seen[n.NetworkID]; ok {
			continue
		}
		seen[n.NetworkID] = struct{}{}
		netIDs = append(netIDs, n.NetworkID)
	}

	decision, perr := placer.Place(ctx, scheduler.PlacementRequest{
		PoolName:      poolName,
		ExcludeNodeID: &src,
		VCPUs:         int(vm.CpuCores),
		MemoryMiB:     int(vm.MemoryMib),
		NetworkIDs:    netIDs,
	})
	if perr != nil {
		// RETRYABLE: record the pending envelope and return the cause so the
		// dispatcher requeues and the scheduler retry loop drives the next attempt.
		// The agent is not contacted.
		//
		// KNOWN LIMITATION (retry-forever not yet fully honored): a pending
		// migration is re-driven ONLY by the vm.migrate job retry budget
		// (workerMaxAttempts = 25). Once that budget is exhausted the job is dropped
		// and the migration stays pending forever with no further attempts - the
		// opposite of "retries forever". The VM stays safe on its source node
		// meanwhile (nothing durable moved; the agent was never contacted), BUT the
		// per-VM active-migration guard stays HELD while the migration is pending: a
		// dropped-pending migration leaves the VM un-migratable (every future
		// CreateMigration returns 409 ErrMigrationActiveExists) until an operator
		// `migration cancel` (or vm stop/delete) releases the guard via the terminal
		// transition. A periodic pending-migration re-driver (mirroring the
		// vms.schedule loop, which requeues unscheduled VMs indefinitely) is required
		// to honor retry-forever and is tracked in ROADMAP.
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
func driveHandshake(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, cfg MigrateConfig, log *slog.Logger, taskID uuid.UUID, m store.Migration, vm store.VM) error {
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
			// The bound target lacks the migration's pool: record
			// pending and requeue, VM stays on source, agent never contacted.
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

	terminal, perr := pollOutgoing(ctx, st, agent, cfg, log, taskID, m, vm, source, agentTaskID)
	if perr != nil {
		return perr
	}
	if terminal == nil {
		// The poll classified the result into a terminal/retryable outcome and
		// already finalized (or requeued); nothing more for this delivery.
		return nil
	}
	t := *terminal

	switch t.Status {
	case "success":
		if err := commitCutover(ctx, st, log, taskID, m.ID, t); err != nil {
			return err
		}
		// Post-cutover convergence. Reached ONLY after CommitMigrationCutover
		// returned nil (a committed cutover - a fail-closed input). Best-effort:
		// a failure here leaks (recoverable), never destroys, and must NOT fail
		// the already-committed migration. DeleteVMOnSource is unreachable on any
		// failed / aborted / pending path - it lives strictly on the success arm
		// after the commit.
		convergePostCutover(ctx, st, agent, log, m.ID, vm, source, target, m.Live)
		return nil
	case "failed", "cancelled":
		// The source task went terminal. Distinguish two causes by reloading the
		// migration: a CP-side operator cancel (we aborted the source, so the
		// migration is ALREADY terminal-cancelled) vs a genuine source failure (the
		// migration is still non-terminal). For the already-terminal case,
		// failMigration would call UpdateMigrationProgress(failed) -> ErrMigrationTerminal
		// -> a wasted retry and a misleading "migration failed" log; instead finalize
		// the backing task to MATCH the terminal migration (cancelled -> cancelled) and
		// reap the bound target's incoming (idempotent). Only a still-non-terminal
		// migration takes the genuine-failure path (fail-safe-to-source).
		if reloaded, rerr := st.MigrationByID(ctx, m.ID); rerr == nil && isTerminalPhase(reloaded.Phase) {
			return finalizeForTerminalMigration(ctx, st, agent, log, taskID, reloaded)
		}
		cancelTargetIncoming(ctx, agent, log, m, vm, target)
		return failMigration(ctx, st, log, taskID, m.ID, t)
	default:
		return failTask(ctx, st, log, taskID, "internal", fmt.Errorf("unexpected agent terminal status %q", t.Status))
	}
}

// pollOutgoing polls the source outgoing task to terminal, classifying every poll
// error so a healthy long migration is never failed for a mere poll-budget
// timeout (#4) and a vanished source task (agent restart) is arbitrated on
// positive target evidence rather than blindly re-driven (#5). It returns:
//
//   - (&terminal, nil) when the agent task reached a terminal status; the caller
//     runs the success/failed/cancelled switch.
//   - (nil, err) when it has already resolved this delivery here: err is the value
//     the worker must return - nil when a terminal/requeue was finalized, or a
//     retryable / propagating error otherwise.
//
// The loop-poll runs under the dispatcher's job-lease renewer (which renews for
// the handler's whole lifetime), so a multi-hour migration never loses its lease.
// It is bounded by cfg.maxPollDuration so a genuinely stuck source task (a hung
// offline convert has no source-side timeout) frees the dispatcher slot via a
// retryable requeue instead of pinning it forever.
func pollOutgoing(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, cfg MigrateConfig, log *slog.Logger, taskID uuid.UUID, m store.Migration, vm store.VM, source store.Node, agentTaskID uuid.UUID) (*agentclient.TaskTerminal, error) {
	deadline := cfg.now().Add(cfg.maxPollDuration())
	for {
		terminal, perr := agent.PollTask(ctx, source.AdvertisedEndpoint, agentTaskID)
		if perr == nil {
			return &terminal, nil
		}

		var te *agentclient.TimeoutError
		if errors.As(perr, &te) {
			// Poll budget elapsed, the agent task is still alive. Keep polling - do
			// NOT finalize, do NOT consume an attempt - unless the overall budget is
			// spent, in which case free the dispatcher slot with a RETRYABLE requeue
			// (the migration stays active; redelivery resumes via agent_task_id).
			if cfg.now().After(deadline) {
				log.WarnContext(ctx, "migration poll budget elapsed; requeueing to free the worker slot",
					slog.String("migration_id", m.ID.String()), slog.String("budget", cfg.maxPollDuration().String()))
				// Return a bare RETRYABLE error (do NOT finalize the task): the
				// migration is healthy and still active, so writing a failed envelope
				// would only flicker the task failed->running on redelivery. The
				// dispatcher requeues (attempt bump) and the next delivery resumes the
				// poll via the persisted agent_task_id.
				return nil, fmt.Errorf("migration %s poll budget %s elapsed; requeueing", m.ID, cfg.maxPollDuration())
			}
			continue
		}
		if errors.Is(perr, context.Canceled) {
			// Graceful shutdown cancelled the run ctx. Return the ctx error WITHOUT
			// finalizing the task: the dispatcher's shutdown arm requeues to pending
			// with NO attempt bump and the next boot resumes via the persisted
			// agent_task_id. Finalizing here would durably mark the task failed on a
			// mere deploy.
			return nil, perr
		}
		var ae *agentclient.AgentError
		if errors.As(perr, &ae) && ae.Status == http.StatusNotFound {
			// The source agent lost the in-memory task - a source restart is the only
			// deleter of an agent task, and it wipes the in-memory migration record
			// with it. Do NOT blind re-POST (destructive re-drive) and do NOT probe
			// the source (its record is gone regardless of how far the push got).
			// Arbitrate on positive TARGET evidence instead.
			return nil, reconcileVanishedSourceTask(ctx, st, agent, log, taskID, m, vm)
		}
		// Genuine transient poll failure (5xx budget-exhausted, network): retryable,
		// consume an attempt (unchanged pre-hardening behaviour).
		return nil, failTask(ctx, st, log, taskID, ErrCodeTargetUnreachable,
			fmt.Errorf("poll outgoing task: %v", perr))
	}
}

// reconcileVanishedSourceTask arbitrates an outgoing task that 404s mid-poll -
// reachable essentially only by a source-agent restart, which erases BOTH the
// in-memory agent task AND the in-memory migration record. Because the source
// record is gone whether or not the live push already handed off, its ABSENCE is
// NOT evidence the migration failed: the source may have completed the push and
// the guest may already be running on the TARGET. So the terminal verdict is
// gated on positive TARGET evidence (the target is the node that did NOT restart
// in this scenario), never on the source record. It always fails toward inaction:
// it commits only on a confirmed target handoff, fails only on confirmed
// non-handoff, and retries on any uncertainty - never destroying or stranding a
// live guest.
func reconcileVanishedSourceTask(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, taskID uuid.UUID, m store.Migration, vm store.VM) error {
	failToSource := func(reason string) error {
		// The guest is on the source (or down, to be restarted on source from the
		// intact source disk by the VM reconciler). Reap the bound target's incoming
		// (best-effort) and mark the migration failed (fail-safe-to-source).
		reapTargetIncoming(ctx, st, agent, log, m)
		log.WarnContext(ctx, "source task vanished; migration failed to source ("+reason+")",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		return failMigration(ctx, st, log, taskID, m.ID,
			agentclient.TaskTerminal{Status: "failed", Error: &agentclient.AgentError{
				Code: ErrCodeConvergenceFailed, Message: "source agent restarted mid-migration (" + reason + ")",
			}})
	}

	if !m.Live {
		// An offline target adopts a STOPPED copy the CP never started pre-cutover
		// (the CP starts it only in convergePostCutover AFTER a committed cutover),
		// and the source disk is intact. So an offline source restart leaves the
		// guest running nowhere and failing toward source is fully recoverable - no
		// target probe needed (mirrors the cancel arbiter's !m.Live short-circuit).
		return failToSource("offline migration; nothing handed off")
	}
	if m.TargetNodeID == nil {
		// A live migration with no bound target never handed off; guest on source.
		return failToSource("no bound target")
	}
	target, err := st.NodeByID(ctx, *m.TargetNodeID)
	if err != nil {
		// Target node unresolved: outcome UNKNOWN. Retry; touch nothing.
		return fmt.Errorf("load target node for vanished-task migration %s: %v", m.ID, err)
	}

	srcHandoff, err := agent.GetMigration(ctx, target.AdvertisedEndpoint, vm.Name, m.ID.String())
	if err != nil {
		var ae *agentclient.AgentError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			// The TARGET has no record either (both nodes restarted, or the target
			// record was also lost): outcome UNKNOWN. Retry; do NOT finalize (fail
			// toward inaction - degrades at worst to the recoverable pre-fix wedge).
			return fmt.Errorf("vanished-task migration %s: target has no record; retry", m.ID)
		}
		// Target unreachable / 5xx: outcome UNKNOWN. Retry; touch nothing.
		return fmt.Errorf("probe target migration %s: %v", m.ID, err)
	}

	switch srcHandoff.Phase {
	case agentapi.MigrationPhaseCompleted:
		// The handoff LANDED: the guest resumed on the target (the target stamps its
		// record completed on cont). Commit the cutover (idempotent re-pin
		// source->target) - the ONLY non-loss outcome. This is the positive evidence
		// that makes a terminal verdict safe.
		log.WarnContext(ctx, "source task vanished but target confirms handoff; completing to target",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		if err := commitCutover(ctx, st, log, taskID, m.ID, agentclient.TaskTerminal{Status: "success"}); err != nil {
			return err
		}
		source, serr := st.NodeByID(ctx, *m.SourceNodeID)
		if serr != nil {
			// Cutover committed; a source-node load failure only skips best-effort
			// post-cutover cleanup (leak, never a loss). Nothing to fail.
			log.WarnContext(ctx, "post-cutover source-node load failed; skipping convergence",
				slog.String("migration_id", m.ID.String()), slog.String("error", serr.Error()))
			return nil
		}
		convergePostCutover(ctx, st, agent, log, m.ID, vm, source, target, m.Live)
		return nil
	case agentapi.MigrationPhaseFailed, agentapi.MigrationPhaseCancelled,
		agentapi.MigrationPhaseSetup, agentapi.MigrationPhaseActive, agentapi.MigrationPhasePostcopyActive:
		// The target shows the handoff did NOT land (aborted, or still mid-flight and
		// now orphaned because the source that was driving it is gone). Positive
		// evidence the guest is NOT running on the target -> fail toward source.
		return failToSource("target confirms no handoff (phase " + string(srcHandoff.Phase) + ")")
	default:
		// An unknown target phase: outcome UNKNOWN. Retry; do NOT finalize.
		return fmt.Errorf("vanished-task migration %s: unexpected target phase %q; retry", m.ID, srcHandoff.Phase)
	}
}

// cancelTargetIncoming best-effort tells the bound TARGET to reap its live-migration
// incoming setup when the migration ends pre-cutover (source-task failure / cancel).
// Only for live migrations with a bound target; offline targets have no autonomous
// resume goroutine to unblock. A failure degrades to the target's own 30-minute
// incoming timeout backstop and must NOT change the migration outcome.
func cancelTargetIncoming(ctx context.Context, agent MigrationAgentClient, log *slog.Logger, m store.Migration, vm store.VM, target store.Node) {
	if !m.Live || m.TargetNodeID == nil {
		return
	}
	if _, err := agent.CancelMigration(ctx, target.AdvertisedEndpoint, vm.Name, m.ID.String()); err != nil {
		log.WarnContext(ctx, "target incoming teardown failed; relying on agent timeout backstop",
			slog.String("migration_id", m.ID.String()), slog.String("target", target.AdvertisedEndpoint), slog.String("error", err.Error()))
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
func convergePostCutover(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, migID uuid.UUID, vm store.VM, source, target store.Node, live bool) {
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

	// Fast-push: the cutover re-pointed current_node_id, so the
	// overlay peers' declared_fdb changed. Nudge them (and the target) to re-pull
	// immediately instead of waiting up to a heartbeat interval. Best-effort: a
	// failed lookup or nudge degrades to the heartbeat backstop and must NOT fail
	// the already-committed migration.
	peers, err := st.OverlayPeerNodesForVM(ctx, vm.ID)
	if err != nil {
		log.WarnContext(ctx, "post-cutover peer lookup failed; relying on heartbeat backstop",
			slog.String("migration_id", migID.String()), slog.String("error", err.Error()))
	}
	endpoints := map[string]struct{}{target.AdvertisedEndpoint: {}}
	for _, p := range peers {
		endpoints[p.AdvertisedEndpoint] = struct{}{}
	}
	for ep := range endpoints {
		if err := agent.NudgeHeartbeat(ctx, ep); err != nil {
			log.WarnContext(ctx, "post-cutover heartbeat nudge failed; heartbeat backstop will converge",
				slog.String("migration_id", migID.String()), slog.String("endpoint", ep), slog.String("error", err.Error()))
		}
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
	nics, err := resolveMigrationNics(ctx, st, vm.ID)
	if err != nil {
		return uuid.Nil, err
	}
	// Size the destination disk from the SOURCE VM's real boot-disk virtual size.
	// This runs BEFORE StartIncoming, while the source VM is still running and its
	// disk is readable via the agent's `-U` probe. A VM created without --disk-gib
	// has SizeGib=0, so without this the manifest sends size_bytes=0 and the target
	// makes a 0-byte disk. Threading ExpectedSizeBytes lets the target's
	// max(SizeBytes, ExpectedSize) guard size it correctly (fixes live AND offline -
	// they share this incoming-request literal). A fetch failure or zero size is a
	// RETRYABLE error: the VM stays on source, fail-safe-to-source.
	srcVM, err := agent.VM(ctx, source.AdvertisedEndpoint, vm.Name)
	if err != nil {
		return uuid.Nil, fmt.Errorf("fetch source vm for disk size: %v", err)
	}
	if srcVM.BootDiskVirtualSizeBytes <= 0 {
		return uuid.Nil, fmt.Errorf("source vm %s reported no boot disk size; cannot size destination", vm.Name)
	}
	expected := srcVM.BootDiskVirtualSizeBytes
	incoming, err := agent.StartIncomingMigration(ctx, target.AdvertisedEndpoint, vm.Name, agentapi.MigrationIncomingRequest{
		MigrationID:        m.ID,
		Mode:               mode,
		SourceNodeIdentity: ptrString(sourceIdentity(source.Name)),
		VMSpec:             spec,
		Disks:              &disks,
		Nics:               &nics,
		UserData:           userData,
		NetworkConfig:      networkConfig,
		ExpectedSizeBytes:  &expected,
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
	// Auditability for the auto-complete-vs-cancel race: if the migration was
	// already cancelled when the source reported success, the cutover OVERRIDES the
	// cancel (the source already handed off; the live copy is on the target). Log
	// the supersede so the cancelled->completed transition is not surprising. A
	// reload error here is non-fatal - it only affects this log line, not the
	// commit.
	if prior, perr := st.MigrationByID(ctx, migID); perr == nil && prior.Phase == store.MigrationPhaseCancelled {
		log.WarnContext(ctx, "cutover superseded a cancel (source already handed off; completing to target)",
			slog.String("migration_id", migID.String()))
	}

	err := st.CommitMigrationCutover(ctx, migID)
	if errors.Is(err, store.ErrConcurrentUpdate) {
		reloaded, rerr := st.MigrationByID(ctx, migID)
		if rerr != nil {
			return fmt.Errorf("reload migration after cutover CAS loss: %v", rerr)
		}
		switch reloaded.Phase {
		case store.MigrationPhaseCompleted:
			// Already completed by a concurrent commit: idempotent success.
		case store.MigrationPhaseFailed:
			// A genuinely-failed migration must never complete; surface and stop.
			return fmt.Errorf("cutover CAS lost to a failed migration %s; not completing", migID)
		default:
			// Lost the CAS to a non-completing writer - a progress update OR a
			// too-late cancel that landed between loadCutoverState and the Txn. Return
			// a retryable error: the source already succeeded, so on the next delivery
			// runMigration sees a (now) cancelled/terminal migration and routes through
			// finalizeForTerminalMigration, which re-polls the source task, observes
			// SUCCESS, and re-commits the cutover (no orphaned running target). A still-
			// active migration is re-driven normally. Either way the redelivery
			// arbitrates correctly on source-task status; this arm need only requeue.
			return fmt.Errorf("cutover CAS lost for migration %s (phase %q); retry", migID, reloaded.Phase)
		}
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
	// Best-effort, fail-open: persist final live-migration stats AFTER the
	// safety-critical cutover Txn has committed. A parse miss or a store error
	// is logged and ignored - the migration stays completed; stats are
	// observability only and must never fail or block a committed cutover.
	if stats, ok := parseMigrationStats(terminal.Result); ok {
		if serr := st.UpdateMigrationStats(ctx, migID, stats); serr != nil {
			log.WarnContext(ctx, "persist migration stats failed (migration stays completed)",
				slog.String("migration_id", migID.String()), slog.String("error", serr.Error()))
		}
	}

	log.InfoContext(ctx, "migration completed (cutover committed)", slog.String("migration_id", migID.String()))
	return nil
}

// parseMigrationStats extracts the final live-migration stats from a terminal
// task result. Returns (_, false) when the result carries no "stats" object -
// the common case for offline migrations and old agents. JSON numbers in a
// map[string]any decode as float64; asInt64 normalises them.
func parseMigrationStats(result map[string]any) (store.MigrationStats, bool) {
	raw, ok := result["stats"].(map[string]any)
	if !ok {
		return store.MigrationStats{}, false
	}
	ram, _ := raw["ram"].(map[string]any)
	disk, _ := raw["disk"].(map[string]any)
	return store.MigrationStats{
		RAMTransferred:    asInt64(ram["transferred"]),
		RAMTotal:          asInt64(ram["total"]),
		RAMDirtyPagesRate: asInt64(ram["dirty_pages_rate"]),
		DiskTransferred:   asInt64(disk["transferred"]),
		DiskTotal:         asInt64(disk["total"]),
		TotalTimeMs:       asInt64(raw["total_time_ms"]),
		DowntimeMs:        asInt64(raw["downtime_ms"]),
		SetupTimeMs:       asInt64(raw["setup_time_ms"]),
	}, true
}

// asInt64 coerces a decoded-JSON numeric (float64 / json.Number / int) to int64,
// defaulting to 0 for any other / missing value.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
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
func finalizeForTerminalMigration(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, taskID uuid.UUID, m store.Migration) error {
	switch m.Phase {
	case store.MigrationPhaseCompleted:
		result := []byte(fmt.Sprintf(`{"migration_id":%q}`, m.ID.String()))
		if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: result}); err != nil {
			return fmt.Errorf("finalize task success for completed migration %s: %v", m.ID, err)
		}
		log.InfoContext(ctx, "reconciled dangling task to success for completed migration",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		return nil
	case store.MigrationPhaseCancelled:
		return reconcileCancelledMigration(ctx, st, agent, log, taskID, m)
	default: // store.MigrationPhaseFailed
		// A failed migration is fail-safe-to-source by construction (the source-failure
		// path is the only writer of phase=failed; the guest never handed off). Reap
		// the bound target's live incoming (best-effort) and finalize the task failed.
		reapTargetIncoming(ctx, st, agent, log, m)
		if err := finalizeTask(ctx, st, taskID, store.TaskStatusFailed, ErrCodeConvergenceFailed, m.ErrorMessage); err != nil {
			return fmt.Errorf("finalize task failed for failed migration %s: %v", m.ID, err)
		}
		log.WarnContext(ctx, "reconciled dangling task to failed for failed migration",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		return nil
	}
}

// reconcileCancelledMigration arbitrates a CANCELLED migration on the AUTHORITATIVE
// source-task status, closing the two stranded paths d44b3eb left open. The cancel
// flag alone is NOT sufficient: a cancel that raced a source-success cutover is too
// late (the source already handed off and force-killed its qemu, so the only live
// copy is on the target). The source agent task is the sole arbiter:
//
//   - No AgentTaskID (cancelled while pending; source never contacted, nothing
//     moved) -> finalize the task cancelled. Nothing to reconcile.
//   - Source poll ERROR or indeterminate status (source outcome UNKNOWN) -> return a
//     RETRYABLE error and touch nothing. Fail toward INACTION: never reap the target
//     (it could be the only live copy) and never decide on uncertainty.
//   - Source SUCCESS (source handed off / destroyed) -> COMMIT THE CUTOVER and
//     complete to the target. Never reap the target.
//   - Source FAILED / CANCELLED (source aborted; guest alive on source) -> reap the
//     target's incoming and finalize the task cancelled. VM stays on source.
func reconcileCancelledMigration(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, taskID uuid.UUID, m store.Migration) error {
	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task for cancelled migration %s: %v", m.ID, err)
	}
	if task.AgentTaskID == nil {
		// A nil agent_task_id does NOT prove the source was never contacted: the id
		// is persisted only AFTER the outgoing StartOutgoing POST returns
		// (startOrResume), so a CP crash in that window leaves it nil while the
		// SOURCE is already pushing. Blindly finalizing cancelled here would strand
		// a source that autonomously completes and hands off - the VM ends up on the
		// target while the CP records cancelled (split-brain / loss). Probe the
		// source's migration record (read-only, keyed on migration_id) and arbitrate
		// on it before finalizing.
		return reconcileCancelledNoAgentTask(ctx, st, agent, log, taskID, m)
	}
	if m.SourceNodeID == nil {
		// A cancelled migration that started a handshake but has no source is
		// malformed and cannot be arbitrated. Retryable rather than a destructive
		// guess.
		return fmt.Errorf("cancelled migration %s has an agent task but no source node; cannot arbitrate", m.ID)
	}
	source, err := st.NodeByID(ctx, *m.SourceNodeID)
	if err != nil {
		// Source node unresolved: source outcome UNKNOWN. Retry; do NOT destroy.
		return fmt.Errorf("load source node for cancelled migration %s: %v", m.ID, err)
	}

	terminal, perr := agent.PollTask(ctx, source.AdvertisedEndpoint, *task.AgentTaskID)
	if perr != nil {
		// Source outcome UNKNOWN: fail toward inaction. The target is NOT reaped and
		// nothing is finalized; the next delivery re-arbitrates.
		return fmt.Errorf("poll source task for cancelled migration %s: %v", m.ID, perr)
	}

	switch terminal.Status {
	case "success":
		// The source HANDED OFF (destroyed); the live copy is on the target. The cancel
		// raced the cutover and lost. Completing to the target is the only
		// non-destructive outcome - refusing would strand a RUNNING target while the CP
		// records cancelled (split-brain / VM-loss).
		log.WarnContext(ctx, "cancel superseded by completed cutover on reconcile (source already handed off; completing to target)",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		if err := commitCutover(ctx, st, log, taskID, m.ID, terminal); err != nil {
			return err
		}
		// Mirror the success arm: best-effort post-cutover convergence. DeleteVMOnSource
		// is a harmless no-op (the source is already gone). NEVER reaps the target.
		if vm, verr := st.VMByID(ctx, m.VmID); verr == nil {
			if m.TargetNodeID != nil {
				if target, terr := st.NodeByID(ctx, *m.TargetNodeID); terr == nil {
					convergePostCutover(ctx, st, agent, log, m.ID, vm, source, target, m.Live)
				}
			}
		}
		return nil
	case "failed", "cancelled":
		// The source ABORTED: the guest is still alive on source (fail-safe). Reap the
		// target's incoming and finalize the task cancelled.
		reapTargetIncoming(ctx, st, agent, log, m)
		if err := finalizeTask(ctx, st, taskID, store.TaskStatusCancelled, ErrCodeMigrationCancelled, m.ErrorMessage); err != nil {
			return fmt.Errorf("finalize task cancelled for cancelled migration %s: %v", m.ID, err)
		}
		log.InfoContext(ctx, "reconciled dangling task to cancelled for cancelled migration (source aborted; vm stays on source)",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		return nil
	default:
		// Indeterminate source status: source outcome UNKNOWN. Retry; do NOT destroy.
		return fmt.Errorf("cancelled migration %s: unexpected source terminal status %q; retry", m.ID, terminal.Status)
	}
}

// reconcileCancelledNoAgentTask arbitrates a cancelled migration whose task has
// no persisted agent_task_id. It probes the SOURCE agent's migration record by
// migration_id (read-only) and branches so the potentially-split-brain action
// (finalize cancelled) is taken ONLY when the source is confirmed uninvolved or
// aborted - never while it is in-flight or has already handed off:
//
//   - 404 (source has no record) -> the source was truly never contacted; the
//     original safe behaviour: finalize the task cancelled. Nothing moved.
//   - probe ERROR (source unreachable / 5xx) -> outcome UNKNOWN; RETRYABLE, touch
//     nothing (fail toward inaction).
//   - source phase COMPLETED -> the source handed off; the live copy is on the
//     target. Commit the cutover (the only non-loss outcome), like the
//     agent-task success arm.
//   - source phase FAILED / CANCELLED -> the source aborted; guest alive on
//     source. Reap the target incoming and finalize cancelled.
//   - source phase in-flight (setup / active / postcopy_active) -> outcome
//     UNKNOWN; RETRYABLE. The source is still pushing; re-arbitrate next delivery
//     (the source record is retained until it reaches a terminal phase).
//
// This is strictly safer than the pre-fix nil-arm, which always finalized
// cancelled: the 404 branch preserves that behaviour, and every other branch
// only adds a non-destructive recovery the old code got wrong.
func reconcileCancelledNoAgentTask(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, taskID uuid.UUID, m store.Migration) error {
	finalizeCancelled := func(reason string) error {
		if ferr := finalizeTask(ctx, st, taskID, store.TaskStatusCancelled, ErrCodeMigrationCancelled, m.ErrorMessage); ferr != nil {
			return fmt.Errorf("finalize task cancelled for cancelled migration %s: %v", m.ID, ferr)
		}
		log.InfoContext(ctx, "reconciled dangling task to cancelled for cancelled migration ("+reason+")",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		return nil
	}

	if !m.Live {
		// The split-brain this arbitration prevents only exists for LIVE
		// migrations: an offline target adopts a STOPPED copy and never
		// auto-resumes, so a completed offline source leaves the guest intact on
		// the source disk - finalizing cancelled is fully recoverable (the pre-fix
		// behaviour). Skip the source probe entirely so a cancel whose very reason
		// is an unreachable source does not hang in a retry loop.
		return finalizeCancelled("offline migration; nothing live to arbitrate")
	}
	if m.SourceNodeID == nil {
		// No source to probe: a cancelled migration with no source truly never
		// contacted anyone. Finalize cancelled (the original safe behaviour).
		return finalizeCancelled("no source node")
	}
	source, err := st.NodeByID(ctx, *m.SourceNodeID)
	if err != nil {
		// Source node unresolved: outcome UNKNOWN. Retry; do NOT finalize.
		return fmt.Errorf("load source node for cancelled migration %s: %v", m.ID, err)
	}
	vm, err := st.VMByID(ctx, m.VmID)
	if err != nil {
		return fmt.Errorf("load vm for cancelled migration %s: %v", m.ID, err)
	}

	srcMig, err := agent.GetMigration(ctx, source.AdvertisedEndpoint, vm.Name, m.ID.String())
	if err != nil {
		var ae *agentclient.AgentError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			// The source has no record for this migration: never contacted (the
			// cancel landed at/before the handshake). Safe to finalize cancelled.
			return finalizeCancelled("source has no migration record")
		}
		// Source outcome UNKNOWN (unreachable / 5xx): fail toward inaction.
		return fmt.Errorf("probe source migration %s: %v", m.ID, err)
	}

	switch srcMig.Phase {
	case agentapi.MigrationPhaseCompleted:
		// The source HANDED OFF; the live copy is on the target. The cancel lost
		// the race. Commit the cutover (mirroring the agent-task success arm) - the
		// only outcome that does not lose a running VM.
		log.WarnContext(ctx, "cancel superseded by completed source handoff on reconcile (no agent task; completing to target)",
			slog.String("migration_id", m.ID.String()), slog.String("task_id", taskID.String()))
		if err := commitCutover(ctx, st, log, taskID, m.ID, agentclient.TaskTerminal{Status: "success"}); err != nil {
			return err
		}
		if m.TargetNodeID != nil {
			if target, terr := st.NodeByID(ctx, *m.TargetNodeID); terr == nil {
				convergePostCutover(ctx, st, agent, log, m.ID, vm, source, target, m.Live)
			}
		}
		return nil
	case agentapi.MigrationPhaseFailed, agentapi.MigrationPhaseCancelled:
		// The source ABORTED; the guest is still alive on source (fail-safe). Reap
		// the target incoming and finalize cancelled.
		reapTargetIncoming(ctx, st, agent, log, m)
		return finalizeCancelled("source aborted; vm stays on source")
	default:
		// setup / active / postcopy_active: the source is still pushing, outcome
		// UNKNOWN. Retry; do NOT finalize or destroy. Re-arbitrate next delivery.
		return fmt.Errorf("cancelled migration %s: source still in-flight (phase %q); retry", m.ID, srcMig.Phase)
	}
}

// reapTargetIncoming best-effort tells the bound TARGET to reap its live-migration
// incoming setup for a pre-cutover terminal outcome (failed / source-aborted
// cancel). NOT called on a completed migration (the target IS the running VM) nor
// on uncertainty (the target could be the only live copy). A failure degrades to
// the agent's own incoming-timeout backstop and never fails the reconcile.
func reapTargetIncoming(ctx context.Context, st MigrationWorkerStore, agent MigrationAgentClient, log *slog.Logger, m store.Migration) {
	if !m.Live || m.TargetNodeID == nil {
		return
	}
	target, err := st.NodeByID(ctx, *m.TargetNodeID)
	if err != nil {
		return
	}
	vm, verr := st.VMByID(ctx, m.VmID)
	if verr != nil {
		return
	}
	if _, cerr := agent.CancelMigration(ctx, target.AdvertisedEndpoint, vm.Name, m.ID.String()); cerr != nil {
		log.WarnContext(ctx, "reconcile: target incoming teardown failed; agent timeout backstop",
			slog.String("migration_id", m.ID.String()), slog.String("error", cerr.Error()))
	}
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

// NicNetworkPair couples a VM NIC with its resolved network. Bridge name and MTU
// live on the network (not the vm_nic row), so MigrationNics needs both to build
// the target-facing manifest.
type NicNetworkPair struct {
	Nic     store.VMNic
	Network store.Network
}

// MigrationNics builds the ordered MigrationNic manifest the target uses to
// materialize taps and drive the post-cutover gratuitous ARP. Bridge name and
// MTU come from the resolved network; mac / model / device order from the NIC.
// ipv4 is included only when the NIC carries a static address.
func MigrationNics(pairs []NicNetworkPair) []agentapi.MigrationNic {
	out := make([]agentapi.MigrationNic, 0, len(pairs))
	for _, p := range pairs {
		m := agentapi.MigrationNic{
			ID:          p.Nic.ID,
			MacAddress:  p.Nic.MacAddress.String(),
			BridgeName:  p.Network.BridgeName,
			Model:       agentapi.MigrationNicModel(p.Nic.Model),
			DeviceOrder: int(p.Nic.DeviceOrder),
		}
		if p.Network.Mtu != 0 {
			mtu := int(p.Network.Mtu)
			m.Mtu = &mtu
		}
		if p.Nic.Ipv4Address != nil {
			ip := p.Nic.Ipv4Address.String()
			m.Ipv4Address = &ip
		}
		out = append(out, m)
	}
	return out
}

// resolveMigrationNics loads the VM's NICs and resolves each against its network
// (mirroring resolveCreateNICs in package vms: bridge / MTU from the network,
// mac / model / order from the vm_nic), then builds the target-facing manifest.
// A missing network is an internal inconsistency (the row is created atomically
// with the NIC) and is propagated.
func resolveMigrationNics(ctx context.Context, st MigrationWorkerStore, vmID uuid.UUID) ([]agentapi.MigrationNic, error) {
	nics, err := st.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("list vm nics: %v", err)
	}
	pairs := make([]NicNetworkPair, 0, len(nics))
	for _, n := range nics {
		net, err := st.NetworkByID(ctx, n.NetworkID)
		if err != nil {
			return nil, fmt.Errorf("resolve network %s for nic %s: %v", n.NetworkID, n.ID, err)
		}
		pairs = append(pairs, NicNetworkPair{Nic: n, Network: net})
	}
	return MigrationNics(pairs), nil
}

// targetPoolPath resolves the bound migration's TargetPoolName to the storage
// pool's absolute filesystem root on the TARGET node. Pool names are scoped per
// (node_id, name), so the name index can return rows on several nodes; the row
// on m.TargetNodeID is the destination. A missing pool on the target is NOT a
// hard failure: it wraps errTargetPoolNotReady so the caller records the
// migration pending (retryable) and waits for the pool to appear, rather
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
