// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

func TestSelectBackendEmpty(t *testing.T) {
	got, ok := selectBackend(nil, func(int) int { t.Fatalf("rnd must not be called on an empty set"); return 0 })
	if ok {
		t.Errorf("selectBackend(nil) ok = true, want false")
	}
	if got != (heartbeat.DeclaredBackend{}) {
		t.Errorf("selectBackend(nil) backend = %+v, want zero value", got)
	}
}

func TestSelectBackendPicksIndex(t *testing.T) {
	backends := []heartbeat.DeclaredBackend{
		{OverlayIP: "10.0.0.1"},
		{OverlayIP: "10.0.0.2"},
		{OverlayIP: "10.0.0.3"},
	}
	got, ok := selectBackend(backends, func(int) int { return 1 })
	if !ok {
		t.Fatalf("selectBackend(3 backends) ok = false, want true")
	}
	if got != backends[1] {
		t.Errorf("selectBackend(3 backends, rnd->1) = %+v, want %+v", got, backends[1])
	}
}

func TestSelectBackendBalances(t *testing.T) {
	backends := []heartbeat.DeclaredBackend{
		{VMID: uuid.New(), OverlayIP: "10.0.0.1"},
		{VMID: uuid.New(), OverlayIP: "10.0.0.2"},
		{VMID: uuid.New(), OverlayIP: "10.0.0.3"},
	}
	seen := make(map[uuid.UUID]bool, len(backends))
	for range 1000 {
		got, ok := selectBackend(backends, rand.IntN)
		if !ok {
			t.Fatalf("selectBackend ok = false, want true")
		}
		seen[got.VMID] = true
	}
	for _, b := range backends {
		if !seen[b.VMID] {
			t.Errorf("backend %v never selected over 1000 calls", b.VMID)
		}
	}
}

func TestSpliceConnsCopiesBothDirections(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	// spliceConns bridges a2<->b1; a1 and b2 are the external endpoints.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go spliceConns(ctx, cancel, a2, b1, time.Minute)

	// a1 -> a2 -> b1 -> b2
	go func() { _, _ = a1.Write([]byte("ping")) }()
	if got := readN(t, b2, 4); got != "ping" {
		t.Errorf("a1->b2 = %q, want %q", got, "ping")
	}

	// b2 -> b1 -> a2 -> a1
	go func() { _, _ = b2.Write([]byte("pong")) }()
	if got := readN(t, a1, 4); got != "pong" {
		t.Errorf("b2->a1 = %q, want %q", got, "pong")
	}
}

func TestSpliceConnsCloseTearsDownBoth(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go spliceConns(ctx, cancel, a2, b1, time.Minute)

	// Closing one external endpoint makes the copy from it return, which must
	// tear down both spliced legs so a read on the other external endpoint errors.
	_ = a1.Close()

	_ = b2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b2.Read(make([]byte, 1)); err == nil {
		t.Errorf("read on the far endpoint after close = nil error, want an error (both legs torn down)")
	}
}

func TestSpliceConnsIdleTimeoutClosesBoth(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go spliceConns(ctx, cancel, a2, b1, 100*time.Millisecond)

	// No bytes flow in either direction. After the idle window elapses the
	// idle-aware copy tears both legs down, so a read on each external endpoint
	// unblocks (with an error). Without an idle timeout these reads block until
	// ctx cancel, which the bounded wait catches as a failure.
	if !readUnblocks(a1, time.Second) {
		t.Error("a1 read still blocked after idle timeout, want the leg torn down")
	}
	if !readUnblocks(b2, time.Second) {
		t.Error("b2 read still blocked after idle timeout, want the leg torn down")
	}
}

