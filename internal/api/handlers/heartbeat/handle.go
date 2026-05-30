// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Receive serves POST /v1/nodes/{name}/heartbeat. Pre-conditions on
// entry: the route runs behind agentMTLS, so r.Context carries an
// *auth.Agent; absence of one is a routing bug, not a runtime
// situation.
//
// Path identifier is the cluster-unique node name (name-keyed agent
// identity). The agent does not know
// its own UUID; identity is derived from the cert CN at startup and
// carried inline in the URL. The cert→UUID binding inside agent_certs
// stays UUID-keyed (unchanged) — this handler resolves name→UUID
// once via GetNodeByName, then enforces the binding check against
// agent.NodeID, identical to the prior UUID-only contract.
func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	if agent == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "agent identity missing from context", nil)
		return
	}

	urlNodeName := chi.URLParam(r, "name")
	if urlNodeName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "node name missing from path",
			map[string]any{"path_field": "name"})
		return
	}
	node, err := h.store.NodeByName(r.Context(), urlNodeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "node not found", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "node lookup by name failed",
			slog.String("node_name", urlNodeName),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "node lookup failed", nil)
		return
	}
	if node.ID != agent.NodeID {
		// The cert is bound to a different node — refuse without
		// disclosing whether the requested name maps to a UUID that
		// happens to exist.
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "client certificate is not bound to this node",
			map[string]any{"reason": "cert_san_unknown"})
		return
	}

	var body requestBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, fmt.Sprintf("invalid request body: %v", err), nil)
		return
	}
	if errResp := validateRequest(&body); errResp != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, errResp.message,
			map[string]any{"field": errResp.field})
		return
	}

	outcome, err := h.project(r.Context(), agent, &body)
	if err != nil {
		var pe *projectionError
		if errors.As(err, &pe) {
			response.WriteError(w, r, pe.status, pe.code, pe.message, pe.details)
			return
		}
		h.log.ErrorContext(r.Context(), "heartbeat projection failed",
			slog.String("node_id", agent.NodeID.String()),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "failed to apply heartbeat", nil)
		return
	}

	h.logPressureTransition(r.Context(), agent.NodeID, &body, outcome)

	response.WriteJSON(w, r, http.StatusOK, responseBody{
		ReceivedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		DeclaredPools: outcome.declaredPools,
		DeclaredVMs:   outcome.declaredVMs,
	})
}

// logPressureTransition emits one slog line per pressure-state change
// (set → WARN, clear → INFO). Steady-state and counting outcomes stay
// quiet — operators want signal on transitions, not on every heartbeat.
// Logged after InTx commits so the line never appears for a rolled-back
// projection.
func (h *Handler) logPressureTransition(ctx context.Context, nodeID uuid.UUID, body *requestBody, outcome heartbeatOutcome) {
	switch outcome.memory {
	case pressureTransitionSet:
		h.log.WarnContext(ctx, "memory pressure set",
			slog.String("node_id", nodeID.String()),
			slog.Float64("memory_percent", memoryPercent(body)),
			slog.Int("threshold_percent", h.pressureMemory.ThresholdPercent),
			slog.Int("consecutive_required", h.pressureMemory.ConsecutiveRequired),
		)
	case pressureTransitionCleared:
		h.log.InfoContext(ctx, "memory pressure cleared",
			slog.String("node_id", nodeID.String()),
			slog.Float64("memory_percent", memoryPercent(body)),
		)
	}
	switch outcome.systemDisk {
	case pressureTransitionSet:
		h.log.WarnContext(ctx, "system_disk pressure set",
			slog.String("node_id", nodeID.String()),
			slog.Float64("system_disk_percent", systemDiskPercent(body)),
			slog.Int("threshold_percent", h.pressureSystemDisk.ThresholdPercent),
			slog.Int("consecutive_required", h.pressureSystemDisk.ConsecutiveRequired),
		)
	case pressureTransitionCleared:
		h.log.InfoContext(ctx, "system_disk pressure cleared",
			slog.String("node_id", nodeID.String()),
			slog.Float64("system_disk_percent", systemDiskPercent(body)),
		)
	}
}

