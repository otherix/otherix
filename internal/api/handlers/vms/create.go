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

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// vmCreateRequest is the body of POST /v1/vms. `vcpus` / `memory_mb`
// translate to `cpu_cores` / `memory_mib` at the handler boundary;
// the requested pool / network names are stashed in the VM's
// SchedulingSpec and resolved later at bind.
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
//   - `node` is an optional placement hint stashed on the SchedulingSpec;
//     the vms.schedule loop restricts the candidate list to that node.
type vmCreateRequest struct {
	Name        string `json:"name"`
	ImageURL    string `json:"image_url"`
	ImageSHA256 string `json:"image_sha256,omitempty"`
	// SourceSnapshotID recreates the VM from a snapshot instead of an image
	// (`vm create --from-snapshot`). Exactly one of ImageURL / SourceSnapshotID
	// must be set. When set, architecture / firmware come from the snapshot
	// manifest and must not be supplied on the request.
	SourceSnapshotID *string `json:"source_snapshot_id,omitempty"`
	Architecture     string  `json:"arch"`
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
	// NetworkConfig is an optional VM-level cloud-init network-config
	// (NoCloud /network-config). Stored verbatim in vms.network_config
	// and passed through to the agent's cidata seed at create time.
	// Mutually exclusive with cloud_init_disabled; allowed alongside
	// user_data (different channels).
	NetworkConfig *string `json:"network_config,omitempty"`
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
// flows as a typed error through the bind transaction (the vms.schedule
// loop) and surfaces as a pending scheduling reason. The admission edge
// uses checkPoolWritableBestEffort for a best-effort fail-fast on an
// already-existing non-writable pool.
type poolNotWritableError struct {
	poolType string
}

func (e *poolNotWritableError) Error() string {
	return fmt.Sprintf("vms: pool type %q not writable", e.poolType)
}

// Create implements POST /v1/vms — the operator-callable admission entry
// point for a VM.
//
// Permission gate: vm:create. RequirePermission middleware admits
// admin / operator / developer (matrix has each at scope=any). The image
// source is supplied directly on the request, so there is no
// template-usability composite check.
//
// Admission-only: Create validates the request, captures the deferred
// placement inputs (pool name, disk size, network name, node hint) in the
// VM's SchedulingSpec, and persists the VM in the "unscheduled" state via
// CreateUnscheduledVM. No placement, no disk / NIC / task rows, and no
// agent job at admission — the vms.schedule loop binds the VM to a
// (node, pool) once its dependencies are ready.
//
// Returns 201 Created + the VM view (status.phase=pending).
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

	src, ok := h.resolveImageSource(w, r, caller, req)
	if !ok {
		return
	}

	poolName, ok := h.resolvePoolName(w, r, req.Pool)
	if !ok {
		return
	}
	if !h.checkPoolWritableBestEffort(w, r, poolName) {
		return
	}

	networkName, ok := h.resolveNetworkName(w, r, req.Network)
	if !ok {
		return
	}

	spec := store.SchedulingSpec{
		PoolName: poolName,
		DiskGiB:  int32(req.DiskGiB), //nolint:gosec // bounded 0..65536 by validateCreateRequest
		NodeHint: req.Node,
	}
	if networkName != "" {
		spec.NetworkName = &networkName
	}
	specJSON, err := store.MarshalSchedulingSpec(spec)
	if err != nil {
		h.log.ErrorContext(r.Context(), "marshal scheduling spec", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "create vm", nil)
		return
	}

	vmID, err := h.store.CreateUnscheduledVM(r.Context(), store.CreateVMParams{
		ID:                uuid.New(),
		OwnerID:           caller.ID,
		Name:              req.Name,
		Architecture:      src.arch,
		ImageURL:          src.imageURL,
		ImageSHA256:       src.imageSHA,
		ImageFormat:       src.format,
		CpuCores:          int32(req.VCPUs),    //nolint:gosec // bounded 1..128 by validateCreateRequest
		MemoryMib:         int32(req.MemoryMB), //nolint:gosec // bounded 128..524288 by validateCreateRequest
		CPUModel:          "host",
		MachineType:       machineTypeFor(src.arch),
		FirmwareID:        src.firmwareID,
		SourceSnapshotID:  src.sourceSnapshotID,
		UserData:          req.UserData,
		CloudInitDisabled: req.CloudInitDisabled,
		NetworkConfig:     req.NetworkConfig,
		Labels:            []byte(`{}`),
		SchedulingSpec:    specJSON,
	})
	if err != nil {
		if errors.Is(err, store.ErrVMNameInUse) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeVMNameInUse, "a vm with this name already exists", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "create unscheduled vm", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "create vm", nil)
		return
	}

	h.renderVM(w, r, vmID, http.StatusCreated)
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

