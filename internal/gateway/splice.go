// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// connectDialTimeout bounds the dial to the target over the overlay. A target
// that is unreachable (still booting, wrong address) fails fast rather than
// holding the request open.
const connectDialTimeout = 10 * time.Second

// Connection-slot caps for the connect/splice plane. They bound the number of
// concurrent spliced sessions a single gateway carries, per VM and in total, so
// one tenant cannot exhaust the gateway's file descriptors or memory and a
// gateway has a hard ceiling on fan-out.
const (
	defaultConnectPerVMCap   = 8
	defaultConnectGatewayCap = 256
)

// dialFunc is the dial seam the connect handler uses to reach the target over
// the overlay. Production wires it to a plain TCP dialer; tests substitute a
// dialer that reaches a loopback stub.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// overlayResolver maps a guest IP to the overlay bridge whose CP-declared subnet
// contains it. The network reconciler (*reconciler.Networks) satisfies it; the
// connect handler uses it to find which overlay datapath a session credential's
// guest IP lives on before binding the dial to the credential MAC.
type overlayResolver interface {
	OverlayBridgeForIP(ip netip.Addr) (bridge string, ok bool)
}

// connectDeps carries the collaborators the connect handler needs beyond its
// logger: the host fabric (for the neighbor-table anti-SSRF check), the overlay
// resolver, and the session-CA store the cred gate verifies against.
type connectDeps struct {
	fabric   netfabric.Fabric
	overlays overlayResolver
	caStore  *sessionCAStore
}

// claimsCtxKey keys the verified session-credential claims the cred gate stashes
// in the request context for the handler.
type claimsCtxKey struct{}

// claimsFromContext returns the verified claims the cred gate placed in ctx.
func claimsFromContext(ctx context.Context) (auth.SessionCredClaims, bool) {
	c, ok := ctx.Value(claimsCtxKey{}).(auth.SessionCredClaims)
	return c, ok
}

// connectHandler serves POST /v1/connect: it verifies a short-lived ingress
// session credential, binds the dial to the credential's NIC MAC via the
// gateway's neighbor table (closing IP-reuse), acquires a concurrency slot, then
// dials the credential's guest target on the overlay and splices the inbound
// connection to it byte for byte. The dial target is taken from the verified
// credential, never from untrusted request input, so the gateway can never be
// steered to an arbitrary address.
type connectHandler struct {
	dial     dialFunc
	fabric   netfabric.Fabric
	overlays overlayResolver
	caStore  *sessionCAStore
	slots    *connectSlots
	log      *slog.Logger
}

// newConnectHandler builds a connect handler with a plain TCP dialer and the
// default connection-slot caps.
func newConnectHandler(deps connectDeps, log *slog.Logger) *connectHandler {
	return &connectHandler{
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		fabric:   deps.fabric,
		overlays: deps.overlays,
		caStore:  deps.caStore,
		slots:    newConnectSlots(defaultConnectPerVMCap, defaultConnectGatewayCap),
		log:      log,
	}
}

