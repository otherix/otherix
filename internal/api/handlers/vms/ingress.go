// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/gateways"
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

// ingressMaxRequestBytes caps the broker request body. The body carries a
// single {"port": N} object (< 32 bytes), so 4 KiB is ample headroom. This is
// a defence-in-depth backstop: the route is also mounted under
// middleware.MaxBodyBytes, but this endpoint sits OUTSIDE the Authn group and
// reads its own bearer, so it bounds its own body here too - a caller with any
// non-empty garbage bearer cannot force unbounded buffering. An over-cap read
// fails inside the JSON decode and surfaces as 400 validation_failed.
const ingressMaxRequestBytes int64 = 4 << 10

// ingressRequest is the broker request body: the guest TCP port the caller
// wants to reach. Validated to 1..65535.
type ingressRequest struct {
	Port int `json:"port"`
}

// ingressResponse tells the client where and how to connect. Transport is the
// discriminator the CLI switches on:
//
//   - "gateway": an overlay VM. SplicerAddr is the converged gateway's
//     ingress endpoint and SessionCred is a short-lived bearer the client
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
// connection to a VM. It selects how the client should reach the guest and
// returns the connect coordinates synchronously - the data-plane connect is
// the client's job, so this is a 200, not a 202.
//
// This route is mounted OUTSIDE the global Authn middleware (like ssh-cert and
// relay) so it can accept an ingress-grant token, which is not an Authn
// principal, and structurally guarantee a grant token reaches no other route.
// The handler reads the bearer itself and dual-dispatches:
//
//   - An ingress-grant token (auth.IsIngressGrantFormat, checked first because
//     its prefix is a superset of "otx_") resolves through the store; the
//     caller is authorized when the grant currently reaches the named VM on the
//     requested port AND the caller's source IP satisfies the grant's optional
//     pin. Every grant negative collapses to a uniform 404 so the endpoint
//     leaks neither VM existence nor grant scope.
//   - Any other bearer is verified as a CLI token (JWT or otx_ API token); the
//     caller must hold vm:connect (a role lacking it is 403 permission_denied)
//     and own the VM (scope permitting; a cross-owner or unknown VM is 404).
//
// Both paths converge on brokerIngress. An overlay VM is brokered to a
// converged gateway with a minted session credential (the control plane is out
// of the data path). A bridge VM is brokered over the control-plane relay (no
// gateway credential). A VM with no usable network, or an overlay with no
// converged gateway, is 409 ingress_unavailable.
func (h *Handler) Ingress(w http.ResponseWriter, r *http.Request) {
	vmName := chi.URLParam(r, "id")

	tok, ok := bearerToken(r)
	if !ok {
		h.rejectIngress(w, r)
		return
	}

	port, ok := parseIngressPort(w, r)
	if !ok {
		return
	}

	var vm store.VM
	if auth.IsIngressGrantFormat(tok) {
		vm, ok = h.authorizeIngressGrant(r, tok, vmName, port)
		if !ok {
			// Uniform 404 for every grant negative (bad/expired/revoked grant,
			// out-of-scope VM or port, source-IP pin miss, unknown VM): the
			// endpoint leaks neither VM existence nor grant scope.
			h.rejectIngress(w, r)
			return
		}
	} else {
		vm, ok = h.authorizeIngressCLI(w, r, tok, vmName)
		if !ok {
			// authorizeIngressCLI has already written its own 403/404.
			return
		}
	}

	h.brokerIngress(w, r, vm, port)
}

