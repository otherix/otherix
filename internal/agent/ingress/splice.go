// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ingress provides the VM ingress connection splicer and the
// session-CA store shared by the standalone gateway daemon and the agent
// runtime. It verifies a short-lived ingress session credential, binds the
// dial to the credential's NIC MAC via the host neighbor table (anti-SSRF),
// and splices the inbound connection to the credential's guest target on the
// overlay.
package ingress

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

// connectIdleTimeout tears down a spliced /v1/connect session that has carried
// zero bytes in BOTH directions for this long. It matches the AWS-NLB
// idle-timeout default (350s) and the sibling published-port datapath
// (reconciler.publishedIdleTimeout): a truly idle session is reclaimed so a
// per-VM/per-gateway slot cannot be pinned open indefinitely, while any byte in
// either direction (SSH/keepalive traffic on an interactive session) re-arms the
// window so a live session is never torn. The credential that opens a session is
// reusable within its short TTL and the caller controls its own backends, so
// credential-gating alone does not stop a caller from pinning slots; the idle
// reclaim closes that cross-tenant exhaustion. This intentionally reverses the
// sibling's note that the ingress path was "deliberately left unchanged" -
// the reusable-credential slot-pinning DoS is the reason it now changes.
const connectIdleTimeout = 350 * time.Second

// Connection-slot caps for the connect/splice plane. They bound the number of
// concurrent spliced sessions a single gateway carries, per VM and in total, so
// one tenant cannot exhaust the gateway's file descriptors or memory and a
// gateway has a hard ceiling on fan-out.
const (
	defaultConnectPerVMCap   = 8
	defaultConnectGatewayCap = 256
)

// dialFunc is the dial seam the connect handler uses to reach the target over
// the overlay. device is the per-overlay veth host-end device the guest IP was
// validated on; the production dialer binds the connection to it (SO_BINDTODEVICE)
// so the dial egresses the same datapath the anti-SSRF neighbor check ran on,
// never a route-table-selected device from a different overlay. Tests substitute
// a dialer that reaches a loopback stub and ignore device.
type dialFunc func(ctx context.Context, network, addr, device string) (net.Conn, error)

// overlayCandidate is one gateway-overlay whose CP-declared subnet contains a
// guest IP: the per-overlay veth host-end Device for the dial binding + neighbor
// probe, and the NetworkID for session accounting. It is a type alias to a plain
// anonymous struct that is byte-identical to reconciler.OverlayCandidate, so
// *reconciler.Networks satisfies OverlayResolver structurally without this package
// importing reconciler (dependency inversion is preserved).
type overlayCandidate = struct {
	Device    string
	NetworkID string
}

// OverlayResolver maps a guest IP to the overlay datapath(s) whose CP-declared
// subnet contains it and tracks live ingress sessions per network. The network
// reconciler (*reconciler.Networks) satisfies it. OverlayCandidatesForIP returns
// every gateway overlay a session credential's guest IP could live on (overlay
// subnets are per-tenant and may overlap, so more than one can match on subnet
// alone); the connect path disambiguates by the per-overlay veth neighbor MAC.
// AcquireSession/ReleaseSession maintain the per-network live-session counter the
// gateway reports through its heartbeat so the CP keeps the gateway's membership
// sticky while sessions drain.
type OverlayResolver interface {
	OverlayCandidatesForIP(ip netip.Addr) []overlayCandidate
	AcquireSession(networkID string)
	ReleaseSession(networkID string)
}

// ConnectDeps carries the collaborators the connect handler needs beyond its
// logger: the host fabric (for the neighbor-table anti-SSRF check), the overlay
// resolver, and the session-CA store the cred gate verifies against.
type ConnectDeps struct {
	Fabric   netfabric.Fabric
	Overlays OverlayResolver
	CAStore  *SessionCAStore
}

// claimsCtxKey keys the verified session-credential claims the cred gate stashes
// in the request context for the handler.
type claimsCtxKey struct{}

// claimsFromContext returns the verified claims the cred gate placed in ctx.
func claimsFromContext(ctx context.Context) (auth.SessionCredClaims, bool) {
	c, ok := ctx.Value(claimsCtxKey{}).(auth.SessionCredClaims)
	return c, ok
}

// ConnectHandler serves POST /v1/connect: it verifies a short-lived ingress
// session credential, binds the dial to the credential's NIC MAC via the
// gateway's neighbor table (closing IP-reuse), acquires a concurrency slot, then
// dials the credential's guest target on the overlay and splices the inbound
// connection to it byte for byte. The dial target is taken from the verified
// credential, never from untrusted request input, so the gateway can never be
// steered to an arbitrary address.
type ConnectHandler struct {
	dial     dialFunc
	fabric   netfabric.Fabric
	overlays OverlayResolver
	caStore  *SessionCAStore
	slots    *connectSlots
	log      *slog.Logger
}