// memoryPercent returns the percentage available reported by the
// heartbeat body, or 0 when totals are missing or zero. Used for the
// log line only — pressure decision logic lives in
// computePressureTransition and does not rely on this helper.
func memoryPercent(body *requestBody) float64 {
	total := body.Capabilities.MemoryTotalMib
	if total <= 0 {
		return 0
	}
	return float64(body.Resources.MemoryAvailableMib) * 100.0 / float64(total)
}

// systemDiskPercent returns the percentage available reported for the
// root filesystem. Zero when the agent omitted the metric (statfs
// failed). Used for the log line only.
func systemDiskPercent(body *requestBody) float64 {
	total := body.Resources.SystemDiskTotalBytes
	avail := body.Resources.SystemDiskAvailableBytes
	if total == nil || avail == nil || *total <= 0 {
		return 0
	}
	return float64(*avail) * 100.0 / float64(*total)
}

// heartbeatOutcome bundles state-change signals computed during the
// projection that the receive handler needs to surface after the
// transaction commits (currently pressure transitions for memory and
// system_disk). One field per pressure dimension; pool disk transitions
// live entirely on the scan worker side.
type heartbeatOutcome struct {
	memory        pressureTransitionKind
	systemDisk    pressureTransitionKind
	declaredPools []declaredPool
	declaredVMs   []declaredVM
}

// project runs the full state projection in a single transaction.
// It returns the post-commit heartbeatOutcome (currently only pressure
// state transitions) and a typed *projectionError for HTTP-shaped
// failures (404 / 409); any other error is treated as internal.
func (h *Handler) project(ctx context.Context, agent *auth.Agent, body *requestBody) (heartbeatOutcome, error) {
	var outcome heartbeatOutcome
	err := h.store.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		node, err := tx.NodeForHeartbeat(ctx, agent.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return &projectionError{
					status: http.StatusNotFound, code: response.CodeNotFound,
					message: "node not found",
				}
			}
			return fmt.Errorf("get node: %v", err)
		}
		if string(node.Architecture) != body.Architecture {
			return &projectionError{
				status: http.StatusConflict, code: response.CodeConflict,
				message: "reported architecture does not match registered architecture",
				details: map[string]any{
					"reason":     "architecture_mismatch",
					"registered": string(node.Architecture),
					"reported":   body.Architecture,
				},
			}
		}
		if node.Status == store.NodeStatusGone {
			return &projectionError{
				status: http.StatusConflict, code: response.CodeConflict,
				message: "node is in gone status; heartbeats rejected",
				details: map[string]any{"reason": "node_gone"},
			}
		}

		if err := h.applyNodeUpdate(ctx, tx, agent.NodeID, body); err != nil {
			return err
		}
		now := time.Now().UTC()
		memKind, err := h.applyMemoryPressure(ctx, tx, agent.NodeID, node, body, now)
		if err != nil {
			return err
		}
		outcome.memory = memKind
		sysKind, err := h.applySystemDiskPressure(ctx, tx, agent.NodeID, node, body, now)
		if err != nil {
			return err
		}
		outcome.systemDisk = sysKind
		if err := h.applyFirmwares(ctx, tx, agent.NodeID, body.Capabilities.Firmwares); err != nil {
			return err
		}
		if err := h.applyVMs(ctx, tx, agent.NodeID, body.VMs); err != nil {
			return err
		}
		if err := h.applyPoolReports(ctx, tx, agent.NodeID, body.Pools); err != nil {
			return err
		}
		declared, err := h.loadDeclaredPools(ctx, tx, agent.NodeID)
		if err != nil {
			return err
		}
		outcome.declaredPools = declared
		declaredVMs, err := h.loadDeclaredVMs(ctx, tx, agent.NodeID)
		if err != nil {
			return err
		}
		outcome.declaredVMs = declaredVMs
		return nil
	})
	return outcome, err
}

