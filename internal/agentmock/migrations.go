// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// migrationRecord is the in-memory state of one migration the mock is
// simulating, keyed by the CP-allocated migration uuid. Both sides of
// the two-phase handshake materialise a record: the target on
// VMMigrationsStartIncoming (role=target, terminalPhase set to the
// incoming-side resting phase), the source on VMMigrationsStartOutgoing
// (role=source, driven by the queued MigrationResult).
//
// Phase / progress are derived at read time from (createdAt, delay,
// finalPhase) — no per-migration goroutine or timer. A terminal
// override (cancelled / failed) short-circuits the time-based advance.
type migrationRecord struct {
	migrationID  uuid.UUID
	vmUUID       uuid.UUID
	vmName       string
	role         agentapi.MigrationRole
	mode         agentapi.MigrationMode
	peerEndpoint string

	createdAt time.Time
	delay     time.Duration

	// finalPhase is the phase the record settles into once delay
	// elapses (completed for a happy path, active for a stall, failed
	// for a staged error).
	finalPhase agentapi.MigrationPhase

	// terminalOverride, when non-empty, pins the phase regardless of
	// elapsed time. Set by VMMigrationsCancel (cancelled) and by an
	// incoming record that has no time-based progression of its own.
	terminalOverride agentapi.MigrationPhase

	errorMessage string
}

// AddMigrationResult queues a synthetic outcome for the next
// VMMigrationsStartOutgoing invocation against the source vmName.
// Calls are FIFO per name; an empty queue yields a default success
// that converges to the completed phase after defaultPoolScanDelay.
// See MigrationResult docs for field semantics.
func (m *Mock) AddMigrationResult(vmName string, r MigrationResult) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.migrationResults == nil {
		m.state.migrationResults = map[string][]MigrationResult{}
	}
	m.state.migrationResults[vmName] = append(m.state.migrationResults[vmName], r)
}

// takeMigrationResultLocked pops the next queued MigrationResult for
// vmName, normalising defaults. An empty queue yields a default
// success. Caller holds mu.
func (s *state) takeMigrationResultLocked(vmName string) MigrationResult {
	queue := s.migrationResults[vmName]
	if len(queue) == 0 {
		return MigrationResult{Status: "success", FinalPhase: "completed", Delay: defaultPoolScanDelay}
	}
	head := queue[0]
	if len(queue) == 1 {
		delete(s.migrationResults, vmName)
	} else {
		s.migrationResults[vmName] = queue[1:]
	}
	if head.Status == "" {
		if head.Error != nil {
			head.Status = "failed"
		} else {
			head.Status = "success"
		}
	}
	if head.FinalPhase == "" {
		if head.Status == "failed" {
			head.FinalPhase = "failed"
		} else {
			head.FinalPhase = "completed"
		}
	}
	if head.Delay <= 0 {
		head.Delay = defaultPoolScanDelay
	}
	return head
}

// VMMigrationsStartIncoming implements
// POST /v1/vms/{vm_name}/migrations/incoming — Phase 1 of the
// two-phase handshake on the target agent. Allocates a fake listen
// endpoint from the advertised migration range, mints a single-use
// auth token, echoes the CP-allocated migration_id, and stores a
// target-side migration record. Sync 200.
func (m *Mock) VMMigrationsStartIncoming(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VMMigrationsStartIncomingParams) {
	const opID = "vmMigrations.startIncoming"
	if m.preDispatch(w, r, opID) {
		return
	}

	var body agentapi.MigrationIncomingRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "malformed incoming migration request body")
		return
	}
	// A real target agent cannot build the TLS authz pin without the
	// source identity; it 400s when absent. Mirror that so the e2e
	// catches a worker that forgets to send it.
	if body.SourceNodeIdentity == nil || *body.SourceNodeIdentity == "" {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "source_node_identity is required")
		return
	}

	now := time.Now().UTC()
	m.state.mu.Lock()
	listenEndpoint := m.allocateMigrationEndpointLocked(0)
	// For live migrations the target exports the guest disks over a
	// SEPARATE NBD listener, distinct from the RAM listener in
	// listen_endpoint. Offline reuses the single endpoint, so nbd is nil.
	var nbdEndpoint *string
	if body.Mode == agentapi.MigrationIncomingRequestMode(agentapi.MigrationModeLive) {
		ep := m.allocateMigrationEndpointLocked(1)
		nbdEndpoint = &ep
	}
	if m.state.migrations == nil {
		m.state.migrations = map[uuid.UUID]*migrationRecord{}
	}
	var vmUUID uuid.UUID
	if v, ok := m.state.storedVMs[vmName]; ok {
		vmUUID = v.ID
	}
	m.state.migrations[body.MigrationID] = &migrationRecord{
		migrationID:  body.MigrationID,
		vmUUID:       vmUUID,
		vmName:       vmName,
		role:         agentapi.Target,
		mode:         agentapi.MigrationMode(body.Mode),
		peerEndpoint: listenEndpoint,
		createdAt:    now,
		// The target side has no precopy clock to drive of its own; it
		// sits in setup until the CP tears it down. The source's
		// outgoing task is the authoritative progress driver.
		terminalOverride: agentapi.MigrationPhaseSetup,
	}
	m.state.mu.Unlock()

	m.respondJSON(w, r, opID, http.StatusOK, agentapi.MigrationIncomingResponse{
		AuthToken:      "mig-token-" + body.MigrationID.String(),
		ListenEndpoint: listenEndpoint,
		MigrationID:    body.MigrationID,
		NbdEndpoint:    nbdEndpoint,
	})
}

