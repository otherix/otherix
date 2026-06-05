// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// vmCreateRequest is the body of POST /v1/vms. `vcpus` / `memory_mb`
// translate to `cpu_cores` / `memory_mib` at the handler boundary;
// pool lands on vm_disks.storage_pool_id.
//
// The image source is supplied directly (`image_url` + optional
// `image_sha256` content pin + `format`): the VM row is self-describing,
// there is no template entity. `arch` is a required request field
// (amd64 | arm64). Firmware is resolved from `firmware` ("bios" | "uefi",
// default uefi) for the (arch, type) default, or from an explicit
// `firmware_id` uuid escape hatch.
//
// The `pool` field stays polymorphic per the multi-instance carve-out
// (either a pool name or a per-instance UUID literal).
//
// Multi-instance pools + scheduler:
//   - `pool` is optional. When omitted (or empty), the handler reads
//     cluster_settings.default_pool_name; if unset, returns 400
//     default_pool_not_set.
//   - `node` is an optional placement hint. When provided, the
//     scheduler restricts the candidate list to exactly that node;
//     if the node does not host the pool, returns 409 pool_not_on_node.
type vmCreateRequest struct {
	Name         string `json:"name"`
	ImageURL     string `json:"image_url"`
	ImageSHA256  string `json:"image_sha256,omitempty"`
	Architecture string `json:"arch"`
	// Firmware selects the firmware type for the default-firmware lookup:
	// "bios" or "uefi" (default uefi). Ignored when FirmwareID is set.
	Firmware string `json:"firmware,omitempty"`
	// FirmwareID is the explicit firmware-row escape hatch (uuid). When set
	// it wins over Firmware.
	FirmwareID string `json:"firmware_id,omitempty"`
	// Format is the image disk format ("qcow2" | "raw"); defaults to qcow2.
	Format  string  `json:"format,omitempty"`
	DiskGiB int     `json:"disk_gib,omitempty"`
	Pool    string  `json:"pool,omitempty"`
	Node    *string `json:"node,omitempty"`
	// Network is an optional network to attach a single NIC to. Accepts
	// either a network name or a uuid literal, of type bridge or overlay.
	// When omitted the VM is created with no NIC (the agent falls back to
	// legacy SLIRP user-mode networking). Other network types are rejected
	// with 400.
	Network  string `json:"network,omitempty"`
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	// UserData is an optional VM-level cloud-init override (L3
	// Area 3 lock). Stored verbatim in vms.user_data so the resolution
	// stays a pure function of the VM row.
	UserData *string `json:"user_data,omitempty"`
	// CloudInitDisabled is the explicit-disable signal. When true, the
	// resolver returns empty user_data to the agent and the agent skips
	// cidata.iso generation. Mutually exclusive with UserData; sending both
	// surfaces as 400 validation_failed.
	CloudInitDisabled bool `json:"cloud_init_disabled,omitempty"`
}

// errFirmwareNotFound is the in-flight signal that firmware resolution found no
// matching row (explicit id missing, or no default for the arch/type pair). The
// caller maps it to a 404 / validation_failed wire envelope.
var errFirmwareNotFound = errors.New("firmware not found")

// errFirmwareBadID is the in-flight signal that an explicit firmware_id failed
// to parse as a uuid. The caller maps it to 400 validation_failed.
var errFirmwareBadID = errors.New("firmware_id is not a valid uuid")

