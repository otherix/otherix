// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// Connection-slot caps for the raw published-port datapath. They bound the
// number of concurrent spliced sessions a single gateway carries, per selected
// backend and in total, so one backend cannot exhaust the gateway's file
// descriptors or memory and a gateway has a hard ceiling on fan-out. The per-key
// is the selected backend VMID.
const (
	publishedPerBackendCap = 8
	publishedGatewayCap    = 256
)

// publishedDialTimeout bounds the backend dial. It does not leak into the splice
// (the dial context is cancelled the moment the dial returns) - a live session
// is torn down only by the Run context, a closed leg, or the idle timeout.
const publishedDialTimeout = 10 * time.Second

// publishedIdleTimeout tears down a spliced session that has carried zero bytes
// in either direction for this long, freeing its per-backend slot. It matches
// the AWS-NLB idle-timeout default this feature emulates: a truly idle session
// is reclaimed, but any activity in either direction resets the window, so a
// long-lived but active passthrough (SSH, a database connection, a stream) is
// never torn down. Because this listener is credential-less, reclaiming idle
// slots is what stops an unauthenticated client from pinning a backend's slots
// open indefinitely.
const publishedIdleTimeout = 350 * time.Second

// deviceResolver maps a backend overlay IP to the gateway veth device that
// reaches its overlay, or reports ok=false when this node does not gateway that
// overlay (fail closed). Satisfied by *Networks (OverlayNetworkForIP).
type deviceResolver interface {
	OverlayNetworkForIP(ip netip.Addr) (device, networkID string, ok bool)
}

// neighborResolver returns the kernel-resolved neighbor MAC for a backend IP on
// the given device. Satisfied by netfabric.Fabric (NeighborMAC). The datapath
// pins the dial to the CP-declared MAC through this seam (anti-SSRF).
type neighborResolver interface {
	NeighborMAC(device string, ip netip.Addr) (net.HardwareAddr, bool, error)
}

// datapathDialer dials a backend, binding the socket to the resolved overlay
// device (SO_BINDTODEVICE) so the connection egresses the right bridge even when
// two overlays carry the same guest subnet. The production impl (a net.Dialer
// with netfabric.BindToDeviceControl) is wired by the agent server; tests inject
// a fake yielding a net.Pipe end or an error.
type datapathDialer interface {
	DialOverlay(ctx context.Context, device, addr string) (net.Conn, error)
}

// overlayDialer is the production datapathDialer. It dials addr with the socket
// pinned to device via SO_BINDTODEVICE (netfabric.BindToDeviceControl), so the
// connection egresses the resolved overlay bridge even when two overlays carry
// the same guest subnet. The constructor wires it as the nil-dialer fallback.
type overlayDialer struct{}

func (overlayDialer) DialOverlay(ctx context.Context, device, addr string) (net.Conn, error) {
	d := net.Dialer{Control: netfabric.BindToDeviceControl(device)}
	return d.DialContext(ctx, "tcp", addr)
}

// macEqual reports whether the kernel-resolved neighbor MAC equals the
// CP-declared backend MAC string want. want is parsed so formatting differences
// never cause a false mismatch; a parse failure returns false (fail closed).
// Mirrors the sibling ingress implementation, internal/agent/ingress/splice.go
// (macEqual).
func macEqual(mac net.HardwareAddr, want string) bool {
	pw, err := net.ParseMAC(want)
	if err != nil {
		return false
	}
	return bytes.Equal(mac, pw)
}