// maxDiskGiB caps the requested root-disk size; it matches the vm_disks
// size_gib ceiling in the OpenAPI contract and keeps the int32 disk column and
// the bytes multiplication (disk_gib * 1 GiB) free of overflow.
const maxDiskGiB = 65536

// validateCreateImageFields enforces the image-source field invariants:
// image_url present and an absolute https URL (SSRF guard, audit M1), arch in
// {amd64, arm64}, image_sha256 (when set) a 64-char lowercase hex digest,
// format (when set) in {qcow2, raw}, and disk_gib in [0, maxDiskGiB]. Returns
// false (after writing the 400 envelope) on the first violation.
func validateCreateImageFields(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	if req.ImageURL == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "image_url is required", nil)
		return false
	}
	if err := validation.ValidateImageURL(req.ImageURL); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
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
	// 0 means "default to the image virtual size"; the upper bound matches the
	// vm_disks size_gib ceiling and keeps the int32 column / bytes math safe.
	if req.DiskGiB < 0 || req.DiskGiB > maxDiskGiB {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "disk_gib must be between 0 and 65536", nil)
		return false
	}
	return true
}

// validateCreateRequest enforces the field-level invariants. Identifier
// well-formedness for `pool` / `network` / `firmware_id` is deferred to the
// resolver / firmware lookup layers.
func validateCreateRequest(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	// The name feeds the cidata local-hostname / instance-id and the
	// console-stream URL path, so it is constrained to a lowercase
	// RFC 1123 DNS label (audit LOW).
	if err := validation.ValidateVMName(req.Name); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return false
	}
	// Exactly one of image_url / source_snapshot_id selects the disk source.
	// The snapshot path takes architecture / firmware from the manifest, so it
	// skips the image-field validation; the image path validates them.
	if !validateImageSourceExclusivity(w, r, req) {
		return false
	}
	if !snapshotSourced(req) {
		if !validateCreateImageFields(w, r, req) {
			return false
		}
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
	// A node hint is a node name, never a uuid. Rejecting a uuid literal here
	// keeps the old admission-time 400 (the placement layer also guards via
	// scheduler.ErrNodeHintIsUUID) so it never silently becomes a pending VM.
	if req.Node != nil {
		if _, err := uuid.Parse(*req.Node); err == nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "node must be a name, not a uuid", nil)
			return false
		}
	}
	if !validateCloudInitExclusivity(w, r, req) {
		return false
	}
	return true
}

// validateCloudInitExclusivity enforces the cloud-init three-state contract:
// cloud_init_disabled may not be combined with either cloud-init channel.
// user_data and network_config are independent channels and may be set
// together; each is individually mutually exclusive with the disable flag.
// It writes a 400 and returns false on conflict.
func validateCloudInitExclusivity(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	if req.CloudInitDisabled && req.UserData != nil && *req.UserData != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"user_data and cloud_init_disabled are mutually exclusive",
			map[string]any{"conflicting_fields": []string{"user_data", "cloud_init_disabled"}})
		return false
	}
	if req.CloudInitDisabled && req.NetworkConfig != nil && *req.NetworkConfig != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"network_config and cloud_init_disabled are mutually exclusive",
			map[string]any{"conflicting_fields": []string{"network_config", "cloud_init_disabled"}})
		return false
	}
	return true
}

// snapshotSourced reports whether the request asks to recreate from a snapshot
// (source_snapshot_id set and non-empty) rather than from an image.
func snapshotSourced(req vmCreateRequest) bool {
	return req.SourceSnapshotID != nil && *req.SourceSnapshotID != ""
}

