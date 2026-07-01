// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/wskeepalive"
)

// SSHStream implements GET /v1/vms/{id}/ssh-stream - the CP-side WebSocket
// relay that carries an SSH session from the operator/connector to the
// guest. The CP authorizes the connect, dials the owning agent's ssh-pipe
// endpoint over mTLS, and pumps raw bytes both ways. The inner SSH is
// end-to-end (the operator's client speaks SSH to the guest sshd via the
// minted guest cert); the CP transports ciphertext only and never inspects
// it. This is the L4 sibling of ConsoleStream, dialing ssh-pipe instead of
// the agent's console-stream.
//
// This route is mounted OUTSIDE the global Authn middleware (like
// console-stream and ssh-cert) so it can accept an ingress-grant token, which
// is not an Authn principal, and structurally guarantee a grant token
// reaches no other route. The handler reads the bearer itself and
// dual-dispatches:
//
//   - An ingress-grant token (auth.IsIngressGrantFormat, checked first because
//     its prefix is a superset of "otx_") resolves through the store; the
//     caller is authorized when the grant currently reaches the named VM.
//   - Any other bearer is verified as a CLI token (JWT or otx_ API token);
//     the caller must hold vm:ssh and own the VM (scope permitting).
//
// Single-step bearer auth keeps every replica interchangeable: all shared
// state (grant, VM, node) lives in etcd, so any replica authorizes
// identically with no ephemeral token and no stickiness.
//
// Every reachable failure (missing/garbage token, unknown VM, unauthorized
// caller, bad/expired/revoked grant, node unresolved, agent dial failed)
// collapses to one uniform 401 ssh_session_rejected so the endpoint never
// leaks VM existence. Authorization, VM load, node resolve, and the agent
// dial all happen BEFORE the WebSocket upgrade, so the uniform 401 is always
// writable - there is no post-upgrade error path that could diverge.
//
// Out of scope (v1): following the session across a VM live migration or a
// CP-replica failover. A broken upstream simply tears the session down and
// the operator reconnects; there is no re-attach loop here (unlike
// ConsoleStream).
func (h *Handler) SSHStream(w http.ResponseWriter, r *http.Request) {
	if h.consoleDeps.AgentClient == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "ssh relay client not configured", nil)
		return
	}

	vmName := chi.URLParam(r, "id")
	tok, ok := bearerToken(r)
	if !ok {
		h.rejectSSH(w, r)
		return
	}

	// The guest port the operator wants to reach. Absent means 22 (the bare
	// `otherix ssh` re-home); a present value must be a valid TCP port. A bad
	// port collapses to the same uniform reject as every other failure so the
	// endpoint stays free of an enumeration oracle.
	port, ok := sshStreamPort(r)
	if !ok {
		h.rejectSSH(w, r)
		return
	}

	if !h.authorizeSSHStream(r.Context(), tok, vmName, port, time.Now()) {
		h.rejectSSH(w, r)
		return
	}

	// Load the VM and resolve its owning node to build the upstream URL. A
	// failure here is anti-enumeration-collapsed to the same uniform 401:
	// for a grant-authorized caller these reads only fail transiently (node
	// down mid-connect), and collapsing keeps the unknown-VM and node-down
	// cases indistinguishable to an unauthorized prober.
	vm, err := h.store.VMByName(r.Context(), vmName)
	if err != nil {
		h.rejectSSH(w, r)
		return
	}
	node, err := h.resolveConsoleNode(r.Context(), vm)
	if err != nil {
		h.log.WarnContext(r.Context(), "vms.sshStream resolve node",
			"vm", vmName, "error", err.Error())
		h.rejectSSH(w, r)
		return
	}

	// Dial the agent's ssh-pipe over the agentclient's mTLS-configured HTTP
	// client. The dial precedes the downstream Accept, so a dial failure
	// (agent unreachable, VM not running on the agent) still collapses to the
	// uniform 401 - no existence signal, no half-open downstream.
	upstream, _, err := websocket.Dial(r.Context(),
		agentclient.BuildSSHPipeURL(node.host, vmName, port),
		&websocket.DialOptions{HTTPClient: h.consoleDeps.AgentClient.HTTPClient()})
	if err != nil {
		h.log.WarnContext(r.Context(), "vms.sshStream dial upstream",
			"vm", vmName, "error", err.Error())
		h.rejectSSH(w, r)
		return
	}
	defer func() { _ = upstream.Close(websocket.StatusInternalError, "") }()

	// Clear hijacked connection deadlines - http.Server read/write deadlines
	// persist on the net.Conn after Accept hijacks it, and would otherwise
	// kill the long-lived relay at ~30s. Logged at WARN rather than bailing.
	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.WarnContext(r.Context(), "vms.sshStream clear read deadline",
			"vm", vmName, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.WarnContext(r.Context(), "vms.sshStream clear write deadline",
			"vm", vmName, "error", derr.Error())
	}

	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept already wrote a response; the deferred upstream Close cleans
		// up the agent leg.
		h.log.ErrorContext(r.Context(), "vms.sshStream downstream accept",
			"vm", vmName, "error", err.Error())
		return
	}
	defer func() { _ = downstream.Close(websocket.StatusInternalError, "") }()

	relaySSH(r.Context(), downstream, upstream)
}

