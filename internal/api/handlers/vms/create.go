// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// vmCreateRequest is the body of POST /v1/vms. `vcpus` / `memory_mb`
// translate to `cpu_cores` / `memory_mib` at the handler boundary;
// pool lands on vm_disks.storage_pool_id.
//
// The `template` field is a template name; UUID literals are
// rejected with 400 validation_failed at the resolver. The `pool`
// field stays polymorphic per the multi-instance carve-out (either
// a pool name or a per-instance UUID literal). There are no
// `template_id` / `pool_id` UUID-only fields - the field rename is
// locked, not a transitional dual-shape.
//
// Multi-instance pools + scheduler:
//   - `pool` is optional. When omitted (or empty), the handler reads
//     cluster_settings.default_pool_name; if unset, returns 400
//     default_pool_not_set.
//   - `node` is an optional placement hint. When provided, the
//     scheduler restricts the candidate list to exactly that node;
//     if the node does not host the pool, returns 409 pool_not_on_node.
//
// Architecture is NOT a request field - it is resolved from the
// template (single-arch-per-template invariant).
type vmCreateRequest struct {
	Name     string  `json:"name"`
	Template string  `json:"template"`
	Pool     string  `json:"pool,omitempty"`
	Node     *string `json:"node,omitempty"`
	VCPUs    int     `json:"vcpus"`
	MemoryMB int     `json:"memory_mb"`
	// UserData is an optional VM-level cloud-init override (L3
	// Area 3 lock). When provided, fully replaces the template's
	// baked cloud_init_user_data in the per-VM resolved blob; the
	// agent receives whichever wins. Stored verbatim in vms.user_data
	// so the resolution stays a pure function of the VM row.
	UserData *string `json:"user_data,omitempty"`
	// CloudInitDisabled is the explicit-disable signal (operator UX
	// iteration). When true, the resolver returns empty user_data to
	// the agent even if the template has a baked
	// cloud_init_user_data — the agent skips cidata.iso generation.
	// Mutually exclusive with UserData; sending both surfaces as 400
	// validation_failed. Defaults to false; the DB CHECK
	// chk_vms_cloud_init_disabled_no_userdata is the schema-level
	// backstop for the same invariant.
	CloudInitDisabled bool `json:"cloud_init_disabled,omitempty"`
}

// errVMTemplateForbidden is the in-flight signal that the
// template-usability composite check rejected the request. Mapped to
// 404 (no leak) — every authenticated role holds template:read:public,
// so a 403 here would lie about visibility.
var errVMTemplateForbidden = errors.New("template not accessible")

// errVMNameInUse marks a 23505 unique-violation on vms.name surfaced
// to the wire as 409 vm_name_in_use.
var errVMNameInUse = errors.New("vm name already in use")

// poolNotWritableError signals that the scheduler's chosen pool has
// a storage type vm.create cannot drive (only `local_dir` today). It
// flows as a typed error through the create transaction and surfaces
// at the wire as 400 pool_not_writable.
type poolNotWritableError struct {
	poolType string
}

func (e *poolNotWritableError) Error() string {
	return fmt.Sprintf("vms: pool type %q not writable", e.poolType)
}

// Create implements POST /v1/vms — the operator-callable entry point
// to the vm.create async pipeline.
//
// Permission gate: vm:create. RequirePermission middleware admits
// admin / operator / developer (matrix has each at scope=any). The
// handler-side composite check is template-usability — same shape as
// templates.image_import.checkImportAccess but keyed on
// `template:use`.
//
// HA-safe atomic enqueue: store.LockKeyPlacement is acquired as the
// first statement inside
// the create transaction, so concurrent api-server replicas serialize
// their placement decisions cluster-wide. Scheduler reads (candidate
// pools + heartbeat metrics) and subsequent inserts (vms + vm_disks +
// tasks + river job + UpdateTaskRiverJobID) live under the same
// transaction-scoped lock; commit/rollback releases it.
//
// Returns 202 + AsyncTaskAccepted; clients poll /v1/tasks/{task_id}
// for completion.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	req, ok := decodeCreateBody(w, r)
	if !ok {
		return
	}
	if !validateCreateRequest(w, r, req) {
		return
	}

	template, err := resolver.Template(r.Context(), h.store.Queries(), req.Template)
	if err != nil {
		writeTemplateLoadError(w, r, err)
		return
	}
	if err := checkTemplateUseAccess(caller, template); err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "template not found", nil)
		return
	}

	poolName, ok := h.resolvePoolName(w, r, req.Pool)
	if !ok {
		return
	}

	taskID, err := h.scheduleAndEnqueueCreate(r.Context(), scheduleInputs{
		Caller:   caller,
		Template: template,
		PoolName: poolName,
		Req:      req,
	})
	if err != nil {
		h.writeCreateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// decodeCreateBody parses the JSON body and writes the standard 400
// envelope on malformed input. Returns ok=false to short-circuit the
// caller.
func decodeCreateBody(w http.ResponseWriter, r *http.Request) (vmCreateRequest, bool) {
	var req vmCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return vmCreateRequest{}, false
	}
	return req, true
}

