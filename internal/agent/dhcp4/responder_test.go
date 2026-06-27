// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// fakeConn is a test packetConn: inbound frames arrive on the in channel, every
// sent payload is recorded, and a send notifies sent so a test can synchronise
// on serving without timing races. A frame may carry a transient recv error
// (err != nil) to model a non-fatal socket failure that the serve loop must
// survive rather than treat as a permanent close.
type fakeConn struct {
	bridge string

	in chan fakeFrame

	mu     sync.Mutex
	sends  []sentFrame
	closed bool

	sent   chan struct{}
	closeC chan struct{}
}

type fakeFrame struct {
	payload []byte
	// err, when non-nil, is returned by recv instead of a payload. It models a
	// transient socket error (EINTR, ENOBUFS) that is NOT the close sentinel.
	err error
}

type sentFrame struct {
	payload []byte
	dstMAC  net.HardwareAddr
}

func newFakeConn(bridge string) *fakeConn {
	return &fakeConn{
		bridge: bridge,
		in:     make(chan fakeFrame, 8),
		sent:   make(chan struct{}, 8),
		closeC: make(chan struct{}),
	}
}

func (c *fakeConn) recv() ([]byte, error) {
	select {
	case f := <-c.in:
		if f.err != nil {
			return nil, f.err
		}
		return f.payload, nil
	case <-c.closeC:
		return nil, io.EOF
	}
}

func (c *fakeConn) send(payload []byte, dstMAC net.HardwareAddr) error {
	c.mu.Lock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	c.sends = append(c.sends, sentFrame{payload: cp, dstMAC: dstMAC})
	c.mu.Unlock()
	c.sent <- struct{}{}
	return nil
}

func (c *fakeConn) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.closeC)
	return nil
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConn) recordedSends() []sentFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]sentFrame, len(c.sends))
	copy(out, c.sends)
	return out
}

// feedRecvError pushes a transient recv error through the fake conn. It is not
// the close sentinel, so the serve loop must survive it.
func feedRecvError(c *fakeConn, err error) {
	c.in <- fakeFrame{err: err}
}

// feedDiscover marshals a DISCOVER from mac and pushes it through the fake conn.
func feedDiscover(t *testing.T, c *fakeConn, mac net.HardwareAddr) {
	t.Helper()
	req, err := dhcpv4.New(dhcpv4.WithHwAddr(mac), dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover))
	if err != nil {
		t.Fatalf("dhcpv4.New() = %v", err)
	}
	c.in <- fakeFrame{payload: req.ToBytes()}
}

// waitSend blocks until the conn records a send or the deadline elapses.
func waitSend(t *testing.T, c *fakeConn) {
	t.Helper()
	select {
	case <-c.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a sent frame")
	}
}

func testResponder(t *testing.T) (*responder, map[string]*fakeConn) {
	t.Helper()
	conns := make(map[string]*fakeConn)
	var mu sync.Mutex
	r, err := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	r.newConn = func(bridge string) (packetConn, error) {
		mu.Lock()
		defer mu.Unlock()
		c := newFakeConn(bridge)
		conns[bridge] = c
		return c, nil
	}
	return r, conns
}

func reservation(t *testing.T, mac, ip string) Reservation {
	t.Helper()
	m, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("ParseMAC(%q) = %v", mac, err)
	}
	return Reservation{MAC: m, IP: netip.MustParseAddr(ip)}
}

func runResponder(t *testing.T, r *responder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-r.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	})
}

func TestRegisterNetworkOpensOneSocketPerBridge(t *testing.T) {
	r, conns := testResponder(t)
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	cfg := NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}
	if err := r.RegisterNetwork(cfg); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}
	if got := len(conns); got != 1 {
		t.Fatalf("opened %d sockets, want 1", got)
	}

	// Re-register the same networkID with an added reservation: no new socket,
	// but the new MAC must now be served.
	res2 := reservation(t, "52:54:00:dd:ee:ff", "10.20.0.6")
	cfg.Reservations = []Reservation{res, res2}
	if err := r.RegisterNetwork(cfg); err != nil {
		t.Fatalf("RegisterNetwork() re-register = %v", err)
	}
	if got := len(conns); got != 1 {
		t.Fatalf("after re-register opened %d sockets, want 1", got)
	}

	c := conns["otvb100"]
	feedDiscover(t, c, res2.MAC)
	waitSend(t, c)

	sends := c.recordedSends()
	if len(sends) != 1 {
		t.Fatalf("recorded %d sends, want 1", len(sends))
	}
	reply, err := dhcpv4.FromBytes(sends[0].payload)
	if err != nil {
		t.Fatalf("FromBytes() = %v", err)
	}
	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("MessageType = %v, want OFFER", got)
	}
	if got := reply.YourIPAddr.String(); got != "10.20.0.6" {
		t.Errorf("YourIPAddr = %v, want 10.20.0.6", got)
	}
}