// authorizeIngressGrant resolves the grant token, checks it currently reaches
// vmName on port and that the caller's source IP satisfies the grant's optional
// pin, then loads and returns the VM. Any failure returns ok=false; the caller
// writes the uniform 404 (no response is written here) so neither VM existence
// nor grant scope leaks.
func (h *Handler) authorizeIngressGrant(r *http.Request, tok, vmName string, port int) (store.VM, bool) {
	grant, err := h.store.IngressGrantByTokenHash(r.Context(), auth.HashToken(tok))
	if err != nil {
		return store.VM{}, false
	}
	// RemoteAddr is host:port; a bare ParseAddr would fail on the port, so
	// parse the pair and take the address half. A parse failure fails closed.
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return store.VM{}, false
	}
	if _, reachable := auth.GrantPrincipalFromStore(grant).CanReach(vmName, port, time.Now()); !reachable {
		return store.VM{}, false
	}
	if !auth.SourceIPAllows(grant.SourceIP, ap.Addr()) {
		return store.VM{}, false
	}
	vm, err := h.store.VMByName(r.Context(), vmName)
	if err != nil {
		return store.VM{}, false
	}
	return vm, true
}

// authorizeIngressCLI verifies a CLI bearer (JWT or otx_ API token) and
// enforces the vm:connect capability + ownership the route's RequirePermission
// middleware used to give. It preserves the 403-vs-404 discipline: a bad token
// is 404 (no oracle); a role lacking vm:connect is 403 permission_denied; an
// unknown or cross-owner VM is 404 (cross-owner invisibility). ok=false means a
// response was already written.
func (h *Handler) authorizeIngressCLI(w http.ResponseWriter, r *http.Request, tok, vmName string) (store.VM, bool) {
	if h.sshDeps.Verifier == nil {
		h.rejectIngress(w, r)
		return store.VM{}, false
	}
	user, err := h.verifyCLIToken(r.Context(), tok)
	if err != nil || user == nil {
		h.rejectIngress(w, r)
		return store.VM{}, false
	}
	if !auth.Has(user.Role, auth.PermVMConnect) {
		response.WriteError(w, r, http.StatusForbidden, response.CodePermissionDenied,
			"vm:connect is not permitted for this role",
			map[string]any{"required_permission": string(auth.PermVMConnect)})
		return store.VM{}, false
	}
	vm, err := h.store.VMByName(r.Context(), vmName)
	if err != nil {
		h.rejectIngress(w, r)
		return store.VM{}, false
	}
	if auth.CheckOwnership(user, &vm.OwnerID, auth.PermVMConnect) != nil {
		// Cross-owner visibility goes through 404, never 403, so the broker
		// never confirms a VM the caller does not own exists.
		h.rejectIngress(w, r)
		return store.VM{}, false
	}
	return vm, true
}

// rejectIngress writes the uniform 404 not_found rejection so neither VM
// existence nor grant scope leaks.
func (h *Handler) rejectIngress(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeVMNotFound, "vm not found", nil)
}

// IngressResult are the connect coordinates the broker computed for one
// (vm, port). On the gateway path SplicerAddr/SessionCred/ExpiresAt are set and
// a session credential has been minted; on the relay path they are zero.
type IngressResult struct {
	Transport   string // "gateway" | "relay"
	VMID        uuid.UUID
	VMName      string
	Port        int
	SplicerAddr string
	SessionCred string
	ExpiresAt   time.Time
}

// ResolveIngress computes connect coordinates for vm:port. It returns
// gateways.ErrIngressUnavailable when the VM currently has no usable network,
// no converged gateway, or no ingress address. It writes nothing to any
// ResponseWriter and persists no durable state.
func (h *Handler) ResolveIngress(ctx context.Context, vm store.VM, port int) (IngressResult, error) {
	overlayNIC, hasUsable, err := h.resolveIngressNICCtx(ctx, vm.ID)
	if err != nil {
		return IngressResult{}, fmt.Errorf("resolve vm network: %v", err)
	}
	if overlayNIC != nil {
		return h.resolveOverlay(ctx, vm, *overlayNIC, port)
	}
	if hasUsable {
		// Bridge VM: brokered over the control-plane relay. No gateway
		// credential is minted; the relay authorizes per request itself.
		return IngressResult{Transport: "relay", VMID: vm.ID, VMName: vm.Name, Port: port}, nil
	}
	// No NIC on any usable network - nothing to broker.
	return IngressResult{}, gateways.ErrIngressUnavailable
}