func TestSpliceConnsActiveSessionKeepsOpen(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := 200 * time.Millisecond
	go spliceConns(ctx, cancel, a2, b1, idle)

	// Send a byte each direction at an interval shorter than idle, several
	// rounds spanning well past a single idle window. Every byte re-arms the
	// read deadline, so an active session must never be torn down and bytes must
	// keep flowing.
	for i := range 4 {
		go func() { _, _ = a1.Write([]byte("x")) }()
		if got := readN(t, b2, 1); got != "x" {
			t.Fatalf("round %d a1->b2 = %q, want x (active session must stay open)", i, got)
		}
		go func() { _, _ = b2.Write([]byte("y")) }()
		if got := readN(t, a1, 1); got != "y" {
			t.Fatalf("round %d b2->a1 = %q, want y (active session must stay open)", i, got)
		}
		time.Sleep(idle / 2)
	}
}

// readUnblocks issues a blocking one-byte Read on c in a goroutine and reports
// whether it returned within d. A false result means the read never unblocked
// (the connection was neither closed nor fed any byte in the window).
func readUnblocks(c net.Conn, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_, _ = c.Read(make([]byte, 1))
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// readN reads exactly n bytes from c under a short deadline and returns them as
// a string, failing the test on any read error.
func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("readN(%d): %v", n, err)
	}
	return string(buf)
}

func TestSlotLimiterPerKeyCap(t *testing.T) {
	s := newSlotLimiter(2, 10)
	for i := range 2 {
		if !s.acquire("a") {
			t.Fatalf("acquire(\"a\") #%d = false, want true", i+1)
		}
	}
	if s.acquire("a") {
		t.Errorf("third acquire(\"a\") = true, want false (per-key cap reached)")
	}
	// A different key is unaffected by the first key's cap.
	if !s.acquire("b") {
		t.Errorf("acquire(\"b\") = false, want true (independent per-key count)")
	}
}

func TestSlotLimiterTotalCap(t *testing.T) {
	s := newSlotLimiter(10, 2)
	for _, k := range []string{"a", "b"} {
		if !s.acquire(k) {
			t.Fatalf("acquire(%q) under totalCap=2 = false, want true", k)
		}
	}
	if s.acquire("c") {
		t.Errorf("acquire(\"c\") at totalCap = true, want false")
	}
}

func TestSlotLimiterDefaultCaps(t *testing.T) {
	// The raw published-port datapath constructs its accountant with these
	// package caps; keep them wired so the datapath's concurrency ceiling is a
	// single source of truth.
	s := newSlotLimiter(publishedPerBackendCap, publishedGatewayCap)
	for i := range publishedPerBackendCap {
		if !s.acquire("a") {
			t.Fatalf("acquire(\"a\") #%d under publishedPerBackendCap=%d = false, want true", i+1, publishedPerBackendCap)
		}
	}
	if s.acquire("a") {
		t.Errorf("acquire(\"a\") past publishedPerBackendCap=%d = true, want false", publishedPerBackendCap)
	}
}

func TestSlotLimiterReleaseFreesSlot(t *testing.T) {
	s := newSlotLimiter(1, 1)
	if !s.acquire("a") {
		t.Fatalf("first acquire(\"a\") = false, want true")
	}
	if s.acquire("a") {
		t.Fatalf("second acquire(\"a\") = true, want false")
	}
	s.release("a")
	if !s.acquire("a") {
		t.Errorf("acquire(\"a\") after release = false, want true (slot freed)")
	}
}

// stubAddr is a net.Addr with a caller-supplied string form so a net.Pipe end
// can present a routable ip:port RemoteAddr to handleConn's client-IP parse.
type stubAddr string

func (stubAddr) Network() string  { return "tcp" }
func (a stubAddr) String() string { return string(a) }

// addrConn wraps a net.Conn to override RemoteAddr and record closure. It lets
// a test drive handleConn with a scripted client address and observe that the
// datapath closed the accepted connection (fail toward inaction).
type addrConn struct {
	net.Conn
	remote    net.Addr
	closeOnce sync.Once
	closed    chan struct{}
}

func newAddrConn(remote string) (*addrConn, net.Conn) {
	c, peer := net.Pipe()
	return &addrConn{Conn: c, remote: stubAddr(remote), closed: make(chan struct{})}, peer
}

func (a *addrConn) RemoteAddr() net.Addr { return a.remote }

func (a *addrConn) Close() error {
	a.closeOnce.Do(func() { close(a.closed) })
	return a.Conn.Close()
}