func TestKnownMACDiscoverProducesOffer(t *testing.T) {
	r, conns := testResponder(t)
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}

	c := conns["otvb100"]
	feedDiscover(t, c, res.MAC)
	waitSend(t, c)

	sends := c.recordedSends()
	if len(sends) != 1 {
		t.Fatalf("recorded %d sends, want 1", len(sends))
	}
	if got := sends[0].dstMAC.String(); got != res.MAC.String() {
		t.Errorf("dstMAC = %v, want %v", got, res.MAC)
	}
	reply, err := dhcpv4.FromBytes(sends[0].payload)
	if err != nil {
		t.Fatalf("FromBytes() = %v", err)
	}
	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("MessageType = %v, want OFFER", got)
	}
	if got := reply.YourIPAddr.String(); got != "10.20.0.5" {
		t.Errorf("YourIPAddr = %v, want 10.20.0.5", got)
	}
}

func TestUnknownMACDiscoverProducesNoReply(t *testing.T) {
	r, conns := testResponder(t)
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}

	c := conns["otvb100"]
	unknown, _ := net.ParseMAC("52:54:00:99:99:99")
	feedDiscover(t, c, unknown)

	// Then feed a known MAC and wait for its reply: by the time the known reply
	// is recorded the read-loop has drained the unknown frame ahead of it.
	feedDiscover(t, c, res.MAC)
	waitSend(t, c)

	sends := c.recordedSends()
	if len(sends) != 1 {
		t.Fatalf("recorded %d sends, want 1 (unknown MAC must be dropped)", len(sends))
	}
	reply, err := dhcpv4.FromBytes(sends[0].payload)
	if err != nil {
		t.Fatalf("FromBytes() = %v", err)
	}
	if got := reply.ClientHWAddr.String(); got != res.MAC.String() {
		t.Errorf("reply chaddr = %v, want %v (only known MAC served)", got, res.MAC)
	}
}

func TestDeregisterNetworkClosesSocket(t *testing.T) {
	r, conns := testResponder(t)
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}
	c := conns["otvb100"]

	if err := r.DeregisterNetwork("net-1"); err != nil {
		t.Fatalf("DeregisterNetwork() = %v", err)
	}
	if !c.isClosed() {
		t.Fatal("DeregisterNetwork did not close the socket")
	}

	// Idempotent: deregistering an absent network is a no-op.
	if err := r.DeregisterNetwork("net-1"); err != nil {
		t.Fatalf("DeregisterNetwork() second call = %v", err)
	}
	if err := r.DeregisterNetwork("nonexistent"); err != nil {
		t.Fatalf("DeregisterNetwork(absent) = %v", err)
	}
}

func TestServeSurvivesTransientRecvError(t *testing.T) {
	r, conns := testResponder(t)
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}

	c := conns["otvb100"]
	// A transient recv error must NOT kill the loop. The valid DISCOVER queued
	// behind it must still be served.
	feedRecvError(c, errors.New("transient recv failure"))
	feedDiscover(t, c, res.MAC)
	waitSend(t, c)

	sends := c.recordedSends()
	if len(sends) != 1 {
		t.Fatalf("recorded %d sends, want 1 (loop must survive transient recv error)", len(sends))
	}
	reply, err := dhcpv4.FromBytes(sends[0].payload)
	if err != nil {
		t.Fatalf("FromBytes() = %v", err)
	}
	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("MessageType = %v, want OFFER", got)
	}
	if got := reply.YourIPAddr.String(); got != "10.20.0.5" {
		t.Errorf("YourIPAddr = %v, want 10.20.0.5", got)
	}
}

func TestRegisterBridgeRenameDrainsOldServer(t *testing.T) {
	r, conns := testResponder(t)
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	cfg := NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}
	if err := r.RegisterNetwork(cfg); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}
	old := conns["otvb100"]

	// Re-register the same networkID on a renamed bridge: the old server must be
	// drained (socket closed) and a new socket opened on the new bridge.
	cfg.Bridge = "otvb200"
	if err := r.RegisterNetwork(cfg); err != nil {
		t.Fatalf("RegisterNetwork() rename = %v", err)
	}

	if !old.isClosed() {
		t.Fatal("bridge rename did not close the old socket")
	}
	newC, ok := conns["otvb200"]
	if !ok {
		t.Fatal("bridge rename did not open a socket on the new bridge")
	}

	// The new bridge serves the reservation.
	feedDiscover(t, newC, res.MAC)
	waitSend(t, newC)
	if got := len(newC.recordedSends()); got != 1 {
		t.Fatalf("new bridge recorded %d sends, want 1", got)
	}
	// The old socket is dead and serves nothing.
	if got := len(old.recordedSends()); got != 0 {
		t.Fatalf("old bridge recorded %d sends after rename, want 0", got)
	}
}