// handleConn is the raw published-port per-connection datapath: source-IP ACL,
// backend select, slot acquire, overlay-device resolve, anti-SSRF neighbor-MAC
// pin, SO_BINDTODEVICE dial, and bidirectional splice. Every uncertain step
// closes the accepted connection (fail toward inaction - the client retries and
// re-lands on a healthy backend); the only destructive action is closing a
// connection.
//
// ctx is the reconciler's Run context, threaded as a PARAMETER (never a struct
// field): a bare accept goroutine has no recover, and a nil ctx would panic in
// context.WithTimeout. The slot is acquired BEFORE the neighbor probe: unlike
// the credential-gated /v1/connect path (which probes before acquiring), the raw
// listener has no pre-filter, so an unauthenticated flood would otherwise spawn
// unbounded goroutines each blocked on the ~2s probe. Selecting the backend
// (cheap) yields the per-backend slot key, so acquiring here caps concurrent
// probes at the per-backend/gateway ceiling. Exactly one release runs, via the
// single defer after acquire, on every subsequent path.
func (r *PublishedListeners) handleConn(ctx context.Context, c net.Conn, cfg *listenerConfig) {
	defer func() {
		if p := recover(); p != nil {
			// A bare accept goroutine has no recovery middleware; a panic here
			// would take down the whole agent (and every VM's heartbeat on this
			// node). Fail toward inaction: log and close the accepted socket.
			r.log.Error("published datapath handler panic", "panic", p)
			_ = c.Close()
		}
	}()

	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		_ = c.Close()
		return
	}
	clientIP, err := netip.ParseAddr(host)
	if err != nil {
		_ = c.Close()
		return
	}
	if !sourceIPAllowed(cfg.sourceCIDRs, clientIP) {
		_ = c.Close()
		return
	}

	b, ok := selectBackend(cfg.backends, r.rnd)
	if !ok {
		_ = c.Close()
		return
	}

	key := b.VMID.String()
	if !r.slots.acquire(key) {
		_ = c.Close()
		return
	}
	defer r.slots.release(key)

	ip, err := netip.ParseAddr(b.OverlayIP)
	if err != nil {
		_ = c.Close()
		return
	}
	device, _, ok := r.devices.OverlayNetworkForIP(ip)
	if !ok {
		_ = c.Close()
		return
	}

	mac, ok, err := r.neighbors.NeighborMAC(device, ip)
	if err != nil || !ok || !macEqual(mac, b.MAC) {
		_ = c.Close()
		return
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, publishedDialTimeout)
	up, err := r.dialer.DialOverlay(dialCtx, device, net.JoinHostPort(b.OverlayIP, strconv.Itoa(int(cfg.backendPort))))
	dialCancel()
	if err != nil {
		_ = c.Close()
		return
	}

	spliceCtx, spliceCancel := context.WithCancel(ctx)
	spliceConns(spliceCtx, spliceCancel, c, up, r.idleTimeout)
}

// selectBackend picks a uniformly random backend from the CP-pushed set and
// returns it with true, or the zero value and false when the set is empty.
// rnd(n) must return a value in [0,n); production callers pass math/rand/v2's
// IntN, tests pass a deterministic stub.
//
// The set is selected in full: the CP already applied backend eligibility
// (fail toward inclusion) before pushing it, so DeclaredBackend.Healthy is
// informational and must not be re-filtered here - doing so would wrongly
// darken a warming-but-eligible backend.
func selectBackend(backends []heartbeat.DeclaredBackend, rnd func(int) int) (heartbeat.DeclaredBackend, bool) {
	if len(backends) == 0 {
		return heartbeat.DeclaredBackend{}, false
	}
	return backends[rnd(len(backends))], true
}

// spliceConns copies bytes both directions until either side closes, ctx is
// cancelled, or a direction sits idle for idle, then tears both legs down (the
// kill-implies-teardown invariant: no goroutine, fd, or slot survives any exit
// path). All copy and close errors are discarded - the only outcome that matters
// is that both connections end closed.
//
// This is a deliberate reimplementation of the sibling ingress splicer,
// internal/agent/ingress/splice.go (spliceConns), kept local to this datapath
// rather than shared: the /v1/connect path is load-bearing and reliability beats
// the trivial DRY saving. It INTENTIONALLY DIVERGES from that sibling by adding
// the idle timeout: unlike the credential-gated /v1/connect path, this listener
// is credential-less, so a torn-down idle session is the only thing that
// reclaims a per-backend slot an unauthenticated client would otherwise pin open
// indefinitely. The ingress sibling is deliberately left unchanged.
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

// copyIdle copies src to dst until either end errors or src stays idle for the
// idle window, then signals done. Before every read it re-arms src's read
// deadline to now+idle, so any byte received resets the window and only a truly
// idle direction (or a closed/broken leg) ends the copy. It signals done exactly
// once on return; the caller's teardown then closes both legs.
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
		}
		if rerr != nil {
			return
		}
	}
}

// slotLimiter enforces the per-backend and per-gateway concurrency caps on the
// raw published-port splice plane, mirroring the sibling ingress accountant,
// internal/agent/ingress/splice.go (connectSlots).
type slotLimiter struct {
	mu        sync.Mutex
	perKey    map[string]int
	total     int
	perKeyCap int
	totalCap  int
}

// newSlotLimiter builds a slot accountant with the given per-key and total caps.
func newSlotLimiter(perKeyCap, totalCap int) *slotLimiter {
	return &slotLimiter{
		perKey:    map[string]int{},
		perKeyCap: perKeyCap,
		totalCap:  totalCap,
	}
}

// acquire reserves a slot for key, enforcing the per-key and total caps. It
// returns true on success, or false (reserving nothing) when either cap is
// already reached.
func (s *slotLimiter) acquire(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= s.totalCap || s.perKey[key] >= s.perKeyCap {
		return false
	}
	s.perKey[key]++
	s.total++
	return true
}

// release returns a slot previously taken by acquire, deleting the map entry
// when its count falls to zero.
func (s *slotLimiter) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perKey[key] > 0 {
		s.perKey[key]--
		if s.perKey[key] == 0 {
			delete(s.perKey, key)
		}
		s.total--
	}
}