// loadDeclaredVMs returns the per-node VM desired-state inventory the CP
// wants the agent's VM reconciler to converge on. Surfaces as
// `HeartbeatResponse.declared_vms`. The
// underlying SQL orders rows lower(name) asc so the agent's diff stays
// deterministic. Soft-deleted VMs are filtered out at the SQL layer;
// VMs whose vm_runtime.phase has reached 'gone' are excluded too (the
// runtime declared the VM unmaterialised on this node).
func (h *Handler) loadDeclaredVMs(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID) ([]declaredVM, error) {
	rows, err := tx.ListVMsForNodeDeclared(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list vms for node declared: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]declaredVM, 0, len(rows))
	for _, row := range rows {
		out = append(out, declaredVM{
			Name:         row.Name,
			DesiredPhase: string(row.DesiredPhase),
			Generation:   row.Generation,
		})
	}
	return out, nil
}

// applyPoolReports walks each agent-reported pool and applies its
// reconciliation_status / reconciliation_error onto the matching
// `storage_pools` row. The join key is
// (node_id, lower(name)); rows that no longer exist (operator deleted
// the pool mid-tick) yield zero rows affected — the agent reconciles
// on its next tick. failure here propagates as a projection error;
// the transaction rolls back.
func (h *Handler) applyPoolReports(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, reports []poolReport) error {
	for _, p := range reports {
		params := store.UpdateStoragePoolReconciliationParams{
			ReconciliationStatus: p.ReconciliationStatus,
			ReconciliationError:  p.ReconciliationError,
			NodeID:               nodeID,
			Name:                 p.Name,
		}
		if err := tx.UpdateStoragePoolReconciliation(ctx, params); err != nil {
			return fmt.Errorf("update pool reconciliation: %v", err)
		}
	}
	return nil
}

// loadDeclaredPools returns the per-node pool inventory the CP wants
// the agent to materialise. Surfaces as `HeartbeatResponse.declared_pools`.
// The query is row-ordered (lower(name) asc) so the
// agent's diff against observed state stays deterministic across
// heartbeats.
func (h *Handler) loadDeclaredPools(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID) ([]declaredPool, error) {
	rows, err := tx.ListStoragePoolsByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list storage pools by node: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]declaredPool, 0, len(rows))
	for _, row := range rows {
		dp := declaredPool{
			Name: row.Name,
			Type: row.Type,
			Path: row.Path,
		}
		if len(row.Config) > 0 {
			cfg := make(map[string]any)
			if err := json.Unmarshal(row.Config, &cfg); err != nil {
				// Storage_pools.config is a JSON object literal per the
				// CHECK constraint, but a malformed value should not
				// take down the whole heartbeat. Log + skip the field.
				h.log.WarnContext(ctx, "storage_pools.config unmarshal failed; emitting empty object",
					slog.String("pool_name", row.Name),
					slog.String("error", err.Error()))
				dp.Config = map[string]any{}
			} else {
				dp.Config = cfg
			}
		}
		out = append(out, dp)
	}
	return out, nil
}

// applyMemoryPressure computes the next memory-pressure state via the
// pure computePressureTransition function and persists it via
// UpdateNodeMemoryPressure. Runs inside the same transaction as the
// rest of the projection so pressure state never drifts ahead of the
// raw metrics that determined it. Returns the transition kind so the
// caller can log set / clear lines after InTx commits.
func (h *Handler) applyMemoryPressure(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, node store.GetNodeForHeartbeatRow, body *requestBody, now time.Time) (pressureTransitionKind, error) {
	availMib := body.Resources.MemoryAvailableMib
	totalMib := body.Capabilities.MemoryTotalMib
	newSince, newCount, kind := computePressureTransition(
		node.MemoryPressureSince,
		node.MemoryPressureCount,
		&availMib,
		&totalMib,
		h.pressureMemory,
		now,
	)
	if newSince == node.MemoryPressureSince && newCount == node.MemoryPressureCount {
		// No-op write avoidance: pure function returned the same state.
		// Common steady-state path; skipping the UPDATE keeps replication
		// and triggers quiet during stable heartbeats.
		return kind, nil
	}
	if err := tx.UpdateNodeMemoryPressure(ctx, store.UpdateNodeMemoryPressureParams{
		ID:                  nodeID,
		MemoryPressureSince: newSince,
		MemoryPressureCount: newCount,
	}); err != nil {
		return pressureTransitionNone, fmt.Errorf("update memory pressure: %v", err)
	}
	return kind, nil
}