// errFirmwareBadType is the in-flight signal that `firmware` carried a value
// other than "bios" / "uefi". The caller maps it to 400 validation_failed.
var errFirmwareBadType = errors.New("firmware must be bios or uefi")

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
// admin / operator / developer (matrix has each at scope=any). The image
// source is supplied directly on the request, so there is no
// template-usability composite check.
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

	firmwareID, ok := h.resolveFirmwareForRequest(w, r, req)
	if !ok {
		return
	}

	poolName, ok := h.resolvePoolName(w, r, req.Pool)
	if !ok {
		return
	}

	network, ok := h.resolveNetwork(w, r, req.Network)
	if !ok {
		return
	}

	taskID, err := h.scheduleAndEnqueueCreate(r.Context(), scheduleInputs{
		Caller:     caller,
		FirmwareID: firmwareID,
		PoolName:   poolName,
		Network:    network,
		Req:        req,
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

// validateCreateImageFields enforces the image-source field invariants:
// image_url present, arch in {amd64, arm64}, image_sha256 (when set) a 64-char
// lowercase hex digest, format (when set) in {qcow2, raw}, and disk_gib >= 0.
// Returns false (after writing the 400 envelope) on the first violation.
func validateCreateImageFields(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	if req.ImageURL == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "image_url is required", nil)
		return false
	}
	if req.Architecture != string(store.CpuArchAmd64) && req.Architecture != string(store.CpuArchArm64) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "arch must be one of: amd64, arm64", nil)
		return false
	}
	if req.ImageSHA256 != "" {
		if err := validation.ValidateImageChecksumSHA256(req.ImageSHA256); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "image_sha256 must be 64-char lowercase hex", nil)
			return false
		}
	}
	if req.Format != "" {
		if err := validation.ValidateImageFormat(req.Format); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "format must be one of: qcow2, raw", nil)
			return false
		}
	}
	if req.DiskGiB < 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "disk_gib must be >= 0", nil)
		return false
	}
	return true
}

// validateCreateRequest enforces the field-level invariants. The
// schema also carries CHECK constraints (vms.cpu_cores / memory_mib)
// so a defective request fails twice; the API-edge check produces a
// friendlier envelope than a raw 23514. Identifier well-formedness for
// `pool` / `network` / `firmware_id` is deferred to the resolver / firmware
// lookup layers.
func validateCreateRequest(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	if req.Name == "" || len(req.Name) > 255 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name is required (1..255 chars)", nil)
		return false
	}
	if !validateCreateImageFields(w, r, req) {
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

// resolveFirmwareForRequest resolves the firmware id for a create request and
// writes the matching wire envelope on failure (returning ok=false to
// short-circuit Create). An unparseable firmware_id is 400 validation_failed; a
// missing firmware row (explicit id or no default for arch/type) is 404
// not_found; a bad `firmware` type string is 400 validation_failed.
func (h *Handler) resolveFirmwareForRequest(w http.ResponseWriter, r *http.Request, req vmCreateRequest) (*uuid.UUID, bool) {
	id, err := h.resolveFirmware(r.Context(), store.CPUArch(req.Architecture), req.Firmware, req.FirmwareID)
	switch {
	case err == nil:
		return id, true
	case errors.Is(err, errFirmwareBadID):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "firmware_id is not a valid uuid", nil)
	case errors.Is(err, errFirmwareBadType):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "firmware must be one of: bios, uefi", nil)
	case errors.Is(err, errFirmwareNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "no matching firmware", nil)
	default:
		h.log.ErrorContext(r.Context(), "resolve firmware", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve firmware", nil)
	}
	return nil, false
}

// resolveFirmware resolves the firmware id for a VM create: an explicit
// firmware_id wins; otherwise the default firmware for (arch, type) where type
// is "uefi" unless firmware=="bios". Returns a sentinel (errFirmwareBadID /
// errFirmwareBadType / errFirmwareNotFound) the caller maps to a wire envelope.
func (h *Handler) resolveFirmware(ctx context.Context, arch store.CPUArch, firmware, firmwareID string) (*uuid.UUID, error) {
	if firmwareID != "" {
		id, err := uuid.Parse(firmwareID)
		if err != nil {
			return nil, errFirmwareBadID
		}
		fw, err := h.store.FirmwareByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errFirmwareNotFound
			}
			return nil, fmt.Errorf("load firmware by id: %v", err)
		}
		return &fw.ID, nil
	}

	var ftype store.FirmwareType
	switch firmware {
	case "", string(store.FirmwareTypeUefi):
		ftype = store.FirmwareTypeUefi
	case string(store.FirmwareTypeBios):
		ftype = store.FirmwareTypeBios
	default:
		return nil, errFirmwareBadType
	}
	fw, err := h.store.DefaultFirmwareForArchType(ctx, arch, ftype)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Firmware is optional (the VM row's firmware_id is nullable). When
			// no default firmware is seeded for this arch/type, the VM boots
			// with the agent/qemu built-in default rather than failing create.
			// An explicit firmware_id that misses still 404s above.
			return nil, nil
		}
		return nil, fmt.Errorf("load default firmware: %v", err)
	}
	return &fw.ID, nil
}