// validateImageSourceExclusivity enforces the exactly-one-of contract: exactly
// one of image_url / source_snapshot_id must be set. Both-set or neither-set is
// 400 validation_failed with details.reason="image_xor_snapshot".
func validateImageSourceExclusivity(w http.ResponseWriter, r *http.Request, req vmCreateRequest) bool {
	if (req.ImageURL != "") == snapshotSourced(req) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"exactly one of image_url or source_snapshot_id must be set",
			map[string]any{"reason": "image_xor_snapshot"})
		return false
	}
	return true
}

// imageSource is the resolved disk source for a create: the architecture,
// format, firmware, image fields, and recreate provenance the VM row is built
// from. For an image-sourced create it carries the request's image fields; for a
// snapshot-sourced create it carries the snapshot's architecture / firmware,
// empty image fields, and the source snapshot id.
type imageSource struct {
	arch             store.CPUArch
	format           store.ImageFormat
	firmwareID       *uuid.UUID
	imageURL         string
	imageSHA         []byte
	sourceSnapshotID *uuid.UUID
}

// resolveImageSource resolves the disk source for the create. For a
// snapshot-sourced create it loads the snapshot (404 on missing / cross-owner -
// existence is not leaked), requires status=ready (409 snapshot_not_ready),
// rejects a caller-supplied architecture / firmware (400 field_from_snapshot -
// these come from the manifest), and takes architecture / firmware from the
// manifest with empty image fields. For an image-sourced create it resolves
// firmware from the request and carries the request's image fields. It writes the
// wire envelope and returns ok=false on any failure.
func (h *Handler) resolveImageSource(w http.ResponseWriter, r *http.Request, caller *auth.User, req vmCreateRequest) (imageSource, bool) {
	if snapshotSourced(req) {
		return h.resolveSnapshotSource(w, r, caller, req)
	}
	return h.resolveImageSourceFromRequest(w, r, req)
}

// resolveImageSourceFromRequest builds the image-sourced disk source from the
// request fields (the legacy image path). Firmware is resolved from
// firmware/firmware_id; image fields come straight off the validated request.
func (h *Handler) resolveImageSourceFromRequest(w http.ResponseWriter, r *http.Request, req vmCreateRequest) (imageSource, bool) {
	firmwareID, ok := h.resolveFirmwareForRequest(w, r, req)
	if !ok {
		return imageSource{}, false
	}
	var imageSHA []byte
	if req.ImageSHA256 != "" {
		// Validated 64-char lowercase hex at the API edge, so DecodeString
		// cannot fail; the error is dropped (defense-in-depth: an empty digest
		// degrades to name-keyed cache reuse).
		imageSHA, _ = hex.DecodeString(req.ImageSHA256)
	}
	format := store.ImageFormatQcow2
	if req.Format != "" {
		format = store.ImageFormat(req.Format)
	}
	return imageSource{
		arch:       store.CPUArch(req.Architecture),
		format:     format,
		firmwareID: firmwareID,
		imageURL:   req.ImageURL,
		imageSHA:   imageSHA,
	}, true
}

// resolveSnapshotSource builds the recreate disk source from a snapshot manifest.
// It enforces the cross-owner 404, the ready-status 409, and the
// must-not-supply-architecture/firmware 400 before taking the architecture /
// firmware from the manifest.
func (h *Handler) resolveSnapshotSource(w http.ResponseWriter, r *http.Request, caller *auth.User, req vmCreateRequest) (imageSource, bool) {
	snapID, err := uuid.Parse(*req.SourceSnapshotID)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "source_snapshot_id is not a valid uuid", nil)
		return imageSource{}, false
	}
	// Architecture / firmware are taken from the snapshot manifest; a
	// conflicting request value is rejected rather than silently ignored.
	if req.Architecture != "" || req.FirmwareID != "" || req.Firmware != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"architecture and firmware come from the snapshot and must not be supplied",
			map[string]any{"reason": "field_from_snapshot"})
		return imageSource{}, false
	}

	snap, err := h.store.SnapshotByID(r.Context(), snapID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "snapshot not found", nil)
			return imageSource{}, false
		}
		h.log.ErrorContext(r.Context(), "load source snapshot", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "create vm", nil)
		return imageSource{}, false
	}
	// Cross-owner visibility is a 404 (never 403) - leaking existence leaks data.
	if err := auth.CheckOwnership(caller, &snap.OwnerID, auth.PermSnapshotRead); err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "snapshot not found", nil)
		return imageSource{}, false
	}
	if snap.Status != store.SnapshotStatusReady {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "snapshot is not ready",
			map[string]any{"reason": "snapshot_not_ready"})
		return imageSource{}, false
	}

	return imageSource{
		arch:             snap.SourceArchitecture,
		format:           store.ImageFormatQcow2,
		firmwareID:       snap.SourceFirmwareID,
		sourceSnapshotID: &snapID,
	}, true
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

