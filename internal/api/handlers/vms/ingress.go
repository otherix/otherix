// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/gateways"
	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// ingressCredTTL is the validity stamped onto a brokered ingress session
// credential. Deliberately single-digit minutes: the credential is a
// connect-time bearer re-minted per broker call, so a leaked token is
// near-useless while still covering a connect handshake without a clock-skew
// miss.
const ingressCredTTL = 5 * time.Minute

// ingressRequest is the broker request body: the guest TCP port the caller
// wants to reach. Validated to 1..65535.
type ingressRequest struct {
	Port int `json:"port"`
}

// ingressResponse tells the client where and how to connect. Transport is the
// discriminator the CLI switches on:
//
//   - "gateway": an overlay VM. SplicerAddr is the converged gateway's
//     advertised endpoint and SessionCred is a short-lived bearer the client
//     presents to the gateway, which verifies it offline against the session CA
//     public half. The control plane is out of the data path.
//   - "relay": a bridge VM. The client connects back through the control-plane
//     relay (the relay authorizes per request itself), so no gateway address or
//     session credential is minted here.
//
// Gateway-only fields are omitted on the relay path.
type ingressResponse struct {
	Transport   string `json:"transport"`
	VMID        string `json:"vm_id"`
	Port        int    `json:"port"`
	SplicerAddr string `json:"splicer_addr,omitempty"`
	SessionCred string `json:"session_cred,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// Ingress implements POST /v1/vms/{id}/ingress: it brokers an L4 ingress
// connection to a VM. It authorizes the caller (vm:connect at the route plus an
// ownership check), selects how the client should reach the guest, and returns
// the connect coordinates synchronously - the data-plane connect is the
// client's job, so this is a 200, not a 202.
//
// An overlay VM is brokered to a converged gateway with a minted session
// credential (the control plane is out of the data path). A bridge VM is
// brokered over the control-plane relay (no gateway credential). A VM the caller
// cannot see returns 404, never 403, so the endpoint leaks no VM existence. A VM
// with no usable network, or an overlay with no converged gateway, is 409
// ingress_unavailable.
func (h *Handler) Ingress(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vm, err := resolver.VM(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeResolveError(w, r, err)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMConnect); err != nil {
		// Cross-owner visibility goes through 404, never 403, so the broker
		// never confirms a VM the caller does not own exists.
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}

	port, ok := parseIngressPort(w, r)
	if !ok {
		return
	}

	overlayNIC, hasUsable, err := h.resolveIngressNIC(r, vm.ID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.ingress resolve nic",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve vm network", nil)
		return
	}

	if overlayNIC != nil {
		h.brokerOverlay(w, r, vm, *overlayNIC, port)
		return
	}
	if hasUsable {
		// Bridge VM: brokered over the control-plane relay. No gateway
		// credential is minted; the relay authorizes per request itself.
		response.WriteJSON(w, r, http.StatusOK, ingressResponse{
			Transport: "relay",
			VMID:      vm.ID.String(),
			Port:      port,
		})
		return
	}

	// No NIC on any usable network - nothing to broker.
	response.WriteError(w, r, http.StatusConflict,
		response.CodeIngressUnavailable, "no usable network for ingress", nil)
}

// brokerOverlay selects a converged gateway for the overlay VM, mints a
// short-lived session credential bound to the NIC, and returns the gateway
// splicer address. ErrIngressUnavailable from selection (or a NIC without a
// guest IP) is 409 ingress_unavailable.
func (h *Handler) brokerOverlay(w http.ResponseWriter, r *http.Request, vm store.VM, nic store.VMNic, port int) {
	gw, err := gateways.SelectGatewayForVM(r.Context(), h.store, vm.ID)
	if err != nil {
		if errors.Is(err, gateways.ErrIngressUnavailable) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeIngressUnavailable, "no converged gateway for ingress", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "vms.ingress select gateway",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "select gateway", nil)
		return
	}
	if nic.Ipv4Address == nil {
		// An overlay NIC without a CP-IPAM address cannot be brokered: the
		// credential binds to the guest IP. Surface it as retryable.
		response.WriteError(w, r, http.StatusConflict,
			response.CodeIngressUnavailable, "vm has no ingress address yet", nil)
		return
	}

	caRow, err := h.store.ActiveSessionCA(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.ingress load session ca",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "session ca unavailable", nil)
		return
	}
	signer, err := auth.ParseSessionCASigner(caRow.PrivateKeyPEM)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.ingress parse session ca",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "session ca unavailable", nil)
		return
	}

	now := time.Now()
	expiresAt := now.Add(ingressCredTTL)
	cred, err := auth.SignSessionCred(signer, auth.SessionCredClaims{
		VMID:    vm.ID,
		NICMAC:  nic.MacAddress.String(),
		GuestIP: *nic.Ipv4Address,
		Port:    port,
		// LeaseEpoch is reserved for a future bridge-lease check and is not
		// read on the gateway path (which binds by NIC MAC). Left zero.
		LeaseEpoch: 0,
		ExpiresAt:  expiresAt,
	}, now)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.ingress sign session cred",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "sign session credential", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, ingressResponse{
		Transport:   "gateway",
		VMID:        vm.ID.String(),
		Port:        port,
		SplicerAddr: gw.AdvertisedEndpoint,
		SessionCred: cred,
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
	})
}

// resolveIngressNIC inspects the VM's NICs and decides the ingress path. It
// returns the first overlay NIC (the gateway path) when present; otherwise
// hasUsable reports whether any non-overlay (bridge) NIC exists (the relay
// path). A NIC referencing a missing network is skipped (the VM is being torn
// down).
func (h *Handler) resolveIngressNIC(r *http.Request, vmID uuid.UUID) (overlay *store.VMNic, hasUsable bool, err error) {
	nics, err := h.store.ListVMNicsByVM(r.Context(), vmID)
	if err != nil {
		return nil, false, err
	}
	for i := range nics {
		net, nerr := h.store.NetworkByID(r.Context(), nics[i].NetworkID)
		if nerr != nil {
			if errors.Is(nerr, store.ErrNotFound) {
				continue
			}
			return nil, false, nerr
		}
		switch net.Type {
		case store.NetworkTypeOverlay:
			if overlay == nil {
				overlay = &nics[i]
			}
			hasUsable = true
		case store.NetworkTypeBridge:
			hasUsable = true
		}
	}
	return overlay, hasUsable, nil
}

// parseIngressPort decodes the request body and validates the port is in
// 1..65535. On any failure it writes 400 validation_failed and returns ok=false
// so the caller bails.
func parseIngressPort(w http.ResponseWriter, r *http.Request) (port int, ok bool) {
	var req ingressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return 0, false
	}
	if req.Port < 1 || req.Port > 65535 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "port must be in 1..65535", nil)
		return 0, false
	}
	return req.Port, true
}