// allocateMigrationEndpointLocked returns a host:port the source must
// connect to, drawn from the advertised migration range at
// range-start+offset. The mock hands out fixed offsets (0 for the RAM
// listener, 1 for the live NBD disk-export listener); it does not model
// multi-migration port contention. Caller holds mu.
func (m *Mock) allocateMigrationEndpointLocked(offset int) string {
	host := m.state.migration.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := m.state.migration.PortRangeStart
	if port == 0 {
		port = 49152
	}
	return host + ":" + strconv.Itoa(port+offset)
}

// VMMigrationsStartOutgoing implements
// POST /v1/vms/{vm_name}/migrations/outgoing — Phase 2 on the source
// agent. Mints a vm.migrate async task with the queued outcome, stores
// a source-side migration record whose phase advances in lockstep with
// the task, and returns 202 + AsyncTaskAccepted. The migration
// converges (or fails) lazily as TasksGet / VMMigrationsGet are polled.
func (m *Mock) VMMigrationsStartOutgoing(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VMMigrationsStartOutgoingParams) {
	const opID = "vmMigrations.startOutgoing"
	if m.preDispatch(w, r, opID) {
		return
	}

	var body agentapi.MigrationOutgoingRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "malformed outgoing migration request body")
		return
	}
	// A real source agent cannot set the tls-hostname pin without the
	// target identity; it 400s when absent. Mirror that so the e2e
	// catches a worker that forgets to send it.
	if body.TargetNodeIdentity == nil || *body.TargetNodeIdentity == "" {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "target_node_identity is required")
		return
	}
	// A real source agent dials the target's NBD disk-export listener for
	// live migrations; it 400s when the CP forgot to relay nbd_endpoint
	// (offline reuses the single endpoint, so the guard is live-only).
	// Mirror that so the e2e catches a worker that drops the relay.
	if body.Mode == agentapi.MigrationOutgoingRequestMode(agentapi.MigrationModeLive) &&
		(body.NbdEndpoint == nil || *body.NbdEndpoint == "") {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "nbd_endpoint is required for live migrations")
		return
	}

	agentTaskID := uuid.New()
	now := time.Now().UTC()

	m.state.mu.Lock()
	outcome := m.state.takeMigrationResultLocked(vmName)
	var vmUUID uuid.UUID
	if v, ok := m.state.storedVMs[vmName]; ok {
		vmUUID = v.ID
	}
	errMessage := ""
	if outcome.Error != nil {
		errMessage = outcome.Error.Message
	}
	if m.state.migrations == nil {
		m.state.migrations = map[uuid.UUID]*migrationRecord{}
	}
	m.state.migrations[body.MigrationID] = &migrationRecord{
		migrationID:  body.MigrationID,
		vmUUID:       vmUUID,
		vmName:       vmName,
		role:         agentapi.Source,
		mode:         agentapi.MigrationMode(body.Mode),
		peerEndpoint: body.TargetEndpoint,
		createdAt:    now,
		delay:        outcome.Delay,
		finalPhase:   agentapi.MigrationPhase(outcome.FinalPhase),
		errorMessage: errMessage,
	}
	m.state.tasks[agentTaskID] = &agentTask{
		id:              agentTaskID,
		taskType:        "vm.migrate",
		resourceType:    "vm",
		resourceID:      vmUUID,
		createdAt:       now,
		delay:           outcome.Delay,
		migrationResult: &outcome,
		migrationID:     body.MigrationID,
	}
	m.state.mu.Unlock()

	m.respondJSON(w, r, opID, http.StatusAccepted, agentapi.AsyncTaskAccepted{
		TaskID: agentTaskID,
		Status: agentapi.AsyncTaskAcceptedStatus("pending"),
		Links: struct {
			Self string `json:"self"`
		}{Self: "/v1/tasks/" + agentTaskID.String()},
	})
}