// applySystemDiskPressure mirrors applyMemoryPressure for the root
// filesystem dimension. Both pressures share the same debouncing
// pattern; only the raw metrics (bytes vs MiB) and the configured knobs
// differ. Same no-op write avoidance applies — a heartbeat that does
// not change the pressure state writes nothing.
func (h *Handler) applySystemDiskPressure(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, node store.GetNodeForHeartbeatRow, body *requestBody, now time.Time) (pressureTransitionKind, error) {
	newSince, newCount, kind := computePressureTransition(
		node.SystemDiskPressureSince,
		node.SystemDiskPressureCount,
		body.Resources.SystemDiskAvailableBytes,
		body.Resources.SystemDiskTotalBytes,
		h.pressureSystemDisk,
		now,
	)
	if newSince == node.SystemDiskPressureSince && newCount == node.SystemDiskPressureCount {
		return kind, nil
	}
	if err := tx.UpdateNodeSystemDiskPressure(ctx, store.UpdateNodeSystemDiskPressureParams{
		ID:                      nodeID,
		SystemDiskPressureSince: newSince,
		SystemDiskPressureCount: newCount,
	}); err != nil {
		return pressureTransitionNone, fmt.Errorf("update system_disk pressure: %v", err)
	}
	return kind, nil
}

func (h *Handler) applyNodeUpdate(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, body *requestBody) error {
	caps := body.Capabilities
	capsBlob, err := buildCapabilitiesJSON(caps)
	if err != nil {
		return fmt.Errorf("build capabilities jsonb: %v", err)
	}
	numaBlob, err := buildNumaJSON(caps.NumaTopology)
	if err != nil {
		return fmt.Errorf("build numa jsonb: %v", err)
	}

	migHost, migStart, migEnd, err := resolveMigration(ctx, tx, nodeID, body.Migration)
	if err != nil {
		return err
	}

	params := store.UpdateNodeHeartbeatParams{
		ID:                       nodeID,
		AgentVersion:             ptrString(body.AgentVersion),
		MigrationHost:            migHost,
		MigrationPortRangeStart:  migStart,
		MigrationPortRangeEnd:    migEnd,
		CPUCoresTotal:            ptrInt32(caps.CPUCoresTotal),
		CPUCoresAvailable:        ptrInt32(body.Resources.CPUCoresAvailable),
		CPUModel:                 ptrString(caps.CPUModel),
		CpuFlags:                 nonNilStrings(caps.CPUFlags),
		MemoryTotalMib:           ptrInt64(caps.MemoryTotalMib),
		MemoryAvailableMib:       ptrInt64(body.Resources.MemoryAvailableMib),
		Hugepages2mibTotal:       caps.Hugepages2MibTotal,
		Hugepages1gibTotal:       caps.Hugepages1GibTotal,
		KernelVersion:            ptrString(caps.KernelVersion),
		QEMUVersion:              ptrString(caps.QEMUVersion),
		NumaTopology:             numaBlob,
		Capabilities:             capsBlob,
		SystemDiskTotalBytes:     body.Resources.SystemDiskTotalBytes,
		SystemDiskAvailableBytes: body.Resources.SystemDiskAvailableBytes,
	}
	if err := tx.UpdateNodeHeartbeat(ctx, params); err != nil {
		return fmt.Errorf("update node heartbeat: %v", err)
	}
	return nil
}

// resolveMigration returns the migration triple to write back. When
// the heartbeat carries a Migration block, the agent is asserting an
// updated capability — use it. Otherwise reuse the values currently
// stored on the node so UpdateNodeHeartbeat (which always rewrites
// the columns) is a no-op for that triple.
func resolveMigration(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, mig *migrationCapability) (string, int32, int32, error) {
	if mig != nil {
		return mig.Host, mig.PortRangeStart, mig.PortRangeEnd, nil
	}
	row, err := tx.NodeByID(ctx, nodeID)
	if err != nil {
		return "", 0, 0, fmt.Errorf("reload node migration: %v", err)
	}
	return row.MigrationHost, row.MigrationPortRangeStart, row.MigrationPortRangeEnd, nil
}

