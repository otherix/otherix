// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingress

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/api/response"
)

// overlayWGDevice is the single WireGuard interface a NAT'd node's overlay IP is
// reachable over. The production dial binds to it (SO_BINDTODEVICE) so the splice
// egresses the overlay, never a route-table-selected device. Mirrors the
// reconciler's wgInterfaceName; kept local to avoid importing that package.
const overlayWGDevice = "otwg0"

// nodeConnectMaxBody caps the CP-supplied target body. It is a two-field JSON
// object; a few hundred bytes is ample, and the cap stops a hostile body being
// read unbounded before it is rejected.
const nodeConnectMaxBody = 4 << 10

// defaultNodeConnectCap bounds the concurrent CP splices one gateway carries.
// The control plane opens one session per cold connection to a node behind it,
// and its streaming proxies turn a single user request (a followed serial log,
// for instance) into one long-lived session, so the count is user-driven even
// though only the control plane dials this route. Steady state is a handful of
// connections per node, so a few hundred is orders of magnitude above normal
// while still bounding the descriptors and buffers one gateway can be made to
// hold - and the gateway shares those descriptors with its other planes, whose
// own caps would not otherwise protect them.
const defaultNodeConnectCap = 256

// nodeConnectSlotKey is the single accounting bucket the node-connect plane
// uses. connectSlots keys per VM for the credential-gated splice; a node-connect
// target is a NODE, so there is no second dimension to split on and both caps
// collapse onto one total.
const nodeConnectSlotKey = "node-connect"

// NodeConnectDeps carries what the gateway CP-only /v1/connect-node splice needs.
// The route is reached only by the control plane (RequireCPIdentity gates the
// router it mounts on), so there is no per-request credential here; the security
// seam is the target validation.
type NodeConnectDeps struct {
	// IsKnownNodeOverlayIP reports whether an IP is the overlay address of a node
	// the gateway currently meshes with (a CP-declared WireGuard peer). It is the
	// load-bearing anti-SSRF gate: only a meshed node's overlay IP may be spliced,
	// never a guest VM address or the anycast service IP.
	IsKnownNodeOverlayIP func(ip netip.Addr) bool
	// ControlPort is the only port a target may name (the agent control listener
	// port, a cluster convention). A target on any other port is refused.
	ControlPort int
	// dial reaches the validated target over the overlay. Nil defaults to a dialer
	// bound to otwg0; tests inject a loopback dialer.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Log records refused targets (audit) and splice lifecycle. Nil defaults to
	// slog.Default().
	Log *slog.Logger
}

// NodeConnectHandler serves POST /v1/connect-node: the control plane names a
// target (overlay_ip, port); on success the handler validates the target against
// the known-node overlay set, hijacks the inbound connection, dials the target
// over the overlay, and splices raw bytes. The inner bytes are opaque (the CP
// runs an end-to-end agent TLS over the tunnel); the gateway never terminates or
// inspects them.
type NodeConnectHandler struct {
	isKnownNodeOverlayIP func(ip netip.Addr) bool
	controlPort          int
	dial                 func(ctx context.Context, network, addr string) (net.Conn, error)
	slots                *connectSlots
	log                  *slog.Logger
}

// NewNodeConnectHandler builds the handler, defaulting the dial to an
// otwg0-bound dialer and the logger to slog.Default() when unset.
func NewNodeConnectHandler(deps NodeConnectDeps) *NodeConnectHandler {
	dial := deps.dial
	if dial == nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Control: netfabric.BindToDeviceControl(overlayWGDevice)}).DialContext(ctx, network, addr)
		}
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	return &NodeConnectHandler{
		isKnownNodeOverlayIP: deps.IsKnownNodeOverlayIP,
		controlPort:          deps.ControlPort,
		dial:                 dial,
		slots:                newConnectSlots(defaultNodeConnectCap, defaultNodeConnectCap),
		log:                  log,
	}
}

// nodeConnectTarget is the CP-supplied dial target. Only these two fields are
// read from the request; the dial address is rebuilt from the validated values,
// so no other request input can steer the dial.
type nodeConnectTarget struct {
	OverlayIP string `json:"overlay_ip"`
	Port      int    `json:"port"`
}