// lockedBuffer is a mutex-guarded bytes.Buffer: the serve loop logs from its
// own goroutine while the test reads the buffer, so writes and reads must be
// synchronised.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestServeRecvTimeoutIsBenign proves a receive timeout (errRecvTimeout, what
// the real socket returns when SO_RCVTIMEO elapses with no frame) is treated as
// a benign idle signal: serve must NOT log it as a recv error or trip the
// backoff, and must keep serving. Without the special-case, every ~1s idle tick
// on a quiet bridge would warn-spam and back the loop off. The teeth: the
// asserted absence of "dhcp recv error" fails if serve counts the timeout.
func TestServeRecvTimeoutIsBenign(t *testing.T) {
	buf := &lockedBuffer{}
	conns := make(map[string]*fakeConn)
	var mu sync.Mutex
	r, err := New(Config{Log: slog.New(slog.NewTextHandler(buf, nil))})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	r.newConn = func(bridge string) (packetConn, error) {
		mu.Lock()
		defer mu.Unlock()
		c := newFakeConn(bridge)
		conns[bridge] = c
		return c, nil
	}
	runResponder(t, r)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}

	c := conns["otvb100"]
	// A run of benign receive timeouts must not be counted as recv errors. The
	// DISCOVER queued behind them must still be served promptly.
	for i := 0; i < 5; i++ {
		feedRecvError(c, errRecvTimeout)
	}
	feedDiscover(t, c, res.MAC)
	waitSend(t, c)

	if got := buf.String(); strings.Contains(got, "dhcp recv error") {
		t.Errorf("serve logged a recv error for a benign receive timeout; want none:\n%s", got)
	}
}

// blockingConn models the raw AF_PACKET bug faithfully: recv blocks until the
// test explicitly releases it, and close() does NOT unblock it (closing a raw
// fd cannot wake an in-progress Recvfrom). It is the input that wedged the
// reconciler before the drain bound.
type blockingConn struct {
	release <-chan struct{}
	closed  *atomic.Bool
}

func (c *blockingConn) recv() ([]byte, error) {
	<-c.release
	return nil, io.EOF
}

func (c *blockingConn) send([]byte, net.HardwareAddr) error { return nil }

func (c *blockingConn) close() error {
	c.closed.Store(true)
	return nil
}

// TestStopServerDrainBounded is the reliability teeth: teardown must return even
// when the read-loop is stuck in a recv that close() cannot unblock. The drain
// wait is bounded, so a wedged read-loop can never permanently dark the network
// reconciler (DeregisterNetwork -> stopServer -> reconcile). Without the bound,
// DeregisterNetwork blocks forever and this test times out.
func TestStopServerDrainBounded(t *testing.T) {
	prev := stopServerDrainTimeout
	stopServerDrainTimeout = 100 * time.Millisecond
	defer func() { stopServerDrainTimeout = prev }()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var closed atomic.Bool
	r, err := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	r.newConn = func(string) (packetConn, error) {
		return &blockingConn{release: release, closed: &closed}, nil
	}
	runResponder(t, r)

	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")},
	}); err != nil {
		t.Fatalf("RegisterNetwork() = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- r.DeregisterNetwork("net-1") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeregisterNetwork() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeregisterNetwork did not return: teardown wedged on a recv that close cannot unblock")
	}
	if !closed.Load() {
		t.Error("stopServer did not close the socket before giving up on the drain")
	}
}

func TestRegisterBeforeRunBuffers(t *testing.T) {
	r, conns := testResponder(t)

	res := reservation(t, "52:54:00:aa:bb:cc", "10.20.0.5")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.20.0.0/24"),
		Reservations: []Reservation{res},
	}); err != nil {
		t.Fatalf("RegisterNetwork() before Run = %v", err)
	}
	// Socket must open only once Run starts.
	if got := len(conns); got != 0 {
		t.Fatalf("opened %d sockets before Run, want 0", got)
	}

	runResponder(t, r)
	if got := len(conns); got != 1 {
		t.Fatalf("opened %d sockets after Run, want 1", got)
	}

	c := conns["otvb100"]
	feedDiscover(t, c, res.MAC)
	waitSend(t, c)
	if got := len(c.recordedSends()); got != 1 {
		t.Fatalf("recorded %d sends, want 1", got)
	}
}