// scheduleInputs bundles the resolved entities scheduleAndEnqueueCreate
// consumes. Decouples the long argument list from the public Create
// handler signature.
type scheduleInputs struct {
	Caller     *auth.User
	FirmwareID *uuid.UUID
	PoolName   string
	Network    *store.Network
	Req        vmCreateRequest
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

	// The store owns the placement-locked transaction (lock release on
	// commit/rollback keeps the scheduler reads and the pinned-node
	// write atomic) and the enqueue; this plan callback acquires the
	// lock through the tx-bound PlacementReader, scores candidates with
	// the scheduler (the reader is assignable to scheduler.Querier), and
	// builds the rows + job args from the chosen instance. A uq_vms_name
	// violation is translated to store.ErrVMNameInUse by the store.
	return createWithMACRetry(ctx, h.store, func(pr store.PlacementReader) (store.VMCreateWrites, error) {
		if err := pr.AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
			return store.VMCreateWrites{}, fmt.Errorf("acquire placement lock: %v", err)
		}

		// Disk reservation for placement is derived from the requested
		// disk_gib. When disk_gib is 0 the true size is unknown until the
		// agent sizes the root disk to the image's virtual size, so we
		// reserve 0 (no disk filter) and let the agent materialize it. A
		// non-zero request reserves that many GiB.
		diskBytes := int64(in.Req.DiskGiB) * 1073741824
		// The network-aware filter excludes nodes where a requested
		// network failed to reconcile (ADR 0034 NL18). One network per
		// VM today (single vm_nics row from --network); the slice keeps
		// the contract open for multi-NIC VMs without a signature change.
		var networkIDs []uuid.UUID
		if in.Network != nil {
			networkIDs = []uuid.UUID{in.Network.ID}
		}
		decision, err := scheduler.SchedulePlacement(ctx, pr, scheduler.PlacementRequest{
			PoolName:   in.PoolName,
			NodeHint:   in.Req.Node,
			VCPUs:      in.Req.VCPUs,
			MemoryMiB:  in.Req.MemoryMB,
			DiskBytes:  diskBytes,
			NetworkIDs: networkIDs,
		}, scheduler.PlacementConfig{
			Algorithm: h.placementAlgorithm,
			Resources: h.placementResources,
		})
		if err != nil {
			return store.VMCreateWrites{}, err
		}
		if decision.PoolInstance.Type != "local_dir" {
			return store.VMCreateWrites{}, &poolNotWritableError{poolType: decision.PoolInstance.Type}
		}

		argsJSON, err := json.Marshal(map[string]any{
			"vm_id":   vmID.String(),
			"pool_id": decision.PoolInstance.ID.String(),
			"node_id": decision.Node.ID.String(),
		})
		if err != nil {
			return store.VMCreateWrites{}, fmt.Errorf("marshal task args: %v", err)
		}

		var nic *store.CreateVMNicParams
		if in.Network != nil {
			mac, macErr := generateLocalMAC()
			if macErr != nil {
				return store.VMCreateWrites{}, fmt.Errorf("generate nic mac: %v", macErr)
			}
			nic = &store.CreateVMNicParams{
				ID:          uuid.New(),
				VmID:        vmID,
				NetworkID:   in.Network.ID,
				DeviceOrder: 0,
				Model:       store.NicModelVirtio,
				MacAddress:  mac,
			}
		}

		arch := store.CPUArch(in.Req.Architecture)
		format := store.ImageFormatQcow2
		if in.Req.Format != "" {
			format = store.ImageFormat(in.Req.Format)
		}
		var imageSHA []byte
		if in.Req.ImageSHA256 != "" {
			// Validated 64-char lowercase hex at the API edge, so DecodeString
			// cannot fail; the error path is defense-in-depth.
			imageSHA, err = hex.DecodeString(in.Req.ImageSHA256)
			if err != nil {
				return store.VMCreateWrites{}, fmt.Errorf("decode image_sha256: %v", err)
			}
		}

		nodeID := decision.Node.ID
		return store.VMCreateWrites{
			VM: store.CreateVMParams{
				ID:                vmID,
				OwnerID:           in.Caller.ID,
				Name:              in.Req.Name,
				Description:       "",
				Architecture:      arch,
				ImageURL:          in.Req.ImageURL,
				ImageSHA256:       imageSHA,
				ImageFormat:       format,
				CpuCores:          int32(in.Req.VCPUs),    //nolint:gosec // bounded to 1..128 by validateCreateRequest
				MemoryMib:         int32(in.Req.MemoryMB), //nolint:gosec // bounded to 128..524288 by validateCreateRequest
				CPUModel:          "host",
				MachineType:       machineTypeFor(arch),
				FirmwareID:        in.FirmwareID,
				PinnedNodeID:      &nodeID,
				UserData:          in.Req.UserData,
				CloudInitDisabled: in.Req.CloudInitDisabled,
				Labels:            []byte(`{}`),
			},
			Disk: store.CreateVMDiskParams{
				VmID:          vmID,
				StoragePoolID: decision.PoolInstance.ID,
				DeviceOrder:   0,
				Bus:           store.DiskBusVirtio,
				SizeGib:       int32(in.Req.DiskGiB), //nolint:gosec // bounded >= 0 by validateCreateRequest
				SourceKind:    "image",
				Format:        format,
				ReadOnly:      false,
				CacheMode:     store.DiskCacheModeWriteback,
				Discard:       store.DiskDiscardUnmap,
				BootOrder:     nil,
			},
			Nic: nic,
			Task: store.CreateTaskParams{
				ID:           taskID,
				Type:         "vm.create",
				Status:       store.TaskStatusPending,
				ResourceType: "vm",
				ResourceID:   &resID,
				Args:         argsJSON,
				MaxAttempts:  25,
				CreatedBy:    &createdBy,
			},
			Job: VMCreateArgs{
				TaskID:      taskID,
				VMID:        vmID,
				PoolID:      decision.PoolInstance.ID,
				NodeID:      decision.Node.ID,
				ImageURL:    in.Req.ImageURL,
				ImageSHA256: in.Req.ImageSHA256,
				Format:      string(format),
				DiskGiB:     in.Req.DiskGiB,
			},
		}, nil
	})
}