// isClosed reports whether Close was called within d.
func (a *addrConn) isClosed(d time.Duration) bool {
	select {
	case <-a.closed:
		return true
	case <-time.After(d):
		return false
	}
}

// fakeDeviceResolver is a scripted deviceResolver seam.
type fakeDeviceResolver struct {
	device string
	netID  string
	ok     bool
}

func (f *fakeDeviceResolver) OverlayNetworkForIP(netip.Addr) (string, string, bool) {
	return f.device, f.netID, f.ok
}

// fakeNeighborResolver is a scripted neighborResolver seam that counts calls so
// a test can prove the slot cap gates the probe (acquire before probe).
type fakeNeighborResolver struct {
	mu    sync.Mutex
	calls int
	mac   net.HardwareAddr
	ok    bool
	err   error
}

func (f *fakeNeighborResolver) NeighborMAC(string, netip.Addr) (net.HardwareAddr, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.mac, f.ok, f.err
}

func (f *fakeNeighborResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeDatapathDialer is a scripted datapathDialer seam. fn yields the upstream
// conn (or an error) per call; the dialer records the resolved device+addr and
// its call count so a test can assert a dial happened (or never did).
type fakeDatapathDialer struct {
	mu         sync.Mutex
	calls      int
	lastDevice string
	lastAddr   string
	fn         func() (net.Conn, error)
}

func (f *fakeDatapathDialer) DialOverlay(_ context.Context, device, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.calls++
	f.lastDevice = device
	f.lastAddr = addr
	fn := f.fn
	f.mu.Unlock()
	return fn()
}

func (f *fakeDatapathDialer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const testBackendMAC = "02:00:00:00:00:01"

// datapathReconciler builds a PublishedListeners with all datapath seams
// injected as the given fakes and deterministic single-backend selection.
func datapathReconciler(dev *fakeDeviceResolver, nbr *fakeNeighborResolver, dialer *fakeDatapathDialer) *PublishedListeners {
	return &PublishedListeners{
		log:         testLogger(),
		slots:       newSlotLimiter(publishedPerBackendCap, publishedGatewayCap),
		rnd:         func(int) int { return 0 },
		devices:     dev,
		neighbors:   nbr,
		dialer:      dialer,
		idleTimeout: publishedIdleTimeout,
	}
}

func mustParseMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("parse mac %q: %v", s, err)
	}
	return m
}

// oneBackendCfg is a listenerConfig with a single eligible backend and an
// optional source allowlist.
func oneBackendCfg(sourceCIDRs []string) *listenerConfig {
	return &listenerConfig{
		backendPort: 8080,
		sourceCIDRs: sourceCIDRs,
		backends: []heartbeat.DeclaredBackend{
			{VMID: uuid.New(), OverlayIP: "10.0.0.5", MAC: testBackendMAC, Healthy: true},
		},
	}
}

// okSeams returns seams that would let a connection reach the dial step: device
// resolvable, neighbor MAC matching the backend.
func okSeams(t *testing.T) (*fakeDeviceResolver, *fakeNeighborResolver) {
	return &fakeDeviceResolver{device: "otvg1", netID: "net-1", ok: true},
		&fakeNeighborResolver{mac: mustParseMAC(t, testBackendMAC), ok: true}
}

func TestHandleConnACLReject(t *testing.T) {
	dev, nbr := okSeams(t)
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return nil, nil }}
	r := datapathReconciler(dev, nbr, dialer)

	c, _ := newAddrConn("198.51.100.7:40000")
	cfg := oneBackendCfg([]string{"192.0.2.0/24"}) // client not in allowlist

	r.handleConn(context.Background(), c, cfg)

	if !c.isClosed(time.Second) {
		t.Error("ACL-rejected conn not closed")
	}
	if got := dialer.callCount(); got != 0 {
		t.Errorf("dialer calls on ACL reject = %d, want 0", got)
	}
}

