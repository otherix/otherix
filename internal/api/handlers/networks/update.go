// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Update implements PATCH /v1/networks/{id}. Required permission:
// network:manage (admin only). The handler runs three passes: it first
// rejects the API-immutable `type` and `managed` keys before loading
// the row (pre-load), then loads the row and - for overlay networks -
// rejects every key other than `name` (post-load, because the row's
// type is needed to apply this rule), and finally merges the typed
// updateRequest into the existing row.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "could not read body", nil)
		return
	}

	if forbidden := rejectImmutableKeys(body); forbidden != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "field is immutable",
			map[string]any{"forbidden_fields": forbidden})
		return
	}

	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	row, err := h.store.NetworkByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "network not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load network", nil)
		return
	}

	if row.Type == store.NetworkTypeOverlay {
		if forbidden := rejectNonNameKeys(body); forbidden != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "field is not patchable on an overlay network",
				map[string]any{"forbidden_fields": forbidden})
			return
		}
	}

	if !applyUpdate(w, r, &row, &req) {
		return
	}

	updated, err := h.store.UpdateNetwork(r.Context(), store.UpdateNetworkParams{
		ID:         row.ID,
		Name:       row.Name,
		BridgeName: row.BridgeName,
		Managed:    row.Managed,
		Egress:     row.Egress,
		VlanTag:    row.VlanTag,
		Mtu:        row.Mtu,
		Subnet:     row.Subnet,
		Gateway:    row.Gateway,
		Config:     row.Config,
	})
	if err != nil {
		if errors.Is(err, store.ErrNetworkNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "network name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update network", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// rejectImmutableKeys scans the top-level JSON object for keys the API
// does not allow on PATCH and returns the offending field names (nil
// when none are present). `type` is API-immutable; `managed` is fixed at
// creation time (egress=nat requires managed=true, and flipping managed
// would silently invalidate that invariant), so both are rejected here
// before the typed decode.
func rejectImmutableKeys(body []byte) []string {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		// Defer the malformed-body 400 to the strict decode path so
		// the client sees a single, clearer error.
		return nil
	}
	var forbidden []string
	for _, k := range []string{"type", "managed"} {
		if _, ok := keys[k]; ok {
			forbidden = append(forbidden, k)
		}
	}
	return forbidden
}

// rejectNonNameKeys returns every top-level JSON key other than "name". On
// overlay networks VNI, subnet, bridge name, and MTU are server-owned and
// immutable; only the human-readable name may be patched.
func rejectNonNameKeys(body []byte) []string {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil // malformed body -> let the strict decode issue the 400
	}
	var forbidden []string
	for k := range keys {
		if k != "name" {
			forbidden = append(forbidden, k)
		}
	}
	sort.Strings(forbidden)
	return forbidden
}

// applyUpdate merges req into row in place. Returns false (after
// writing the validation-failed response) when a field is invalid.
func applyUpdate(w http.ResponseWriter, r *http.Request, row *store.Network, req *updateRequest) bool {
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		switch {
		case name == "":
			writeValidation(w, r, "name must not be empty")
			return false
		case utf8.RuneCountInString(name) > validation.NetworkNameMaxLength:
			writeValidation(w, r, "name is too long")
			return false
		}
		row.Name = name
	}
	if req.BridgeName != nil {
		if err := validation.ValidateBridgeName(*req.BridgeName); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.BridgeName = *req.BridgeName
	}
	if len(req.VlanTag) > 0 {
		v, err := decodeNullableInt32(req.VlanTag)
		if err != nil {
			writeValidation(w, r, "vlan_tag must be null or an integer")
			return false
		}
		if v != nil {
			if err := validation.ValidateVLANTag(int(*v)); err != nil {
				writeValidation(w, r, err.Error())
				return false
			}
		}
		row.VlanTag = v
	}
	if req.MTU != nil {
		if err := validation.ValidateMTU(int(*req.MTU)); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Mtu = *req.MTU
	}
	if len(req.Config) > 0 {
		if err := validateConfigShape(req.Config); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Config = normaliseConfig(req.Config)
	}
	if !applyEgressUpdate(w, r, row, req) {
		return false
	}
	return true
}

// egressIntent captures whether the request set or cleared the subnet
// and gateway, alongside the parsed gateway when one was supplied. The
// new subnet is merged onto the row directly; the gateway is held here
// until the resulting egress is known.
type egressIntent struct {
	subnetSet     bool
	subnetCleared bool
	gateway       *netip.Addr
	gatewaySet    bool
}

// applyEgressUpdate merges the egress / subnet / gateway fields into row
// and enforces the resulting-row invariants. The order matters: egress
// and the tri-state subnet/gateway are applied first, then the whole
// (type, managed, egress) triple is re-validated, then the gateway is
// derived or checked against the resulting subnet. egress=none clears
// any subnet/gateway and forbids setting them.
func applyEgressUpdate(w http.ResponseWriter, r *http.Request, row *store.Network, req *updateRequest) bool {
	if req.Egress != nil {
		if err := validation.ValidateNetworkEgress(*req.Egress); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		// ValidateNetworkEgress admits "" as a synonym for none; normalise it
		// here so an empty egress never persists as an out-of-enum value
		// (mirrors CreateNetwork's defaulting in etcdstore).
		egress := store.NetworkEgress(*req.Egress)
		if egress == "" {
			egress = store.NetworkEgressNone
		}
		row.Egress = egress
	}

	intent, ok := applySubnetGateway(w, r, row, req)
	if !ok {
		return false
	}

	if err := validation.ValidateNetworkInvariants(row.Type, row.Managed, row.Egress); err != nil {
		writeValidation(w, r, err.Error())
		return false
	}

	if row.Egress == store.NetworkEgressNAT {
		return finalizeNATEgress(w, r, row, intent)
	}
	return finalizeNoneEgress(w, r, row, intent)
}

// applySubnetGateway decodes the tri-state subnet/gateway fields onto
// row, returning the merge intent. A new subnet is written to row.Subnet
// immediately; the gateway is parsed but held in the intent so the
// resulting egress can decide whether to keep, derive, or reject it.
func applySubnetGateway(w http.ResponseWriter, r *http.Request, row *store.Network, req *updateRequest) (egressIntent, bool) {
	var intent egressIntent
	if len(req.Subnet) > 0 {
		s, cleared, err := decodeNullableString(req.Subnet)
		if err != nil {
			writeValidation(w, r, "subnet must be null or a string")
			return intent, false
		}
		if cleared {
			intent.subnetCleared = true
			row.Subnet = nil
		} else {
			subnet, err := validation.ParseSubnet(s)
			if err != nil {
				writeValidation(w, r, err.Error())
				return intent, false
			}
			row.Subnet = &subnet
			intent.subnetSet = true
		}
	}
	if len(req.Gateway) > 0 {
		s, cleared, err := decodeNullableString(req.Gateway)
		if err != nil {
			writeValidation(w, r, "gateway must be null or a string")
			return intent, false
		}
		if cleared {
			row.Gateway = nil
		} else {
			gw, err := netip.ParseAddr(s)
			if err != nil {
				writeValidation(w, r, "gateway must be a valid ip address")
				return intent, false
			}
			intent.gateway = &gw
			intent.gatewaySet = true
		}
	}
	return intent, true
}

// finalizeNATEgress resolves the gateway for an egress=nat row: an
// explicit gateway is validated against the subnet, a changed/missing
// gateway re-derives the default, and an untouched gateway is re-checked.
func finalizeNATEgress(w http.ResponseWriter, r *http.Request, row *store.Network, intent egressIntent) bool {
	if row.Subnet == nil {
		writeValidation(w, r, "subnet is required when egress=nat")
		return false
	}
	switch {
	case intent.gatewaySet:
		if err := validation.ValidateGatewayInSubnet(*intent.gateway, *row.Subnet); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Gateway = intent.gateway
	case intent.subnetSet || row.Gateway == nil:
		// A new subnet (or a missing gateway) re-derives the default
		// gateway so it always sits inside the active subnet.
		gw := validation.GatewayDefault(*row.Subnet)
		row.Gateway = &gw
	default:
		// Gateway untouched and still valid against an unchanged subnet;
		// re-check defensively.
		if err := validation.ValidateGatewayInSubnet(*row.Gateway, *row.Subnet); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
	}
	return true
}

// finalizeNoneEgress enforces the egress=none invariant: the row carries
// no subnet/gateway. Setting either (without also clearing it) is
// rejected; otherwise both are cleared.
func finalizeNoneEgress(w http.ResponseWriter, r *http.Request, row *store.Network, intent egressIntent) bool {
	if (intent.subnetSet && !intent.subnetCleared) || intent.gatewaySet {
		writeValidation(w, r, "subnet and gateway are only allowed when egress=nat")
		return false
	}
	row.Subnet = nil
	row.Gateway = nil
	return true
}

// decodeNullableString unwraps a tri-state RawMessage into a string. The
// literal `null` returns ("", true, nil); a JSON string returns
// (value, false, nil); anything else surfaces an error.
func decodeNullableString(raw json.RawMessage) (value string, cleared bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return "", true, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false, err
	}
	return s, false, nil
}

// decodeNullableInt32 unwraps a tri-state RawMessage into an
// optional int32. The literal `null` returns (nil, nil); a JSON
// number returns (&n, nil); anything else surfaces an error so the
// handler can issue a 400.
func decodeNullableInt32(raw json.RawMessage) (*int32, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var n int32
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// writeValidation is a thin shorthand around the validation_failed
// envelope used by applyUpdate.
func writeValidation(w http.ResponseWriter, r *http.Request, msg string) {
	response.WriteError(w, r, http.StatusBadRequest,
		response.CodeValidationFailed, msg, nil)
}
