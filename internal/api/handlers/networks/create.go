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

	cfg := normaliseConfig(req.Config)

	row, err := h.store.CreateNetwork(r.Context(), store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       req.Name,
		Type:       store.NetworkType(req.Type),
		BridgeName: req.BridgeName,
		Managed:    managed,
		Egress:     egress,
		VlanTag:    req.VlanTag,
		Mtu:        mtu,
		Subnet:     subnet,
		Gateway:    gateway,
		Config:     cfg,
	})
	if err != nil {
		if errors.Is(err, store.ErrNetworkNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "network name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist network", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// validateCreate enforces the API-edge invariants on the create
// payload. Order is biased toward the most specific error first.
func validateCreate(req *createRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		return errors.New("name is required")
	case utf8.RuneCountInString(req.Name) > validation.NetworkNameMaxLength:
		return errors.New("name is too long")
	}

	if err := validation.ValidateNetworkType(req.Type); err != nil {
		return err
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