// validateCreateRequest enforces the field-level invariants. The
// schema also carries CHECK constraints (vms.cpu_cores / memory_mib)
// so a defective request fails twice; the API-edge check produces a
// friendlier envelope than a raw 23514. Identifier well-formedness
// (template / pool) is deferred to the resolver layer — it accepts
// either UUID literals or names and returns a 404 for either branch
// when no row matches.
func validateCreateRequest(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	if req.Name == "" || len(req.Name) > 255 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name is required (1..255 chars)", nil)
		return false
	}
	if req.Template == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "template is required", nil)
		return false
	}
	if req.VCPUs < 1 || req.VCPUs > 128 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "vcpus must be in [1, 128]", nil)
		return false
	}
	if req.MemoryMB < 128 || req.MemoryMB > 524288 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "memory_mb must be in [128, 524288]", nil)
		return false
	}
	// Mutual-exclusion check for the three-state cloud-init contract.
	// The DB CHECK is the durable backstop; this edge check produces
	// a friendlier 400 envelope than the raw 23514 the handler would
	// otherwise propagate.
	if req.CloudInitDisabled && req.UserData != nil && *req.UserData != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"user_data and cloud_init_disabled are mutually exclusive",
			map[string]any{"conflicting_fields": []string{"user_data", "cloud_init_disabled"}})
		return false
	}
	return true
}

// checkTemplateUseAccess enforces the template-usability composite
// rule. Mirrors templates.image_import.checkImportAccess but uses
// template:use as the per-resource permission. Returns nil when the
// caller may use the template; errVMTemplateForbidden otherwise.
func checkTemplateUseAccess(caller *auth.User, template store.Template) error {
	scope := auth.ScopeFor(caller.Role, auth.PermTemplateUse)
	if scope == auth.ScopeAny {
		return nil
	}
	if scope == auth.ScopeOwn && template.OwnerID == caller.ID {
		return nil
	}
	if auth.Has(caller.Role, auth.PermTemplateReadPublic) && template.Visibility == "public" {
		return nil
	}
	return errVMTemplateForbidden
}

// scheduleInputs bundles the resolved entities scheduleAndEnqueueCreate
// consumes. Decouples the long argument list from the public Create
// handler signature.
type scheduleInputs struct {
	Caller   *auth.User
	Template store.Template
	PoolName string
	Req      vmCreateRequest
}