// createWithMACRetry calls CreateScheduledVM, re-minting a colliding NIC
// MAC up to maxMACRetries times. A locally-administered MAC collision on a
// network is astronomically rare; re-minting makes it transparent instead
// of a 5xx. plan mints a fresh MAC each call, so a retry re-rolls the MAC.
// The loop breaks on any non-conflict outcome (success or a different
// error). A persistent conflict after maxMACRetries attempts returns the
// last ErrVMNicMACConflict, which the handler maps to 500.
func createWithMACRetry(ctx context.Context, st Store, plan func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error) {
	const maxMACRetries = 8
	var (
		id  uuid.UUID
		err error
	)
	for attempt := 0; attempt < maxMACRetries; attempt++ {
		// Each MAC-conflict retry burns a job-sequence number via enqueueJobOp/nextJobSeq (the seq counter advances immediately), so retries leave benign gaps in the sequence - not lost jobs: the job OpPut rides in the failed txn's Then and never commits, so no orphan job row is written.
		id, err = st.CreateScheduledVM(ctx, plan)
		if !errors.Is(err, store.ErrVMNicMACConflict) {
			return id, err
		}
	}
	return id, err
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
		settings, err := h.store.ClusterSettings(r.Context())
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
		row, err := h.store.StoragePoolByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
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

// resolveNetwork resolves the optional `network` request field to a network
// row. It accepts:
//
//   - empty: no NIC is attached (nil, true) — legacy SLIRP fallback.
//   - uuid literal: looked up by id; unknown id → 404 not_found.
//   - bare string: looked up by name; unknown name → 404 not_found.
//
// Both `bridge` and `overlay` networks are attachable; any other type is
// rejected with 400. The boolean second return mirrors the other resolve*
// helpers' short-circuit signal.
func (h *Handler) resolveNetwork(w http.ResponseWriter, r *http.Request, requested string) (*store.Network, bool) {
	if requested == "" {
		return nil, true
	}
	var (
		net store.Network
		err error
	)
	if id, perr := uuid.Parse(requested); perr == nil {
		net, err = h.store.NetworkByID(r.Context(), id)
	} else {
		net, err = h.store.NetworkByName(r.Context(), requested)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "network not found", nil)
			return nil, false
		}
		h.log.ErrorContext(r.Context(), "load network", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load network", nil)
		return nil, false
	}
	switch net.Type {
	case store.NetworkTypeBridge, store.NetworkTypeOverlay:
		// attachable
	default:
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"network type cannot be attached at vm create",
			map[string]any{"network_type": string(net.Type)})
		return nil, false
	}
	return &net, true
}