// verifyCred is the middleware that gates the connect route. It accepts only an
// "otx_ingress_" bearer verified against the session CA public half learned from
// heartbeat; on success it stashes the verified claims in the request context
// for the handler. Every credential failure (absent, wrong format, bad
// signature, expired, tampered) collapses to a uniform 401 so the gate is no
// oracle; a credential that cannot yet be verified because no session CA has
// been received fails closed with 503.
func (h *connectHandler) verifyCred(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		// IsIngressCredFormat must gate the path before any other token check:
		// "otx_ingress_" is a superset of the API-token prefix "otx_", so the
		// format check routes only ingress credentials here.
		if !ok || !auth.IsIngressCredFormat(token) {
			h.unauthorized(w, r)
			return
		}
		caPub := h.caStore.current()
		if caPub == nil {
			response.WriteError(w, r, http.StatusServiceUnavailable,
				response.CodeIngressUnavailable,
				"ingress session verification is not yet available", nil)
			return
		}
		claims, err := auth.VerifySessionCred(caPub, token, time.Now())
		if err != nil {
			h.unauthorized(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), claimsCtxKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Connect handles POST /v1/connect. The verified credential's claims supply the
// dial target; verifyCred must have run first to place them in the context. The
// handler resolves the overlay the guest IP lives on, refuses unless the guest
// IP's neighbor MAC equals the credential MAC (anti-SSRF / IP-reuse binding),
// acquires a concurrency slot, dials the guest target, hijacks the inbound
// connection, and splices the two legs until either side closes.
func (h *connectHandler) Connect(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		// Defensive: the cred gate must run before this handler.
		h.unauthorized(w, r)
		return
	}

	// Resolve the overlay bridge the guest IP lives on, then bind the dial to the
	// credential MAC: refuse unless the IP currently resolves, in the gateway's
	// neighbor table, to exactly the credential's NIC MAC. A stale credential
	// whose guest IP has been reassigned to a different NIC resolves to a
	// different MAC and is refused. Every binding failure collapses to a uniform
	// refusal so the gateway is no oracle and never dials on uncertain input.
	bridge, ok := h.overlays.OverlayBridgeForIP(claims.GuestIP)
	if !ok {
		h.log.Warn("connect: refused, guest ip not on any declared overlay",
			"guest_ip", claims.GuestIP.String(), "vm_id", claims.VMID.String())
		h.refuse(w, r)
		return
	}
	mac, ok, err := h.fabric.NeighborMAC(bridge, claims.GuestIP)
	if err != nil || !ok || !macEqual(mac, claims.NICMAC) {
		h.log.Warn("connect: refused, neighbor mac does not match credential",
			"guest_ip", claims.GuestIP.String(), "bridge", bridge,
			"vm_id", claims.VMID.String(), "resolved", ok, "error", errString(err))
		h.refuse(w, r)
		return
	}

	// Acquire a concurrency slot before dialing. Released on every exit path
	// (defer), including after the splice tears down, so a freed slot lets a
	// later connect through.
	vmID := claims.VMID.String()
	if err := h.slots.acquire(vmID); err != nil {
		response.WriteError(w, r, http.StatusServiceUnavailable,
			response.CodeIngressUnavailable, "gateway connection capacity reached", nil)
		return
	}
	defer h.slots.release(vmID)

	target := net.JoinHostPort(claims.GuestIP.String(), strconv.Itoa(claims.Port))

	hj, ok := w.(http.Hijacker)
	if !ok {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "connection does not support splicing", nil)
		return
	}

	// Dial the target before hijacking so a dial failure is reported as a
	// normal HTTP error rather than a half-open hijacked socket.
	dialCtx, dialCancel := context.WithTimeout(r.Context(), connectDialTimeout)
	upstream, err := h.dial(dialCtx, "tcp", target)
	dialCancel()
	if err != nil {
		h.log.Warn("connect: dial target failed", "target", target, "error", err.Error())
		response.WriteError(w, r, http.StatusBadGateway,
			response.CodeAgentUnreachable, "could not reach the forward target", nil)
		return
	}

	conn, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		h.log.Error("connect: hijack failed", "target", target, "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	// Clear any read/write deadline the HTTP server armed for the request so the
	// long-lived spliced session is not torn at the listener's timeout.
	_ = conn.SetDeadline(time.Time{})

	// Signal the caller the pipe is open; everything after is raw spliced bytes.
	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		_ = upstream.Close()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spliceConns(ctx, cancel, conn, upstream)
	h.log.Info("connect: session closed", "target", target, "vm_id", vmID)
}

// unauthorized writes the uniform 401 the cred gate uses for every credential
// failure, leaking nothing about which check failed.
func (h *connectHandler) unauthorized(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusUnauthorized,
		response.CodeUnauthenticated, "a valid ingress session credential is required", nil)
}

// refuse writes the uniform 403 the handler uses for every anti-SSRF binding
// failure (guest IP off-overlay, unresolved neighbor, MAC mismatch), so a holder
// of a valid-but-stale credential learns only that the target is no longer
// authorized, never why.
func (h *connectHandler) refuse(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusForbidden,
		response.CodePermissionDenied,
		"the credential does not authorize a connection to this target", nil)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
// ok is false when the header is absent or not a bearer.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

// macEqual reports whether the kernel-resolved neighbor MAC a equals the
// credential MAC string b. Both sides are normalized (b is parsed; a is already
// a parsed hardware address) and compared byte for byte so formatting
// differences (case, separators) never cause a false mismatch.
func macEqual(a net.HardwareAddr, b string) bool {
	pb, err := net.ParseMAC(b)
	if err != nil {
		return false
	}
	return bytes.Equal(a, pb)
}

// errString returns err.Error() or "" for a nil error, for structured logging.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// connectSlots enforces the per-VM and per-gateway concurrency caps on the
// connect/splice plane, mirroring the agent's ssh-pipe slot accounting.
type connectSlots struct {
	mu         sync.Mutex
	perVM      map[string]int
	total      int
	perVMCap   int
	gatewayCap int
}

// newConnectSlots builds a slot accountant with the given per-VM and per-gateway
// caps.
func newConnectSlots(perVMCap, gatewayCap int) *connectSlots {
	return &connectSlots{
		perVM:      map[string]int{},
		perVMCap:   perVMCap,
		gatewayCap: gatewayCap,
	}
}

// acquire reserves a slot for vmID, enforcing the per-VM and per-gateway caps. It
// returns an error (and reserves nothing) when either cap is already reached.
func (s *connectSlots) acquire(vmID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= s.gatewayCap {
		return errors.New("connect: gateway capacity reached")
	}
	if s.perVM[vmID] >= s.perVMCap {
		return errors.New("connect: vm capacity reached")
	}
	s.perVM[vmID]++
	s.total++
	return nil
}

// release returns a slot previously taken by acquire.
func (s *connectSlots) release(vmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perVM[vmID] > 0 {
		s.perVM[vmID]--
		if s.perVM[vmID] == 0 {
			delete(s.perVM, vmID)
		}
		s.total--
	}
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