// scheduleAndEnqueueCreate runs the atomic critical section: acquire
// the cluster-wide placement lock, score candidates with the configured
// algorithm, and persist vms/vm_disks/tasks rows + the river job in one
// transaction. Returns the freshly-minted task id on success.
//
// Lock scope matters: pg_advisory_xact_lock releases on commit/rollback,
// so the scheduler's reads (ListEligiblePoolsByName + CountRunningVMs‑
// ByNode) and the CreateVM write (pinned_node_id) MUST share a single
// transaction. Otherwise concurrent api-server replicas can observe
// stale candidate availability and double-allocate. See store.LockKey‑
// Placement.
func (h *Handler) scheduleAndEnqueueCreate(ctx context.Context, in scheduleInputs) (uuid.UUID, error) {
	vmID := uuid.New()
	taskID := uuid.New()
	createdBy := in.Caller.ID
	resID := vmID

	err := h.store.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		if err := q.AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
			return fmt.Errorf("acquire placement lock: %v", err)
		}

		// Disk requirement is derived from the template's
		// default_disk_gib in the single-root-disk model.
		// default_disk_gib is INT NOT NULL with a
		// CHECK between 1 and 65536, so the multiplication is well-
		// defined and cannot overflow int64.
		diskBytes := int64(in.Template.DefaultDiskGib) * 1073741824
		decision, err := scheduler.SchedulePlacement(ctx, q, scheduler.PlacementRequest{
			PoolName:  in.PoolName,
			NodeHint:  in.Req.Node,
			VCPUs:     in.Req.VCPUs,
			MemoryMiB: in.Req.MemoryMB,
			DiskBytes: diskBytes,
		}, scheduler.PlacementConfig{
			Algorithm: h.placementAlgorithm,
			Resources: h.placementResources,
		})
		if err != nil {
			return err
		}
		if decision.PoolInstance.Type != "local_dir" {
			return &poolNotWritableError{poolType: decision.PoolInstance.Type}
		}

		argsJSON, err := json.Marshal(map[string]any{
			"vm_id":       vmID.String(),
			"template_id": in.Template.ID.String(),
			"pool_id":     decision.PoolInstance.ID.String(),
			"node_id":     decision.Node.ID.String(),
		})
		if err != nil {
			return fmt.Errorf("marshal task args: %v", err)
		}

		nodeID := decision.Node.ID
		if _, err := q.CreateVM(ctx, store.CreateVMParams{
			ID:                vmID,
			OwnerID:           in.Caller.ID,
			Name:              in.Req.Name,
			Description:       "",
			TemplateID:        ptrUUID(in.Template.ID),
			Architecture:      in.Template.Architecture,
			CpuCores:          int32(in.Req.VCPUs),    //nolint:gosec // bounded to 1..128 by validateCreateRequest
			MemoryMib:         int32(in.Req.MemoryMB), //nolint:gosec // bounded to 128..524288 by validateCreateRequest
			CPUModel:          "host",
			MachineType:       machineTypeFor(in.Template.Architecture),
			FirmwareID:        nil,
			PinnedNodeID:      &nodeID,
			UserData:          in.Req.UserData,
			CloudInitDisabled: in.Req.CloudInitDisabled,
			Labels:            []byte(`{}`),
		}); err != nil {
			return err
		}
		if _, err := q.CreateVMDisk(ctx, store.CreateVMDiskParams{
			VmID:             vmID,
			StoragePoolID:    decision.PoolInstance.ID,
			DeviceOrder:      0,
			Bus:              store.DiskBusVirtio,
			SizeGib:          in.Template.DefaultDiskGib,
			SourceKind:       "template",
			SourceTemplateID: ptrUUID(in.Template.ID),
			Format:           in.Template.ImageFormat,
			ReadOnly:         false,
			CacheMode:        store.DiskCacheModeWriteback,
			Discard:          store.DiskDiscardUnmap,
			BootOrder:        nil,
		}); err != nil {
			return err
		}
		if _, err := q.CreateTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.create",
			Status:       store.TaskStatusPending,
			ResourceType: "vm",
			ResourceID:   &resID,
			Args:         argsJSON,
			MaxAttempts:  25,
			CreatedBy:    &createdBy,
		}); err != nil {
			return err
		}
		insertResult, err := h.riverClient.InsertTx(ctx, tx, VMCreateArgs{
			TaskID:     taskID,
			VMID:       vmID,
			TemplateID: in.Template.ID,
			PoolID:     decision.PoolInstance.ID,
			NodeID:     decision.Node.ID,
		}, nil)
		if err != nil {
			return err
		}
		jobID := insertResult.Job.ID
		return q.UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
			ID:         taskID,
			RiverJobID: &jobID,
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_vms_name" {
			return uuid.Nil, errVMNameInUse
		}
		return uuid.Nil, err
	}
	return taskID, nil
}

// writeTemplateLoadError maps a resolver.Template error to a wire
// envelope. CodeNotFound -> 404 not_found (template missing or
// invisible - the composite check below would also reject, but
// loading first keeps the path linear). UUID literals in the
// `template` body field surface as 400 validation_failed.
func writeTemplateLoadError(w http.ResponseWriter, r *http.Request, err error) {
	if resolver.IsUUIDInName(err) {
		response.WriteUUIDNotAllowedError(w, r, "template", "template")
		return
	}
	if resolver.IsNotFound(err) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "template not found", nil)
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "load template", nil)
}