func TestHandleConnNoBackend(t *testing.T) {
	dev, nbr := okSeams(t)
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return nil, nil }}
	r := datapathReconciler(dev, nbr, dialer)

	c, _ := newAddrConn("198.51.100.7:40000")
	cfg := &listenerConfig{backendPort: 8080} // no backends

	r.handleConn(context.Background(), c, cfg)

	if !c.isClosed(time.Second) {
		t.Error("no-backend conn not closed")
	}
	if got := dialer.callCount(); got != 0 {
		t.Errorf("dialer calls with no backend = %d, want 0", got)
	}
	if got := nbr.callCount(); got != 0 {
		t.Errorf("neighbor probe with no backend = %d, want 0", got)
	}
}

func TestHandleConnDeviceUnresolvable(t *testing.T) {
	dev := &fakeDeviceResolver{ok: false} // gateway not on the backend overlay
	_, nbr := okSeams(t)
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return nil, nil }}
	r := datapathReconciler(dev, nbr, dialer)

	c, _ := newAddrConn("198.51.100.7:40000")
	r.handleConn(context.Background(), c, oneBackendCfg(nil))

	if !c.isClosed(time.Second) {
		t.Error("device-unresolvable conn not closed")
	}
	if got := dialer.callCount(); got != 0 {
		t.Errorf("dialer calls on unresolvable device = %d, want 0", got)
	}
	if got := nbr.callCount(); got != 0 {
		t.Errorf("neighbor probe on unresolvable device = %d, want 0", got)
	}
}

func TestHandleConnNeighborMismatch(t *testing.T) {
	dev := &fakeDeviceResolver{device: "otvg1", netID: "net-1", ok: true}
	nbr := &fakeNeighborResolver{mac: mustParseMAC(t, "02:00:00:00:00:99"), ok: true} // different MAC
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return nil, nil }}
	r := datapathReconciler(dev, nbr, dialer)

	c, _ := newAddrConn("198.51.100.7:40000")
	r.handleConn(context.Background(), c, oneBackendCfg(nil))

	if !c.isClosed(time.Second) {
		t.Error("neighbor-mismatch conn not closed")
	}
	if got := dialer.callCount(); got != 0 {
		t.Errorf("dialer calls on neighbor mismatch = %d, want 0", got)
	}
}

func TestHandleConnHappyPathSplice(t *testing.T) {
	dev, nbr := okSeams(t)
	upstream, testUpstream := net.Pipe()
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return upstream, nil }}
	r := datapathReconciler(dev, nbr, dialer)

	c, testClient := newAddrConn("192.0.2.10:50000")
	cfg := oneBackendCfg([]string{"192.0.2.0/24"}) // client allowed

	go r.handleConn(context.Background(), c, cfg)

	// Wait for the dial, then assert it targeted the resolved device + addr.
	deadline := time.Now().Add(2 * time.Second)
	for dialer.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dial never happened on happy path")
		}
		time.Sleep(time.Millisecond)
	}
	if dialer.lastDevice != "otvg1" {
		t.Errorf("dial device = %q, want otvg1", dialer.lastDevice)
	}
	if dialer.lastAddr != "10.0.0.5:8080" {
		t.Errorf("dial addr = %q, want 10.0.0.5:8080", dialer.lastAddr)
	}

	// Client -> upstream.
	go func() { _, _ = testClient.Write([]byte("hello")) }()
	if got := readN(t, testUpstream, 5); got != "hello" {
		t.Errorf("upstream got %q, want hello", got)
	}

	// Upstream -> client.
	go func() { _, _ = testUpstream.Write([]byte("world")) }()
	if got := readN(t, testClient, 5); got != "world" {
		t.Errorf("client got %q, want world", got)
	}

	// Closing one side tears down both.
	_ = testClient.Close()
	_ = testUpstream.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := testUpstream.Read(make([]byte, 1)); err == nil {
		t.Error("upstream still open after client close, want torn down")
	}
}