func (h *Handler) applyFirmwares(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, reports []firmwareReport) error {
	for _, fr := range reports {
		fwID, err := tx.LookupFirmwareByCatalog(ctx, store.LookupFirmwareByCatalogParams{
			Name:         fr.Name,
			Architecture: store.CPUArch(fr.Architecture),
			Type:         store.FirmwareType(fr.Type),
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.log.WarnContext(ctx, "heartbeat firmware not in catalogue; skipping",
					slog.String("node_id", nodeID.String()),
					slog.String("firmware_name", fr.Name),
					slog.String("firmware_architecture", fr.Architecture),
					slog.String("firmware_type", fr.Type))
				continue
			}
			return fmt.Errorf("firmware lookup: %v", err)
		}
		params := store.UpsertNodeFirmwareParams{
			NodeID:     nodeID,
			FirmwareID: fwID,
			CodePath:   fr.CodePath,
			VarsPath:   fr.VarsTemplatePath,
			Available:  true,
		}
		if err := tx.UpsertNodeFirmware(ctx, params); err != nil {
			return fmt.Errorf("upsert node_firmware: %v", err)
		}
	}
	return nil
}

func (h *Handler) applyVMs(ctx context.Context, tx store.HeartbeatTx, nodeID uuid.UUID, reports []vmReport) error {
	if len(reports) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(reports))
	for _, r := range reports {
		ids = append(ids, r.VMUUID)
	}
	known, err := tx.FilterExistingVMIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("filter vm ids: %v", err)
	}
	knownSet := make(map[uuid.UUID]struct{}, len(known))
	for _, id := range known {
		knownSet[id] = struct{}{}
	}

	for _, r := range reports {
		if _, ok := knownSet[r.VMUUID]; !ok {
			h.log.WarnContext(ctx, "heartbeat references unknown vm; skipping",
				slog.String("node_id", nodeID.String()),
				slog.String("vm_uuid", r.VMUUID.String()))
			continue
		}
		var lastStarted *time.Time
		if r.LastStartedAt != nil {
			t, err := time.Parse(time.RFC3339Nano, *r.LastStartedAt)
			if err != nil {
				h.log.WarnContext(ctx, "heartbeat vm last_started_at not RFC3339; skipping field",
					slog.String("node_id", nodeID.String()),
					slog.String("vm_uuid", r.VMUUID.String()),
					slog.String("value", *r.LastStartedAt))
			} else {
				ts := t.UTC()
				lastStarted = &ts
			}
		}
		obsGen := int64(0)
		if r.ObservedGeneration != nil {
			obsGen = *r.ObservedGeneration
		}
		nodeIDCopy := nodeID
		params := store.UpsertVMRuntimeParams{
			VmID:               r.VMUUID,
			CurrentNodeID:      &nodeIDCopy,
			Phase:              store.VMPhase(r.Phase),
			ObservedGeneration: obsGen,
			QEMUPID:            r.QEMUPID,
			LastStartedAt:      lastStarted,
			LastErrorMessage:   r.LastErrorMessage,
		}
		if err := tx.UpsertVMRuntime(ctx, params); err != nil {
			return fmt.Errorf("upsert vm_runtime: %v", err)
		}
	}
	return nil
}

// projectionError carries an HTTP-shaped failure out of project()
// so the handler entry point can render it through response.WriteError
// without re-classifying every nested call site.
type projectionError struct {
	status  int
	code    response.ErrorCode
	message string
	details map[string]any
}

func (e *projectionError) Error() string {
	return fmt.Sprintf("heartbeat: %d %s: %s", e.status, e.code, e.message)
}

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func ptrInt32(i int32) *int32 {
	v := i
	return &v
}

func ptrInt64(i int64) *int64 {
	v := i
	return &v
}

// nonNilStrings normalises a possibly-nil slice into an empty
// non-nil slice. nodes.cpu_flags is NOT NULL DEFAULT '{}', so a nil
// slice would surface as a NOT NULL violation when written through
// the heartbeat update.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
