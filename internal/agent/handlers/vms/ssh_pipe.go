// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	avm "github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/wskeepalive"
)

// defaultSSHPipePort is the guest port the L4 pipe dials when the request omits
// one, preserving the bare `otherix ssh` re-home to sshd. The wire never
// supplies the dial host - only the VM name (path) and the port (query) - so
// the port is the only caller-influenced input the pipe honors.
const defaultSSHPipePort = 22

// sshDialTimeout bounds the connect to the guest's sshd. A guest that is up but
// not yet listening on 22 fails fast rather than holding the request open.
const sshDialTimeout = 10 * time.Second

// defaultSSHPerVMCap and defaultSSHAgentCap bound concurrent ssh pipes so a
// runaway relay cannot exhaust the agent's sockets/goroutines: at most N pipes
// per VM and a global ceiling across all VMs on the node.
const (
	defaultSSHPerVMCap = 4
	defaultSSHAgentCap = 256
)

// byNameManager is the narrow manager surface the ssh-pipe handler needs.
// *vm.Manager satisfies it; tests substitute a fake.
type byNameManager interface {
	ByName(name string) (*avm.VM, error)
}

// ipByMAC is the narrow DHCP-reservation lookup the handler needs. The dhcp4
// responder satisfies it; tests substitute a fake. The IP is the agent's own
// CP-IPAM lease (pushed via heartbeat), never anything from the request.
type ipByMAC interface {
	LookupByMAC(mac string) (netip.Addr, bool)
}

// resolveSSHTarget maps a VM name and a guest port to "ip:port" using ONLY the
// agent's local state: the VM must be one this agent currently hosts and runs,
// and the dialed IP is the agent's own DHCP-reservation lease for one of the
// VM's NIC MACs (never from the request). This is the anti-SSRF boundary - a
// caller cannot steer the agent at an arbitrary address - and it covers any
// Otherix-managed-DHCP network (overlay or managed bridge). The port selects
// only the guest port (SSH, psql, ...); it never influences the host.
func resolveSSHTarget(mgr byNameManager, leases ipByMAC, name string, port int) (string, error) {
	v, err := mgr.ByName(name)
	if err != nil {
		return "", fmt.Errorf("ssh target: %w", err)
	}
	if v.Status != avm.StatusRunning {
		return "", errors.New("ssh target: vm not running")
	}
	for _, n := range v.NICs {
		if ip, ok := leases.LookupByMAC(n.MAC); ok && ip.IsValid() {
			return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
		}
	}
	return "", errors.New("ssh target: no managed-DHCP lease")
}

// pipePort reads the guest port from the request's "port" query parameter. An
// absent port defaults to 22 (the SSH re-home). A present port must parse and
// fall in 1..65535; anything else is rejected so a malformed CP-relayed request
// never dials port 0 or a garbage target. The agent listener is CP-only mTLS,
// so this only ever guards against a contract violation by the relay.
func pipePort(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("port")
	if raw == "" {
		return defaultSSHPipePort, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("ssh pipe: parse port: %v", err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("ssh pipe: port %d out of range", port)
	}
	return port, nil
}

// SSHPipe handles GET /v1/vms/{vm_name}/ssh-pipe: an mTLS WebSocket the CP relay
// dials, spliced byte-for-byte to the guest's TCP port. The inner protocol is
// end-to-end between the operator and the guest; the agent transports
// ciphertext only and resolves the dial target purely from its own local state
// (see resolveSSHTarget) - the wire carries only the VM name (path) and the
// guest port (query, default 22), never the dial host.
func (h *Handler) SSHPipe(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")

	port, err := pipePort(r)
	if err != nil {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}

	target, err := resolveSSHTarget(h.manager, h.leases, name, port)
	if err != nil {
		// Uniform rejection: do not distinguish unknown / not-local /
		// not-running / no-lease. The agent listener is mTLS CP-only, so the CP
		// collapses this to the generic operator-facing reject.
		http.Error(w, "ssh pipe unavailable", http.StatusNotFound)
		return
	}

	if err := h.acquireSSHSlot(name); err != nil {
		http.Error(w, "ssh pipe busy", http.StatusConflict)
		return
	}
	defer h.releaseSSHSlot(name)

	dialCtx, dialCancel := context.WithTimeout(r.Context(), sshDialTimeout)
	tcp, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", target)
	dialCancel()
	if err != nil {
		http.Error(w, "ssh pipe dial failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = tcp.Close() }()

	// Clear the hijacked connection's read/write deadlines so the long-lived
	// pipe is not killed at the server's ReadTimeout/WriteTimeout once the
	// response is hijacked (the keepalive ping below is itself a write). Mirrors
	// the console-stream handler; logged at WARN, not fatal.
	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.Warn("ssh pipe: clear read deadline", "vm", name, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.Warn("ssh pipe: clear write deadline", "vm", name, "error", derr.Error())
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.log.Error("ssh pipe: websocket upgrade failed", "vm", name, "error", err.Error())
		return // Accept wrote the response
	}
	defer func() { _ = ws.Close(websocket.StatusInternalError, "") }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Keepalive on the WebSocket leg so an intermediate proxy in front of the CP
	// (which front-proxies the operator leg) does not tear a quiet session; a
	// dead peer cancels ctx and tears both directions down. websocket.NetConn's
	// reader below drains the pongs the Ping needs.
	go wskeepalive.Run(ctx, cancel, ws, wskeepalive.DefaultInterval, wskeepalive.DefaultTimeout)

	wsConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	spliceConns(ctx, cancel, wsConn, tcp)
	h.log.Info("ssh pipe: session closed", "vm", name)
}

// spliceConns copies bytes both directions until either side closes or ctx is
// cancelled, then tears both legs down (the kill-implies-teardown invariant: no
// goroutine, fd, or slot survives any exit path).
func spliceConns(ctx context.Context, cancel context.CancelFunc, a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)

	select {
	case <-ctx.Done():
	case <-done:
	}
	cancel()
	_ = a.Close()
	_ = b.Close()
}

// acquireSSHSlot reserves a concurrency slot for an ssh pipe to name, enforcing
// the per-VM and per-agent caps. It returns an error (and reserves nothing) when
// either cap is already reached.
func (h *Handler) acquireSSHSlot(name string) error {
	h.sshMu.Lock()
	defer h.sshMu.Unlock()
	if h.sshTotal >= h.sshAgentCap {
		return errors.New("ssh pipe: agent capacity reached")
	}
	if h.sshPerVM[name] >= h.sshPerVMCap {
		return errors.New("ssh pipe: vm capacity reached")
	}
	h.sshPerVM[name]++
	h.sshTotal++
	return nil
}

// releaseSSHSlot returns a slot previously taken by acquireSSHSlot.
func (h *Handler) releaseSSHSlot(name string) {
	h.sshMu.Lock()
	defer h.sshMu.Unlock()
	if h.sshPerVM[name] > 0 {
		h.sshPerVM[name]--
		if h.sshPerVM[name] == 0 {
			delete(h.sshPerVM, name)
		}
		h.sshTotal--
	}
}