func TestHandleConnSlotCap(t *testing.T) {
	dev, nbr := okSeams(t)
	var mu sync.Mutex
	var peers []net.Conn
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) {
		up, peer := net.Pipe()
		mu.Lock()
		peers = append(peers, peer) // keep the peer end open so the splice blocks
		mu.Unlock()
		return up, nil
	}}
	r := datapathReconciler(dev, nbr, dialer)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range peers {
			_ = p.Close()
		}
	})

	// One shared backend so every conn keys the same per-backend slot.
	shared := heartbeat.DeclaredBackend{VMID: uuid.New(), OverlayIP: "10.0.0.5", MAC: testBackendMAC, Healthy: true}
	cfg := &listenerConfig{backendPort: 8080, backends: []heartbeat.DeclaredBackend{shared}}

	// Fill the per-backend cap with live (blocked-in-splice) connections.
	for range publishedPerBackendCap {
		c, _ := newAddrConn("192.0.2.10:50000")
		go r.handleConn(context.Background(), c, cfg)
	}
	deadline := time.Now().Add(2 * time.Second)
	for dialer.callCount() < publishedPerBackendCap {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d slots filled", dialer.callCount(), publishedPerBackendCap)
		}
		time.Sleep(time.Millisecond)
	}

	// The (cap+1)th conn must be closed without a dial.
	c, _ := newAddrConn("192.0.2.10:50000")
	r.handleConn(context.Background(), c, cfg)
	if !c.isClosed(time.Second) {
		t.Error("over-cap conn not closed")
	}
	if got := dialer.callCount(); got != publishedPerBackendCap {
		t.Errorf("dialer calls after over-cap conn = %d, want %d", got, publishedPerBackendCap)
	}
}

// TestHandleConnAcquireBeforeProbe proves the slot cap gates the expensive
// neighbor probe, not just the dial: with the backend's slots exhausted, the
// neighbor resolver is never consulted.
func TestHandleConnAcquireBeforeProbe(t *testing.T) {
	dev, nbr := okSeams(t)
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return nil, nil }}
	r := datapathReconciler(dev, nbr, dialer)

	backend := heartbeat.DeclaredBackend{VMID: uuid.New(), OverlayIP: "10.0.0.5", MAC: testBackendMAC, Healthy: true}
	cfg := &listenerConfig{backendPort: 8080, backends: []heartbeat.DeclaredBackend{backend}}

	// Exhaust the per-backend cap directly (as if that many sessions were live).
	key := backend.VMID.String()
	for i := range publishedPerBackendCap {
		if !r.slots.acquire(key) {
			t.Fatalf("pre-fill acquire %d failed", i)
		}
	}

	c, _ := newAddrConn("192.0.2.10:50000")
	r.handleConn(context.Background(), c, cfg)

	if !c.isClosed(time.Second) {
		t.Error("slot-exhausted conn not closed")
	}
	if got := nbr.callCount(); got != 0 {
		t.Errorf("neighbor probed while slot-exhausted = %d, want 0 (acquire must gate the probe)", got)
	}
	if got := dialer.callCount(); got != 0 {
		t.Errorf("dial while slot-exhausted = %d, want 0", got)
	}
}