// VMMigrationsGet implements
// GET /v1/vms/{vm_name}/migrations/{migration_id} — snapshots the
// migration record under lock and projects its phase / progress at the
// current instant. Unknown ids return the standard not_found envelope.
func (m *Mock) VMMigrationsGet(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, migrationID agentapi.MigrationID) {
	const opID = "vmMigrations.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	now := time.Now().UTC()

	m.state.mu.Lock()
	rec, ok := m.state.migrations[migrationID]
	var snapshot migrationRecord
	if ok {
		snapshot = *rec
	}
	m.state.mu.Unlock()

	if !ok {
		m.respondNotFound(w, r, opID, "migration not found on this agent")
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, projectMigration(&snapshot, now))
}

// VMMigrationsCancel implements
// POST /v1/vms/{vm_name}/migrations/{migration_id}/cancel — pins the
// migration record to the cancelled phase and returns it. Sync 200.
// Unknown ids return not_found.
func (m *Mock) VMMigrationsCancel(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, migrationID agentapi.MigrationID, _ agentapi.VMMigrationsCancelParams) {
	const opID = "vmMigrations.cancel"
	if m.preDispatch(w, r, opID) {
		return
	}
	now := time.Now().UTC()

	m.state.mu.Lock()
	rec, ok := m.state.migrations[migrationID]
	var snapshot migrationRecord
	if ok {
		rec.terminalOverride = agentapi.MigrationPhaseCancelled
		snapshot = *rec
	}
	m.state.mu.Unlock()

	if !ok {
		m.respondNotFound(w, r, opID, "migration not found on this agent")
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, projectMigration(&snapshot, now))
}

// projectMigration renders the OpenAPI Migration projection of a
// record at the given instant. A terminalOverride (cancelled / setup
// resting phase) pins the phase; otherwise the phase advances by
// elapsed-vs-delay: setup for the first half, active for the second
// half, then finalPhase. ProgressPercent tracks the same arc.
//
// Pure function — callers copy the record under the lock first.
func projectMigration(rec *migrationRecord, now time.Time) agentapi.Migration {
	mig := agentapi.Migration{
		MigrationID:     rec.migrationID,
		VMUUID:          rec.vmUUID,
		Role:            rec.role,
		Mode:            rec.mode,
		CreatedAt:       rec.createdAt,
		ProgressPercent: 0,
	}
	if rec.peerEndpoint != "" {
		ep := rec.peerEndpoint
		mig.PeerEndpoint = &ep
	}

	if rec.terminalOverride != "" {
		mig.Phase = rec.terminalOverride
		return mig
	}

	elapsed := now.Sub(rec.createdAt)
	half := rec.delay / 2

	switch {
	case elapsed < half:
		mig.Phase = agentapi.MigrationPhaseSetup
		mig.ProgressPercent = 0
	case elapsed < rec.delay:
		mig.Phase = agentapi.MigrationPhaseActive
		// Linear ramp across the active half: 0..100, capped below 100
		// until the record actually settles.
		span := rec.delay - half
		if span > 0 {
			pct := int((elapsed - half) * 100 / span)
			if pct > 99 {
				pct = 99
			}
			mig.ProgressPercent = pct
		}
	default:
		mig.Phase = rec.finalPhase
		started := rec.createdAt.Add(half)
		mig.StartedAt = &started
		switch rec.finalPhase {
		case agentapi.MigrationPhaseCompleted:
			mig.ProgressPercent = 100
			completed := rec.createdAt.Add(rec.delay)
			mig.CompletedAt = &completed
		case agentapi.MigrationPhaseFailed:
			if rec.errorMessage != "" {
				em := rec.errorMessage
				mig.ErrorMessage = &em
			}
		default:
			// A stall (finalPhase == active or another non-terminal
			// value) never reaches 100%.
			mig.ProgressPercent = 99
		}
	}
	return mig
}

// projectVMMigrateTerminal fills the Task with the vm.migrate terminal
// projection — migration_id under task.result on success, the staged
// ErrorEnvelope on failure. Mirrors projectVMLifecycleTerminal.
func projectVMMigrateTerminal(task *agentapi.Task, t *agentTask) {
	if t.migrationResult == nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus(t.migrationResult.Status)
	switch t.migrationResult.Status {
	case "success":
		resultMap := map[string]any{
			"migration_id": t.migrationID.String(),
		}
		task.Result = &resultMap
	case "failed", "cancelled":
		if t.migrationResult.Error != nil {
			task.Error = projectErrorEnvelope(t.migrationResult.Error)
		}
	}
}