// writeCreateError dispatches the merged scheduleAndEnqueueCreate
// error to a wire envelope. Categories, in priority order:
//
//   - ErrInsufficientResources (structured details) → 409 no_eligible_nodes
//   - Other scheduler sentinels                     → 400 / 404 / 409
//   - poolNotWritableError                          → 400 pool_not_writable
//   - errVMNameInUse                                → 409 vm_name_in_use
//   - ErrVMNicMACConflict (retries exhausted)       → 500 internal
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

	if errors.Is(err, store.ErrVMNameInUse) {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNameInUse, "vm name already in use", nil)
		return
	}

	// A NIC MAC conflict that survives createWithMACRetry's bounded
	// re-mint loop is not a client error - it signals a sustained
	// collision storm (effectively impossible with a 24-bit random
	// suffix). Surface it as 500, not a 4xx the caller could "fix".
	if errors.Is(err, store.ErrVMNicMACConflict) {
		h.log.ErrorContext(r.Context(), "vms.create exhausted nic mac retries", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "enqueue vm task", nil)
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
	if network, ok := scheduler.ExtractNetworkUnreadyDetail(err); ok && network != nil && len(network.Nodes) > 0 {
		filtered := make([]map[string]any, 0, len(network.Nodes))
		for _, n := range network.Nodes {
			networks := make([]map[string]any, 0, len(n.Networks))
			for _, net := range n.Networks {
				networks = append(networks, map[string]any{
					"network_id": net.NetworkID.String(),
					"status":     net.Status,
				})
			}
			filtered = append(filtered, map[string]any{
				"node":     n.Node,
				"networks": networks,
			})
		}
		return map[string]any{
			"reason":                          "network_not_ready",
			"filtered_due_to_network_unready": filtered,
		}
	}

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

// generateLocalMAC mints a locally-administered unicast MAC in QEMU's
// 52:54:00 OUI with three random low bytes. The 52:54:00 prefix is the
// conventional QEMU/KVM range; the random suffix gives ~16M values, so a
// collision within a cluster is astronomically unlikely and no retry loop is
// warranted.
func generateLocalMAC() (net.HardwareAddr, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return net.HardwareAddr{0x52, 0x54, 0x00, b[0], b[1], b[2]}, nil
}
