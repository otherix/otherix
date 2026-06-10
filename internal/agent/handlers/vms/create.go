// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// nicReq is one CP-declared network interface in a create request. An
// empty or absent nics list falls back to legacy SLIRP networking.
type nicReq struct {
	ID          string `json:"id"`
	Bridge      string `json:"bridge"`
	MAC         string `json:"mac"`
	Model       string `json:"model"`
	MTU         int    `json:"mtu"`
	DeviceOrder int    `json:"device_order"`
}

type createRequest struct {
	// UUID is optional. When supplied it is used as the agent-side VM
	// id (unified UUID model: CP mints, agent uses). When absent the
	// agent generates a fresh uuid - backward compatible with pre-Phase A
	// callers.
	//
	// `pool` is the pool **name**. The agent's local registry is
	// name-keyed; the cluster-wide UUID polymorphism stays an
	// operator-edge concern.
	UUID     string `json:"uuid,omitempty"`
	Name     string `json:"name"`
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	Pool     string `json:"pool"`
	// ImageURL is the HTTPS source the agent ensures locally (basename-keyed
	// IfNotPresent cache). ExpectedSHA256, when non-empty, pins the content
	// digest (verify mode). Format defaults to "qcow2" when empty. DiskGiB is
	// the requested root-disk size in GiB (0 = default to the image's virtual
	// size).
	ImageURL       string `json:"image_url"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	Format         string `json:"format,omitempty"`
	DiskGiB        int    `json:"disk_gib,omitempty"`
	// UserData carries CP-resolved raw `#cloud-config` YAML (L3 Area
	// 3 lock). Optional — empty value skips cidata generation.
	// CP-side resolver passes through vm.user_data and injects a
	// top-level `hostname:` (no template fallback)
	// matching the VM name when missing.
	UserData string `json:"user_data,omitempty"`
	// NetworkConfig carries the CP-supplied cloud-init network-config
	// blob (NoCloud /network-config). Optional; passed through verbatim.
	NetworkConfig string `json:"network_config,omitempty"`
	// Nics are the CP-declared network interfaces to attach. Absent or
	// empty means legacy SLIRP user-mode networking.
	Nics []nicReq `json:"nics,omitempty"`
}

type asyncAccepted struct {
	TaskID string         `json:"task_id"`
	Status string         `json:"status"`
	Links  map[string]any `json:"links"`
}

// Create handles POST /v1/vms.
//
//   - Validates the request body shape.
//   - Asks the manager to begin async creation; receives an agent task.
//   - Returns 202 with task_id immediately; clients poll /v1/tasks/{id}.
//
// The response envelope mirrors AsyncTaskAccepted from the CP-side
// spec: { task_id, status, links: { self } }. The agent has no SQL
// "tasks" table — task IDs are agent-local for the scope of one
// process run.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Pool == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool is required", nil)
		return
	}

	var (
		vmID uuid.UUID
		err  error
	)
	if req.UUID != "" {
		vmID, err = uuid.Parse(req.UUID)
		if err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "uuid is not a valid UUID", nil)
			return
		}
	}

	if msg, details := validateNICs(req.Nics); msg != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, msg, details)
		return
	}

	nics := make([]netfabric.NIC, 0, len(req.Nics))
	for _, n := range req.Nics {
		nicID, perr := uuid.Parse(n.ID)
		if perr != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "nic id is not a valid UUID", nil)
			return
		}
		nics = append(nics, netfabric.NIC{
			ID:          nicID,
			Bridge:      n.Bridge,
			MAC:         n.MAC,
			Model:       n.Model,
			MTU:         n.MTU,
			DeviceOrder: n.DeviceOrder,
		})
	}

	format := req.Format
	if format == "" {
		format = "qcow2"
	}

	task, err := h.manager.Create(r.Context(), vm.CreateSpec{
		UUID:           vmID,
		Name:           req.Name,
		VCPUs:          req.VCPUs,
		MemoryMB:       req.MemoryMB,
		PoolName:       req.Pool,
		ImageURL:       req.ImageURL,
		ExpectedSHA256: req.ExpectedSHA256,
		Format:         format,
		DiskGiB:        req.DiskGiB,
		UserData:       []byte(req.UserData),
		NetworkData:    []byte(req.NetworkConfig),
		NICs:           nics,
	})
	if err != nil {
		mapCreateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

// maxDeviceOrder bounds the per-VM NIC slot index. The guest exposes a
// small, bounded set of PCI slots; the wire device_order is the slot
// the NIC occupies. 0..15 is generous for the supported guest shapes.
const maxDeviceOrder = 15

// validateNICs runs the agent VM-create API-edge checks over the NIC
// list, before anything is materialised. Today a bad MAC or model only
// surfaces deep in qemu.BuildArgs (after taps are created) and a bad
// bridge only at AttachTap; moving the checks here means no host
// primitive is created on bad input. It returns ("", nil) when every
// NIC is valid, or a human-readable message plus optional details for
// the 400 envelope on the first violation.
//
// Checks per NIC: MAC present and a valid 6-octet EUI-48; model empty
// or one of the supported QEMU models; bridge non-empty (the tap needs
// a bridge to attach to); device_order in [0, maxDeviceOrder]. Across
// the list: device_order values are unique and MACs are unique.
func validateNICs(nics []nicReq) (string, map[string]any) {
	seenOrder := make(map[int]struct{}, len(nics))
	seenMAC := make(map[string]struct{}, len(nics))
	for i, n := range nics {
		if err := netfabric.ValidateMAC(n.MAC); err != nil {
			return "nic mac is not a valid 6-octet MAC address",
				map[string]any{"index": i, "mac": n.MAC}
		}
		if err := netfabric.ValidateModel(n.Model); err != nil {
			return "nic model is not a supported QEMU NIC model",
				map[string]any{"index": i, "model": n.Model}
		}
		if n.Bridge == "" {
			return "nic bridge is required", map[string]any{"index": i}
		}
		if n.DeviceOrder < 0 || n.DeviceOrder > maxDeviceOrder {
			return "nic device_order is out of range",
				map[string]any{"index": i, "device_order": n.DeviceOrder, "max": maxDeviceOrder}
		}
		if _, dup := seenOrder[n.DeviceOrder]; dup {
			return "nic device_order is duplicated",
				map[string]any{"index": i, "device_order": n.DeviceOrder}
		}
		seenOrder[n.DeviceOrder] = struct{}{}
		if _, dup := seenMAC[n.MAC]; dup {
			return "nic mac is duplicated", map[string]any{"index": i, "mac": n.MAC}
		}
		seenMAC[n.MAC] = struct{}{}
	}
	return "", nil
}

func mapCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vm.ErrInvalidSpec):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
	case errors.Is(err, vm.ErrPoolUnknown):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool does not match a configured pool", nil)
	case errors.Is(err, vm.ErrCreateInFlight):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "vm create already in flight for this id", nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
	}
}