// resolvePoolName returns the effective pool name for the create
// request. It accepts:
//
//   - empty `pool`: fall back to cluster_settings.default_pool_name;
//     missing default → 400 default_pool_not_set.
//   - UUID literal: look up the storage_pools row by id and return
//     that instance's name (callers retain UUID-based addressing as
//     a backward-compatible escape hatch into the scheduler's
//     name-driven placement).
//   - bare string: passed through as a pool name; the scheduler
//     resolves to instances.
//
// The boolean second return mirrors the decode* helpers' short-circuit
// signal.
func (h *Handler) resolvePoolName(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	if requested == "" {
		settings, err := h.store.Queries().GetClusterSettings(r.Context())
		if err != nil {
			h.log.ErrorContext(r.Context(), "load cluster settings", "error", err)
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load cluster settings", nil)
			return "", false
		}
		if settings.DefaultPoolName == nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeDefaultPoolNotSet,
				"no pool specified and no cluster default pool configured", nil)
			return "", false
		}
		return *settings.DefaultPoolName, true
	}
	if id, err := uuid.Parse(requested); err == nil {
		row, err := h.store.Queries().GetStoragePoolByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				response.WriteError(w, r, http.StatusNotFound,
					response.CodePoolNotFound, "storage pool not found", nil)
				return "", false
			}
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load storage pool", nil)
			return "", false
		}
		return row.Name, true
	}
	return requested, true
}

// writeCreateError dispatches the merged scheduleAndEnqueueCreate
// error to a wire envelope. Categories, in priority order:
//
//   - ErrInsufficientResources (structured details) → 409 no_eligible_nodes
//   - Other scheduler sentinels                     → 400 / 404 / 409
//   - poolNotWritableError                          → 400 pool_not_writable
//   - errVMNameInUse                                → 409 vm_name_in_use
//   - any other error                               → 500 internal
//
// The insufficient-resources path wins over the bare no_eligible_nodes
// sentinel — its envelope carries an actionable utilization payload.
func (h *Handler) writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	var insufficient *insufficientResourcesView
	if extractInsufficient(err, &insufficient) {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeNoEligibleNodes,
			"no nodes have sufficient resources for the requested VM",
			insufficient.toDetails())
		return
	}

	switch {
	case errors.Is(err, scheduler.ErrNodeHintIsUUID):
		response.WriteUUIDNotAllowedError(w, r, "node", "node")
		return
	case errors.Is(err, scheduler.ErrPoolNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodePoolNotFound,
			"no pool with this name exists in the cluster", nil)
		return
	case errors.Is(err, scheduler.ErrPoolNotOnNode):
		response.WriteError(w, r, http.StatusConflict,
			response.CodePoolNotOnNode,
			"requested node does not host an instance of the requested pool", nil)
		return
	case errors.Is(err, scheduler.ErrNoEligibleNodes):
		details := buildNoEligibleDetails(err)
		response.WriteError(w, r, http.StatusConflict,
			response.CodeNoEligibleNodes,
			"pool exists but no node is currently ready and uncordoned",
			details)
		return
	}

	var poolErr *poolNotWritableError
	if errors.As(err, &poolErr) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodePoolNotWritable,
			"storage pool type does not support vm.create",
			map[string]any{"pool_type": poolErr.poolType})
		return
	}

	if errors.Is(err, errVMNameInUse) {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNameInUse, "vm name already in use", nil)
		return
	}

	h.log.ErrorContext(r.Context(), "vms.create enqueue failed", "error", err)
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "enqueue vm task", nil)
}

// insufficientResourcesView is the handler-edge projection of the
// scheduler's structured detail payload. It carries enough metadata to
// build the `details` map on the 409 envelope without leaking scheduler-
// internal field types into the wire layer.
type insufficientResourcesView struct {
	requiredCPU          int32
	requiredMemMiB       int32
	requiredDiskBytes    int64
	candidatesConsidered int
	candidatesEligible   int
	nodes                []scheduler.NodeUtilization
}