// NewConnectHandler builds a connect handler with a plain TCP dialer and the
// default connection-slot caps.
func NewConnectHandler(deps ConnectDeps, log *slog.Logger) *ConnectHandler {
	return &ConnectHandler{
		dial: func(ctx context.Context, network, addr, device string) (net.Conn, error) {
			return (&net.Dialer{Control: netfabric.BindToDeviceControl(device)}).DialContext(ctx, network, addr)
		},
		fabric:   deps.Fabric,
		overlays: deps.Overlays,
		caStore:  deps.CAStore,
		slots:    newConnectSlots(defaultConnectPerVMCap, defaultConnectGatewayCap),
		log:      log,
	}
}

// VerifyCred is the middleware that gates the connect route. It accepts only an
// "otx_ingress_" bearer verified against the session CA public half learned from
// heartbeat; on success it stashes the verified claims in the request context
// for the handler. Every credential failure (absent, wrong format, bad
// signature, expired, tampered) collapses to a uniform 401 so the gate is no
// oracle; a credential that cannot yet be verified because no session CA has
// been received fails closed with 503.
func (h *ConnectHandler) VerifyCred(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		// IsIngressCredFormat must gate the path before any other token check:
		// "otx_ingress_" is a superset of the API-token prefix "otx_", so the
		// format check routes only ingress credentials here.
		if !ok || !auth.IsIngressCredFormat(token) {
			h.unauthorized(w, r)
			return
		}
		caPub := h.caStore.Current()
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
// dial target; VerifyCred must have run first to place them in the context. The
// handler enumerates the candidate overlays the guest IP could live on, acquires a
// concurrency slot, then binds the dial to the credential MAC (refuses unless a
// candidate overlay's neighbor table resolves the guest IP to exactly the
// credential MAC - anti-SSRF / IP-reuse binding, and the disambiguator for
// overlapping overlay subnets), dials the guest target, hijacks the inbound
// connection, and splices the two legs until either side closes.
func (h *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		// Defensive: the cred gate must run before this handler.
		h.unauthorized(w, r)
		return
	}

	// Enumerate the candidate overlays whose CP-declared subnet contains the guest
	// IP. Overlay subnets are per-tenant, isolated domains and may overlap, so more
	// than one can match on subnet alone; the neighbor-MAC probe below disambiguates.
	// An empty set means the guest IP is on no declared gateway overlay - refuse for
	// free before taking a slot.
	candidates := h.overlays.OverlayCandidatesForIP(claims.GuestIP)
	if len(candidates) == 0 {
		h.log.Warn("connect: refused, guest ip not on any declared overlay",
			"guest_ip", claims.GuestIP.String(), "vm_id", claims.VMID.String())
		h.refuse(w, r)
		return
	}

	// Acquire a concurrency slot BEFORE the neighbor probe (not just before the
	// dial). On a cold miss each probe can block up to the fabric's neighbor-resolve
	// budget (~2s) while holding the hijackable request connection; the ingress
	// credential is reusable within its TTL and the caller controls its backends,
	// so one valid credential can be replayed to open unbounded concurrent probes.
	// The per-VM/per-gateway cap bounds concurrent probes to the same ceiling it
	// bounds spliced sessions. Released on every exit path (defer), including a
	// MAC-mismatch refusal and after the splice tears down, so a freed slot lets a
	// later connect through. The off-overlay refusal above returns for free before
	// this point (a cheap map lookup, no slot).
	vmID := claims.VMID.String()
	if err := h.slots.acquire(vmID); err != nil {
		response.WriteError(w, r, http.StatusServiceUnavailable,
			response.CodeIngressUnavailable, "gateway connection capacity reached", nil)
		return
	}
	defer h.slots.release(vmID)

	// Bind the dial to the credential MAC by probing each candidate overlay's veth
	// and using the first whose neighbor table resolves the guest IP to exactly the
	// credential's NIC MAC. This closes IP-reuse (a stale credential whose guest IP
	// now resolves to a different NIC is refused) AND disambiguates overlapping
	// overlay subnets: guest NIC MACs are globally unique across L2 domains, so at
	// most one candidate resolves the IP to the credential MAC. Every binding
	// failure collapses to a uniform refusal so the gateway is no oracle and never
	// dials on uncertain input.
	device, networkID, ok := h.resolveCandidate(candidates, claims.GuestIP, claims.NICMAC)
	if !ok {
		h.log.Warn("connect: refused, no candidate overlay neighbor mac matches credential",
			"guest_ip", claims.GuestIP.String(), "candidates", len(candidates),
			"vm_id", claims.VMID.String())
		h.refuse(w, r)
		return
	}

	// Count this session against its network the instant it holds a slot, and
	// release the count on every exit path (defer), mirroring the slot discipline
	// so the per-network counter can never leak. The count rides the gateway's
	// heartbeat so the CP keeps this gateway's overlay membership sticky while the
	// session is live. An earlier refusal (off-overlay, MAC mismatch) returns
	// before this point and never increments.
	h.overlays.AcquireSession(networkID)
	defer h.overlays.ReleaseSession(networkID)

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
	upstream, err := h.dial(dialCtx, "tcp", target, device)
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

	// Derive the splice context from the request context (not context.Background)
	// so a cancelled request propagates, and bound the session with the idle
	// timeout so a slot pinned by a silent client is reclaimed.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	spliceConns(ctx, cancel, conn, upstream, connectIdleTimeout)
	h.log.Info("connect: session closed", "target", target, "vm_id", vmID)
}