// Connect handles POST /v1/connect-node. It validates the CP-supplied target
// fail-closed (must be a known-node overlay IP on the control port), takes a
// concurrency slot, then hijacks the inbound connection, dials the target over
// the overlay, and splices raw bytes until either side closes or the session
// idles out.
func (h *NodeConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
	ip, port, ok := h.validateTarget(w, r)
	if !ok {
		return // validateTarget wrote the refusal
	}

	// Rebuild the dial address from the validated values only.
	target := net.JoinHostPort(ip.String(), strconv.Itoa(port))

	// Take a slot before the dial and release it on every exit path, exactly as
	// the credential-gated splice does. Each session costs two goroutines, two
	// descriptors and two buffers, and one is opened per control-plane connection,
	// so an unbounded count is a gateway-wide exhaustion the other planes on this
	// process would feel too. Refusing at the cap is transient and retryable; the
	// caller sees the same 503 the sibling route returns.
	if err := h.slots.acquire(nodeConnectSlotKey); err != nil {
		response.WriteError(w, r, http.StatusServiceUnavailable,
			response.CodeIngressUnavailable, "gateway connection capacity reached", nil)
		return
	}
	defer h.slots.release(nodeConnectSlotKey)

	hj, ok := w.(http.Hijacker)
	if !ok {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "connection does not support splicing", nil)
		return
	}

	// Dial before hijacking so a dial failure is a normal HTTP error rather than a
	// half-open hijacked socket.
	dialCtx, dialCancel := context.WithTimeout(r.Context(), connectDialTimeout)
	upstream, err := h.dial(dialCtx, "tcp", target)
	dialCancel()
	if err != nil {
		h.log.Warn("connect-node: dial target failed", "target", target, "error", err.Error())
		response.WriteError(w, r, http.StatusBadGateway,
			response.CodeAgentUnreachable, "could not reach the target node", nil)
		return
	}

	conn, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		h.log.Error("connect-node: hijack failed", "target", target, "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	// Clear the deadline the HTTP server armed for the request so the long-lived
	// spliced session is not torn at the listener's timeout.
	_ = conn.SetDeadline(time.Time{})

	// Signal the CP the pipe is open; everything after is opaque spliced bytes (the
	// CP's end-to-end agent TLS), which the gateway forwards without inspection.
	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		_ = upstream.Close()
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	spliceConns(ctx, cancel, conn, upstream, connectIdleTimeout)
	h.log.Info("connect-node: session closed", "target", target)
}

// validateTarget parses the CP-supplied body and returns the target (ip, port)
// only when it is a known-node overlay IP on the control port. Every other input
// - a malformed body, an unparseable IP, an unknown IP, or the wrong port - fails
// closed with a 4xx and returns ok=false, so the handler never dials on uncertain
// input. A malformed body is a 400; a well-formed but unauthorized target is a
// 403 (mirrors the anti-SSRF refusal on the sibling /v1/connect path).
func (h *NodeConnectHandler) validateTarget(w http.ResponseWriter, r *http.Request) (ip netip.Addr, port int, ok bool) {
	var body nodeConnectTarget
	if err := json.NewDecoder(io.LimitReader(r.Body, nodeConnectMaxBody)).Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "malformed connect-node target", nil)
		return netip.Addr{}, 0, false
	}
	parsed, err := netip.ParseAddr(body.OverlayIP)
	if err != nil || body.Port != h.controlPort || h.isKnownNodeOverlayIP == nil || !h.isKnownNodeOverlayIP(parsed.Unmap()) {
		h.log.Warn("connect-node: refused target",
			"overlay_ip", body.OverlayIP, "port", body.Port)
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied,
			"the target is not a known node on the control port", nil)
		return netip.Addr{}, 0, false
	}
	return parsed.Unmap(), body.Port, true
}

// MountNodeConnectRoute mounts POST /v1/connect-node on r. The caller MUST mount
// it OUTSIDE any per-request Timeout group: the route hijacks and splices a
// long-lived tunnel, which a request deadline would tear and whose guarded writer
// does not support hijacking. Access control is the router's CP-identity gate,
// applied at the router root by the caller (this route carries no bearer).
func MountNodeConnectRoute(r chi.Router, h *NodeConnectHandler) {
	r.Post("/v1/connect-node", h.Connect)
}