// toDetails renders the view into the `details` map expected by
// response.WriteError. `reason` is a stable string the operator (or a
// CLI) can branch on to detect the resource-shortage subcase. Disk
// fields (pool, disk_used_bytes, disk_total_bytes) on each utilization
// entry land under additionalProperties — the OpenAPI envelope is
// free-form.
func (v *insufficientResourcesView) toDetails() map[string]any {
	utilization := make([]map[string]any, 0, len(v.nodes))
	for _, n := range v.nodes {
		utilization = append(utilization, map[string]any{
			"node":             n.Node,
			"pool":             n.Pool,
			"cpu_used":         n.CPUUsed,
			"cpu_total":        n.CPUTotal,
			"mem_used_mib":     n.MemUsedMiB,
			"mem_total_mib":    n.MemTotalMiB,
			"disk_used_bytes":  n.DiskUsedBytes,
			"disk_total_bytes": n.DiskTotalBytes,
		})
	}
	return map[string]any{
		"reason": "insufficient_resources",
		"required": map[string]any{
			"cpu_cores":  v.requiredCPU,
			"memory_mib": v.requiredMemMiB,
			"disk_bytes": v.requiredDiskBytes,
		},
		"candidates_considered": v.candidatesConsidered,
		"candidates_eligible":   v.candidatesEligible,
		"node_utilization":      utilization,
	}
}

// buildNoEligibleDetails projects an ErrNoEligibleNodes chain into the
// 409 envelope's `details` map. When the chain carries a
// NodePressureDetail the payload includes
// `reason="node_pressure"` and a per-node breakdown so operators can see
// which hosts were filtered out by an active pressure condition. The
// bare sentinel (pool exists but every host is cordoned / unreachable
// / soft-deleted) returns nil, keeping the bare envelope shape that
// pre-pressure clients already understand.
//
// Each entry emits the timestamps for whichever pressure conditions are
// active on it and nothing else: PressuredNode's three `*time.Time`
// fields are nullable (present only when the matching
// condition is in `Conditions`), so the nil-checks are both a wire
// hygiene requirement (omit absent fields) and a runtime correctness
// requirement (calling a method on a nil *time.Time panics).
// Pool-scoped entries (disk_pressure) additionally surface the `pool`
// name so the operator can distinguish a node-wide problem from a
// per-pool exhaustion.
func buildNoEligibleDetails(err error) map[string]any {
	pressure, ok := scheduler.ExtractNodePressureDetail(err)
	if !ok || pressure == nil || len(pressure.Nodes) == 0 {
		return nil
	}
	filtered := make([]map[string]any, 0, len(pressure.Nodes))
	for _, n := range pressure.Nodes {
		entry := map[string]any{
			"node":       n.Node,
			"conditions": n.Conditions,
		}
		if n.Pool != "" {
			entry["pool"] = n.Pool
		}
		if t := n.MemoryPressureSince; t != nil {
			entry["memory_pressure_since"] = t.UTC().Format(time.RFC3339Nano)
		}
		if t := n.SystemDiskPressureSince; t != nil {
			entry["system_disk_pressure_since"] = t.UTC().Format(time.RFC3339Nano)
		}
		if t := n.DiskPressureSince; t != nil {
			entry["disk_pressure_since"] = t.UTC().Format(time.RFC3339Nano)
		}
		filtered = append(filtered, entry)
	}
	return map[string]any{
		"reason":                   "node_pressure",
		"filtered_due_to_pressure": filtered,
	}
}

// extractInsufficient walks the error chain for the scheduler's
// resource-shortage payload. Returns true (with `out` populated) when
// the wrapped error is the insufficient-resources sentinel.
func extractInsufficient(err error, out **insufficientResourcesView) bool {
	detail, ok := scheduler.ExtractInsufficientResources(err)
	if !ok {
		return false
	}
	if detail == nil {
		*out = &insufficientResourcesView{}
		return true
	}
	*out = &insufficientResourcesView{
		requiredCPU:          detail.RequiredCPU,
		requiredMemMiB:       detail.RequiredMemMiB,
		requiredDiskBytes:    detail.RequiredDiskBytes,
		candidatesConsidered: detail.CandidatesConsidered,
		candidatesEligible:   detail.CandidatesEligible,
		nodes:                detail.NodeUtilization,
	}
	return true
}

// ptrUUID returns a non-nil pointer to id. The store params expect
// nullable pointers for FKs that are nullable at the schema layer
// (templates.id is nullable on vms via SET NULL); the handler always
// has a real id so we wrap it.
func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }
