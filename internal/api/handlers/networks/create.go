// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Create implements POST /v1/networks. Required permission:
// network:manage (admin only). Returns 201 with the projected
// Network on success.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	if err := validateCreate(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	if store.NetworkType(req.Type) == store.NetworkTypeOverlay {
		h.createOverlay(w, r, &req)
		return
	}
	h.createBridge(w, r, &req)
}

// createOverlay handles the type=overlay create branch: it parses the subnet,
// resolves egress, enforces the DHCP cross-field rules, persists the row (the
// store stamps VNI / bridge name / mtu / managed), and warns on VNI exhaustion.
func (h *Handler) createOverlay(w http.ResponseWriter, r *http.Request, req *createRequest) {
	subnet, err := validation.ParseSubnet(*req.Subnet)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	egress := store.NetworkEgressNone
	if req.Egress != nil {
		egress = store.NetworkEgress(*req.Egress)
	}
	dhcp := req.Dhcp != nil && *req.Dhcp
	dns := true
	if req.DNS != nil {
		dns = *req.DNS
	}
	if err := validation.ValidateDhcp(dhcp, true, true, store.NetworkTypeOverlay); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	if err := validation.ValidateDNS(dns, true, store.NetworkTypeOverlay); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	row, err := h.store.CreateNetwork(r.Context(), store.CreateNetworkParams{
		ID:          uuid.New(),
		Name:        req.Name,
		Type:        store.NetworkTypeOverlay,
		Egress:      egress,
		Subnet:      &subnet,
		DhcpEnabled: dhcp,
		DNSEnabled:  dns,
		Config:      normaliseConfig(req.Config),
	})
	if err != nil {
		h.writeCreateNetworkError(w, r, err)
		return
	}
	if row.VNI != nil {
		if _, max, rangeErr := h.store.VNIRange(r.Context()); rangeErr == nil {
			const vniHighWaterRemaining = 256
			if remaining := max - *row.VNI; remaining < vniHighWaterRemaining {
				h.log.Warn("overlay VNI range nearing exhaustion",
					"allocated_vni", *row.VNI, "max_vni", max, "remaining", remaining)
			}
		}
	}
	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// createBridge handles the type=bridge create branch: it resolves the
// egress-driven subnet/gateway, materialises the mtu default, enforces the DHCP
// cross-field rules (which reject dhcp=true on a bridge), and persists the row.
func (h *Handler) createBridge(w http.ResponseWriter, r *http.Request, req *createRequest) {
	managed := req.Managed != nil && *req.Managed
	egress := store.NetworkEgressNone
	if req.Egress != nil {
		egress = store.NetworkEgress(*req.Egress)
	}

	subnet, gateway, err := resolveCreateEgress(egress, req.Subnet, req.Gateway)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	mtu := int32(validation.DefaultMTU)
	if req.MTU != nil {
		mtu = *req.MTU
	}

	dhcp := req.Dhcp != nil && *req.Dhcp
	dns := req.DNS != nil && *req.DNS
	if err := validation.ValidateDhcp(dhcp, req.Subnet != nil, managed, store.NetworkType(req.Type)); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	if err := validation.ValidateDNS(dns, managed, store.NetworkType(req.Type)); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	row, err := h.store.CreateNetwork(r.Context(), store.CreateNetworkParams{
		ID:          uuid.New(),
		Name:        req.Name,
		Type:        store.NetworkType(req.Type),
		BridgeName:  req.BridgeName,
		Managed:     managed,
		Egress:      egress,
		VlanTag:     req.VlanTag,
		Mtu:         mtu,
		Subnet:      subnet,
		Gateway:     gateway,
		DhcpEnabled: dhcp,
		DNSEnabled:  dns,
		Config:      normaliseConfig(req.Config),
	})
	if err != nil {
		h.writeCreateNetworkError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// writeCreateNetworkError maps a CreateNetwork failure to its HTTP response:
// a name-uniqueness, VNI-exhaustion, or sub-floor underlay MTU conflict to 409,
// anything else to 500.
func (h *Handler) writeCreateNetworkError(w http.ResponseWriter, r *http.Request, err error) {
	var floorErr *store.UnderlayBelowFloorError
	if errors.As(err, &floorErr) {
		response.WriteError(w, r, http.StatusConflict, response.CodeConflict,
			"overlay networks require an underlay mtu at or above the floor",
			map[string]any{
				"underlay_mtu":        floorErr.UnderlayMTU,
				"min_underlay_mtu":    floorErr.MinUnderlayMTU,
				"derived_overlay_mtu": floorErr.DerivedOverlayMTU,
			})
		return
	}
	switch {
	case errors.Is(err, store.ErrNetworkNameExists):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "network name already in use", nil)
	case errors.Is(err, store.ErrVNIExhausted):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "overlay VNI range exhausted", nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist network", nil)
	}
}

// validateNetworkName checks the create-side network name (trimmed by the
// caller): non-empty, within the length bound, and free of '/' (which would
// poison the etcd uniqueness-guard key path).
func validateNetworkName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required")
	case utf8.RuneCountInString(name) > validation.NetworkNameMaxLength:
		return errors.New("name is too long")
	case strings.ContainsRune(name, '/'):
		return errors.New("name must not contain '/'")
	}
	return nil
}

// validateCreate enforces the API-edge invariants on the create
// payload. Order is biased toward the most specific error first.
func validateCreate(req *createRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	if err := validateNetworkName(req.Name); err != nil {
		return err
	}

	if err := validation.ValidateNetworkType(req.Type); err != nil {
		return err
	}

	if store.NetworkType(req.Type) == store.NetworkTypeOverlay {
		return validateOverlayCreate(req)
	}

	if err := validation.ValidateBridgeName(req.BridgeName); err != nil {
		return err
	}

	if req.VlanTag != nil {
		if err := validation.ValidateVLANTag(int(*req.VlanTag)); err != nil {
			return err
		}
	}

	if req.MTU != nil {
		if err := validation.ValidateMTU(int(*req.MTU)); err != nil {
			return err
		}
	}

	egress := store.NetworkEgressNone
	if req.Egress != nil {
		if err := validation.ValidateNetworkEgress(*req.Egress); err != nil {
			return err
		}
		egress = store.NetworkEgress(*req.Egress)
	}
	managed := req.Managed != nil && *req.Managed
	if err := validation.ValidateNetworkInvariants(store.NetworkType(req.Type), managed, egress); err != nil {
		return err
	}

	if err := validateConfigShape(req.Config); err != nil {
		return err
	}

	return nil
}

// validateOverlayCreate enforces the type=overlay create invariants: subnet
// required; bridge_name / mtu / vlan_tag / gateway forbidden (server-derived or
// fixed); egress must be none; managed must be true (default). The VNI, bridge
// name, mtu, and managed flag are stamped by the store, not the client.
func validateOverlayCreate(req *createRequest) error {
	if req.Subnet == nil {
		return errors.New("subnet is required for type=overlay")
	}
	if req.BridgeName != "" {
		return errors.New("bridge_name is forbidden for type=overlay (the server derives otb<vni>)")
	}
	if req.MTU != nil {
		return errors.New("mtu is forbidden for type=overlay (fixed at 1390)")
	}
	if req.VlanTag != nil {
		return errors.New("vlan_tag is forbidden for type=overlay")
	}
	if req.Gateway != nil {
		return errors.New("gateway is forbidden for type=overlay")
	}
	if req.Egress != nil {
		if err := validation.ValidateNetworkEgress(*req.Egress); err != nil {
			return err
		}
	}
	if req.Managed != nil && !*req.Managed {
		return errors.New("managed=false is forbidden for type=overlay (always Otherix-managed)")
	}
	return validateConfigShape(req.Config)
}

// resolveCreateEgress turns the optional subnet/gateway request strings
// into the stored pointers, enforcing the egress-driven invariants:
//
//   - egress=nat: subnet is required and parsed to canonical (masked)
//     form; gateway, if supplied, must be a valid host inside the
//     subnet, otherwise it defaults to the first usable host.
//   - egress=none: neither subnet nor gateway may be present, keeping the
//     stored model free of IP fields the mode does not use.
func resolveCreateEgress(egress store.NetworkEgress, subnetStr, gatewayStr *string) (*netip.Prefix, *netip.Addr, error) {
	if egress != store.NetworkEgressNAT {
		if subnetStr != nil {
			return nil, nil, errors.New("subnet is only allowed when egress=nat")
		}
		if gatewayStr != nil {
			return nil, nil, errors.New("gateway is only allowed when egress=nat")
		}
		return nil, nil, nil
	}

	if subnetStr == nil {
		return nil, nil, errors.New("subnet is required when egress=nat")
	}
	subnet, err := validation.ParseSubnet(*subnetStr)
	if err != nil {
		return nil, nil, err
	}

	var gateway netip.Addr
	if gatewayStr != nil {
		gateway, err = netip.ParseAddr(*gatewayStr)
		if err != nil {
			return nil, nil, errors.New("gateway must be a valid ip address")
		}
		if err := validation.ValidateGatewayInSubnet(gateway, subnet); err != nil {
			return nil, nil, err
		}
	} else {
		gateway = validation.GatewayDefault(subnet)
	}

	return &subnet, &gateway, nil
}

// validateConfigShape returns nil when the supplied bytes are either
// empty (caller omitted the field) or a JSON object literal. The
// schema column is `jsonb NOT NULL DEFAULT '{}'`; arrays, scalars,
// and the `null` literal are rejected so the column never carries a
// shape downstream code does not expect.
func validateConfigShape(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("config must be a JSON object")
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return errors.New("config must be a JSON object")
	}
	if probe == nil {
		return errors.New("config must be a JSON object")
	}
	return nil
}

// normaliseConfig returns the bytes that should land in the
// networks.config column. An absent or empty payload becomes the
// canonical empty-object literal so the column never carries a NULL
// or empty string.
func normaliseConfig(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return []byte(raw)
}