// TestHandleConnExactlyOnceRelease drives a cap's worth of dial FAILURES to one
// backend and then a fresh connection: the failures must each release exactly
// their own slot, so the fresh connection still acquires and dials. A missing
// release would wedge the key at cap; the fresh dial would never happen.
func TestHandleConnExactlyOnceRelease(t *testing.T) {
	dev, nbr := okSeams(t)
	var callN int
	var mu sync.Mutex
	upstream, testUpstream := net.Pipe()
	t.Cleanup(func() { _ = testUpstream.Close() })
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		callN++
		if callN <= publishedPerBackendCap {
			return nil, errFakeDial
		}
		return upstream, nil
	}}
	r := datapathReconciler(dev, nbr, dialer)

	backend := heartbeat.DeclaredBackend{VMID: uuid.New(), OverlayIP: "10.0.0.5", MAC: testBackendMAC, Healthy: true}
	cfg := &listenerConfig{backendPort: 8080, backends: []heartbeat.DeclaredBackend{backend}}

	// N sequential dial failures (each returns immediately, no splice).
	for range publishedPerBackendCap {
		c, _ := newAddrConn("192.0.2.10:50000")
		r.handleConn(context.Background(), c, cfg)
	}

	// A fresh connection must still acquire a slot and dial.
	c, _ := newAddrConn("192.0.2.10:50000")
	go r.handleConn(context.Background(), c, cfg)
	deadline := time.Now().Add(2 * time.Second)
	for dialer.callCount() <= publishedPerBackendCap {
		if time.Now().After(deadline) {
			t.Fatal("fresh conn never dialed; failed dials did not release their slots")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHandleConnNilCtxSafety confirms handleConn takes ctx as a parameter and a
// real context.Background() drives the dial step (context.WithTimeout) without a
// panic - a nil struct-field ctx would panic in the bare accept goroutine.
func TestHandleConnNilCtxSafety(t *testing.T) {
	dev, nbr := okSeams(t)
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return nil, errFakeDial }}
	r := datapathReconciler(dev, nbr, dialer)

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("handleConn panicked: %v", p)
		}
	}()
	c, _ := newAddrConn("192.0.2.10:50000")
	r.handleConn(context.Background(), c, oneBackendCfg(nil))
	if got := dialer.callCount(); got != 1 {
		t.Errorf("dialer calls = %d, want 1 (dial step must be reached)", got)
	}
}

// TestHandleConnIdleReclaimsSlot drives a full happy-path splice with a short
// idle window and sends no bytes: the idle timeout must tear the splice down,
// handleConn must return, and its deferred release must free the per-backend
// slot. This is the end-to-end proof that an idle credential-less connection
// cannot pin a backend's slots open indefinitely.
func TestHandleConnIdleReclaimsSlot(t *testing.T) {
	dev, nbr := okSeams(t)
	upstream, testUpstream := net.Pipe()
	t.Cleanup(func() { _ = testUpstream.Close() })
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { return upstream, nil }}
	r := datapathReconciler(dev, nbr, dialer)
	r.idleTimeout = 100 * time.Millisecond

	c, _ := newAddrConn("192.0.2.10:50000")
	cfg := oneBackendCfg([]string{"192.0.2.0/24"})

	done := make(chan struct{})
	go func() { r.handleConn(context.Background(), c, cfg); close(done) }()

	// Wait for the dial so the slot is known-held.
	deadline := time.Now().Add(2 * time.Second)
	for dialer.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dial never happened on happy path")
		}
		time.Sleep(time.Millisecond)
	}
	if got := slotTotal(r.slots); got != 1 {
		t.Fatalf("slots after dial = %d, want 1 (slot held during splice)", got)
	}

	// No bytes flow: the idle timeout must reclaim the slot.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return after idle timeout; the idle splice never tore down")
	}
	if got := slotTotal(r.slots); got != 0 {
		t.Errorf("slots after idle teardown = %d, want 0 (release must free the slot)", got)
	}
}

// TestHandleConnPanicRecovered injects a panic through the dial seam and proves
// it does not escape handleConn (the bare accept goroutine has no recovery
// middleware), the accepted conn is closed, and the acquired slot is released.
func TestHandleConnPanicRecovered(t *testing.T) {
	dev, nbr := okSeams(t)
	dialer := &fakeDatapathDialer{fn: func() (net.Conn, error) { panic("boom in datapath") }}
	r := datapathReconciler(dev, nbr, dialer)

	c, _ := newAddrConn("192.0.2.10:50000")
	// A missing recover would let this panic escape the goroutine and crash the
	// whole agent process.
	r.handleConn(context.Background(), c, oneBackendCfg(nil))

	if !c.isClosed(time.Second) {
		t.Error("panicked conn not closed")
	}
	if got := slotTotal(r.slots); got != 0 {
		t.Errorf("slots after panic = %d, want 0 (release must still run on unwind)", got)
	}
}

// slotTotal reads the accountant's live total under its lock (race-safe).
func slotTotal(s *slotLimiter) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

type fakeDialErr struct{}

func (fakeDialErr) Error() string { return "fake dial failure" }

var errFakeDial = fakeDialErr{}