// resolvePoolName returns the effective pool name for the create
// request. It accepts:
//
//   - empty `pool`: fall back to cluster_settings.default_pool_name;
//     missing default → 400 default_pool_not_set.
//   - UUID literal: look up the storage_pools row by id and return
//     that instance's name (callers retain UUID-based addressing as
//     a backward-compatible escape hatch into the scheduler's
//     name-driven placement); a missing id → 404 pool_not_found.
//   - bare string: passed through verbatim as a pool name without an
//     existence check. The name is stashed on the SchedulingSpec and
//     resolved to instances at bind; an unknown name becomes a pending
//     scheduling reason, never an admission error.
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

// checkPoolWritableBestEffort fails the create fast with 409 pool_not_writable
// when the named pool already exists and every instance has a storage type
// vm.create cannot drive (only local_dir today). When the pool does not exist
// yet the check is deferred (it becomes a pending reason at bind), consistent
// with deferred name resolution. The boolean return mirrors the resolve*
// helpers' short-circuit signal.
func (h *Handler) checkPoolWritableBestEffort(w http.ResponseWriter, r *http.Request, poolName string) bool {
	pools, err := h.store.StoragePoolsByName(r.Context(), poolName)
	if err != nil || len(pools) == 0 {
		// Unknown / not-yet-created (or a transient read error) -> defer to bind.
		return true
	}
	for _, p := range pools {
		if p.Type == "local_dir" {
			return true
		}
	}
	response.WriteError(w, r, http.StatusConflict,
		response.CodePoolNotWritable,
		"storage pool type does not support vm.create",
		map[string]any{"pool": poolName})
	return false
}

// resolveNetworkName resolves the optional `network` request field to a
// network name stashed on the SchedulingSpec; the NIC row is created later at
// bind. It accepts:
//
//   - empty: no NIC is attached (returns "", true) — legacy SLIRP fallback.
//   - uuid literal: looked up by id (an explicit reference); a missing id →
//     404 not_found. Returns the instance's canonical name.
//   - bare string: returned verbatim without an existence check. An unknown
//     name becomes a pending scheduling reason at bind, never an admission
//     error.
//
// The boolean second return mirrors the other resolve* helpers'
// short-circuit signal.
func (h *Handler) resolveNetworkName(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	if requested == "" {
		return "", true
	}
	id, perr := uuid.Parse(requested)
	if perr != nil {
		// A bare name is deferred (no existence check at admission).
		return requested, true
	}
	net, err := h.store.NetworkByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "network not found", nil)
			return "", false
		}
		h.log.ErrorContext(r.Context(), "load network", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load network", nil)
		return "", false
	}
	return net.Name, true
}

// generateLocalMAC mints a locally-administered unicast MAC in QEMU's
// 52:54:00 OUI with three random low bytes. The 52:54:00 prefix is the
// conventional QEMU/KVM range; the random suffix gives ~16M values, so a
// collision within a cluster is astronomically unlikely and no retry loop is
// warranted. Consumed by the vms.schedule loop when it mints the NIC at bind.
func generateLocalMAC() (net.HardwareAddr, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return net.HardwareAddr{0x52, 0x54, 0x00, b[0], b[1], b[2]}, nil
}