// resolveCandidate probes the neighbor MAC on each candidate overlay veth and
// returns the device + network id of the first whose kernel-resolved neighbor MAC
// for ip equals wantMAC. ok is false when none match (fail closed). The MAC pin
// both closes IP-reuse and disambiguates overlapping overlay subnets: guest NIC
// MACs are globally unique across L2 domains, so at most one overlapping overlay
// resolves ip to wantMAC. In the common no-overlap case there is exactly one
// candidate and this is a single probe.
func (h *ConnectHandler) resolveCandidate(candidates []overlayCandidate, ip netip.Addr, wantMAC string) (device, networkID string, ok bool) {
	for _, cand := range candidates {
		mac, resolved, err := h.fabric.NeighborMAC(cand.Device, ip)
		if err == nil && resolved && macEqual(mac, wantMAC) {
			return cand.Device, cand.NetworkID, true
		}
	}
	return "", "", false
}

// unauthorized writes the uniform 401 the cred gate uses for every credential
// failure, leaking nothing about which check failed.
func (h *ConnectHandler) unauthorized(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusUnauthorized,
		response.CodeUnauthenticated, "a valid ingress session credential is required", nil)
}

// refuse writes the uniform 403 the handler uses for every anti-SSRF binding
// failure (guest IP off-overlay, unresolved neighbor, MAC mismatch), so a holder
// of a valid-but-stale credential learns only that the target is no longer
// authorized, never why.
func (h *ConnectHandler) refuse(w http.ResponseWriter, r *http.Request) {
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

// spliceConns copies bytes both directions until either side closes, ctx is
// cancelled, or the session sits idle in both directions for idle, then tears
// both legs down (the kill-implies-teardown invariant: no goroutine, fd, or slot
// survives any exit path). idle bounds a silent session so a per-VM/per-gateway
// slot cannot be pinned open indefinitely; see connectIdleTimeout.
func spliceConns(ctx context.Context, cancel context.CancelFunc, a, b net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)
	go copyIdle(a, b, idle, done)
	go copyIdle(b, a, idle, done)

	select {
	case <-ctx.Done():
	case <-done:
	}
	cancel()
	_ = a.Close()
	_ = b.Close()
}

// copyIdle copies src to dst until either end errors or the session stays idle
// in both directions for the idle window, then signals done. Before every read
// it re-arms src's read deadline to now+idle, and after every successful write it
// re-arms dst's read deadline (dst is the peer leg's src), so a byte in EITHER
// direction resets both legs' windows and only a session idle in both directions
// (or a closed/broken leg) ends the copy. It signals done exactly once on return;
// the caller's teardown then closes both legs. Mirrors the sibling published-port
// datapath (reconciler.copyIdle); kept local rather than shared because the
// /v1/connect splice plane is load-bearing and reliability beats the DRY saving.
func copyIdle(dst, src net.Conn, idle time.Duration, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(idle))
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			// Activity in this direction must also reset the peer leg's idle
			// window: dst is the peer leg's src, so pushing dst's read deadline
			// forward keeps a session alive whenever bytes flow in EITHER
			// direction. Only a session idle in both directions is torn down.
			_ = dst.SetReadDeadline(time.Now().Add(idle))
		}
		if rerr != nil {
			return
		}
	}
}