// resolveOverlay selects a converged gateway, mints a session credential bound
// to the NIC, and returns the coordinates. It is the return-a-value form of the
// old brokerOverlay; ErrIngressUnavailable is returned for a missing gateway or
// a NIC without a guest IP.
func (h *Handler) resolveOverlay(ctx context.Context, vm store.VM, nic store.VMNic, port int) (IngressResult, error) {
	gw, err := gateways.SelectGatewayForVM(ctx, h.store, vm.ID)
	if err != nil {
		if errors.Is(err, gateways.ErrIngressUnavailable) {
			return IngressResult{}, gateways.ErrIngressUnavailable
		}
		return IngressResult{}, fmt.Errorf("select gateway: %v", err)
	}
	if nic.Ipv4Address == nil {
		// An overlay NIC without a CP-IPAM address cannot be brokered: the
		// credential binds to the guest IP. Surface it as retryable.
		return IngressResult{}, gateways.ErrIngressUnavailable
	}

	caRow, err := h.store.ActiveSessionCA(ctx)
	if err != nil {
		return IngressResult{}, fmt.Errorf("session ca unavailable: %v", err)
	}
	signer, err := auth.ParseSessionCASigner(caRow.PrivateKeyPEM)
	if err != nil {
		return IngressResult{}, fmt.Errorf("session ca unavailable: %v", err)
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
		return IngressResult{}, fmt.Errorf("sign session credential: %v", err)
	}

	return IngressResult{
		Transport:   "gateway",
		VMID:        vm.ID,
		VMName:      vm.Name,
		Port:        port,
		SplicerAddr: gw.IngressAdvertisedEndpoint,
		SessionCred: cred,
		ExpiresAt:   expiresAt,
	}, nil
}

// brokerIngress is the shared post-authorization core: it resolves the connect
// coordinates via ResolveIngress and writes them. Success is 200 with the
// transport-specific ingressResponse; ErrIngressUnavailable is 409
// ingress_unavailable; any other error is 500.
func (h *Handler) brokerIngress(w http.ResponseWriter, r *http.Request, vm store.VM, port int) {
	res, err := h.ResolveIngress(r.Context(), vm, port)
	if err != nil {
		if errors.Is(err, gateways.ErrIngressUnavailable) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeIngressUnavailable, "no usable network for ingress", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "vms.ingress resolve",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve vm ingress", nil)
		return
	}

	resp := ingressResponse{Transport: res.Transport, VMID: res.VMID.String(), Port: res.Port}
	if res.Transport == "gateway" {
		resp.SplicerAddr = res.SplicerAddr
		resp.SessionCred = res.SessionCred
		resp.ExpiresAt = res.ExpiresAt.UTC().Format(time.RFC3339)
	}
	response.WriteJSON(w, r, http.StatusOK, resp)
}

// resolveIngressNICCtx inspects the VM's NICs and decides the ingress path. It
// returns the first overlay NIC (the gateway path) when present; otherwise
// hasUsable reports whether any non-overlay (bridge) NIC exists (the relay
// path). A NIC referencing a missing network is skipped (the VM is being torn
// down).
func (h *Handler) resolveIngressNICCtx(ctx context.Context, vmID uuid.UUID) (overlay *store.VMNic, hasUsable bool, err error) {
	nics, err := h.store.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return nil, false, err
	}
	for i := range nics {
		net, nerr := h.store.NetworkByID(ctx, nics[i].NetworkID)
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
// so the caller bails. It caps the body with a MaxBytesReader backstop before
// decoding (the route sits outside the Authn group and reads its own bearer).
func parseIngressPort(w http.ResponseWriter, r *http.Request) (port int, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, ingressMaxRequestBytes)
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