// sshStreamPort reads the guest TCP port the client wants to reach from the
// "port" query parameter. An absent port defaults to 22 (the SSH re-home for a
// bare `otherix ssh`); a present port must parse and fall in 1..65535. It
// returns ok=false on a malformed or out-of-range value so the caller collapses
// it to the endpoint's single uniform reject. The port is the only
// client-influenced input the relay forwards; the dial host stays lease-derived
// on the agent.
func sshStreamPort(r *http.Request) (port int, ok bool) {
	raw := r.URL.Query().Get("port")
	if raw == "" {
		return 22, true
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// authorizeSSHStream reports whether the bearer authorizes a session to vmName
// on port at time now. It dual-dispatches grant vs CLI exactly like the
// cert-mint endpoint but needs no login (the inner SSH cert carries the
// principal) and writes no response: every failure is the caller's single
// uniform reject, so this returns a bare bool. The port is the actual requested
// guest port: ssh_stream is the generic bridge relay for arbitrary ports, so a
// grant must authorize the exact port the relay would forward, not a constant.
func (h *Handler) authorizeSSHStream(ctx context.Context, tok, vmName string, port int, now time.Time) bool {
	if auth.IsIngressGrantFormat(tok) {
		grant, err := h.store.IngressGrantByTokenHash(ctx, auth.HashToken(tok))
		if err != nil {
			return false
		}
		_, reachable := auth.GrantPrincipalFromStore(grant).CanReach(vmName, port, now)
		return reachable
	}
	if h.sshDeps.Verifier == nil {
		return false
	}
	user, err := h.verifyCLIToken(ctx, tok)
	if err != nil || user == nil {
		return false
	}
	vm, err := h.store.VMByName(ctx, vmName)
	if err != nil {
		return false
	}
	if !auth.Has(user.Role, auth.PermVMSSH) {
		return false
	}
	return auth.CheckOwnership(user, &vm.OwnerID, auth.PermVMSSH) == nil
}

// relaySSH pumps raw bytes bidirectionally between the operator (downstream)
// and the agent ssh-pipe (upstream) until either side closes, the relay
// context is cancelled, or a keepalive ping fails on either leg. It wraps
// each WebSocket in a net.Conn and io.Copy-s both directions; the first
// copy to return cancels the shared context, which unblocks the other copy
// and both keepalive pumps, so neither leg (operator WS, agent WS) is left
// open and no goroutine leaks. It does not close either conn - SSHStream
// owns lifecycle via its defers.
func relaySSH(parent context.Context, downstream, upstream *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	dconn := websocket.NetConn(ctx, downstream, websocket.MessageBinary)
	uconn := websocket.NetConn(ctx, upstream, websocket.MessageBinary)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		_, _ = io.Copy(uconn, dconn)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		_, _ = io.Copy(dconn, uconn)
	}()
	// Keepalive on both legs: the io.Copy reads above are the concurrent
	// readers each conn's Ping needs to observe its pong. A half-open peer
	// (slept laptop, dropped network, killed client) trips a ping timeout and
	// cancels the session within ~interval+timeout, which TCP keepalives
	// (~2h) would never catch.
	go wskeepalive.Run(ctx, cancel, downstream, wskeepalive.DefaultInterval, wskeepalive.DefaultTimeout)
	go wskeepalive.Run(ctx, cancel, upstream, wskeepalive.DefaultInterval, wskeepalive.DefaultTimeout)

	wg.Wait()
}
