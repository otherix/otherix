// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/auth"
)

// spyOverlays records every AcquireSession/ReleaseSession call so a test can
// assert the connect plane keeps the per-network live-session counter balanced
// across every exit path.
type spyOverlays struct {
	bridge    string
	networkID string
	ok        bool

	mu       sync.Mutex
	acquired []string
	released []string
}

func (s *spyOverlays) OverlayNetworkForIP(netip.Addr) (string, string, bool) {
	return s.bridge, s.networkID, s.ok
}

func (s *spyOverlays) AcquireSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquired = append(s.acquired, id)
}

func (s *spyOverlays) ReleaseSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, id)
}

func (s *spyOverlays) counts() (acquired, released int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.acquired), len(s.released)
}

// TestConnectSessionCounterBalancesOnSplice proves a completed session acquires
// exactly one session against its network and releases it when the splice tears
// down, and that the increment is keyed by the resolved network id.
func TestConnectSessionCounterBalancesOnSplice(t *testing.T) {
	echoAddr, _ := startEcho(t)
	host, portStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	ip := netip.MustParseAddr(host)

	signer, pubPEM := newTestSessionCA(t)
	bridge := "otvb100"
	netID := "33333333-3333-3333-3333-333333333333"
	spy := &spyOverlays{bridge: bridge, networkID: netID, ok: true}
	h := &connectHandler{
		dial:     netDial,
		fabric:   fabricResolving(t, bridge, ip, testMACA),
		overlays: spy,
		caStore:  storeWith(t, pubPEM),
		slots:    newConnectSlots(8, 256),
		log:      discardLogger(),
	}
	srv := gatedConnect(t, h)

	token := signCred(t, signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: ip, Port: port,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	c, br, status := rawConnectCred(t, srv.Listener.Addr().String(), token)
	if !strings.Contains(status, "200") {
		t.Fatalf("connect status = %q, want a 200", strings.TrimSpace(status))
	}
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	// While the session is live the counter has acquired once, not yet released.
	if a, r := spy.counts(); a != 1 || r != 0 {
		t.Fatalf("mid-session counts = (acquired %d, released %d), want (1, 0)", a, r)
	}
	spy.mu.Lock()
	gotID := spy.acquired[0]
	spy.mu.Unlock()
	if gotID != netID {
		t.Fatalf("acquired network id = %q, want %q", gotID, netID)
	}

	// Closing the client tears the splice down; the deferred release must fire.
	_ = c.Close()
	waitBalanced(t, spy, 1)
}

// TestConnectSessionCounterNoLeakOnDialFailure proves a session that acquires a
// slot but then fails to dial the target still releases its session count - the
// release is deferred on every exit path after the slot is taken.
func TestConnectSessionCounterNoLeakOnDialFailure(t *testing.T) {
	signer, pubPEM := newTestSessionCA(t)
	ip := netip.MustParseAddr("10.42.0.9")
	bridge := "otvb100"
	netID := "44444444-4444-4444-4444-444444444444"
	spy := &spyOverlays{bridge: bridge, networkID: netID, ok: true}
	h := &connectHandler{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, io.ErrUnexpectedEOF
		},
		fabric:   fabricResolving(t, bridge, ip, testMACA),
		overlays: spy,
		caStore:  storeWith(t, pubPEM),
		slots:    newConnectSlots(8, 256),
		log:      discardLogger(),
	}
	srv := gatedConnect(t, h)

	token := signCred(t, signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: ip, Port: 22,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	resp := doConnect(t, srv.URL, token)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (dial failed)", resp.StatusCode)
	}
	// The session acquired then released exactly once: no leak.
	waitBalanced(t, spy, 1)
}

// TestConnectSessionCounterNotTakenOnEarlyRefusal proves a guest IP that resolves
// to no declared overlay refuses before the slot is acquired and never increments
// the session counter.
func TestConnectSessionCounterNotTakenOnEarlyRefusal(t *testing.T) {
	signer, pubPEM := newTestSessionCA(t)
	ip := netip.MustParseAddr("10.42.0.5")
	spy := &spyOverlays{ok: false} // guest IP on no declared overlay
	h := &connectHandler{
		dial:     failDial(t),
		fabric:   &netfabric.FakeFabric{},
		overlays: spy,
		caStore:  storeWith(t, pubPEM),
		slots:    newConnectSlots(8, 256),
		log:      discardLogger(),
	}
	srv := gatedConnect(t, h)

	token := signCred(t, signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: ip, Port: 22,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	resp := doConnect(t, srv.URL, token)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (refused)", resp.StatusCode)
	}
	if a, r := spy.counts(); a != 0 || r != 0 {
		t.Fatalf("counts after early refusal = (acquired %d, released %d), want (0, 0)", a, r)
	}
}

// waitBalanced waits until the spy has acquired and released want sessions, so a
// deferred release running on the handler goroutine is observed deterministically.
func waitBalanced(t *testing.T, spy *spyOverlays, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a, r := spy.counts(); a == want && r == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	a, r := spy.counts()
	t.Fatalf("session counts = (acquired %d, released %d), want (%d, %d) balanced", a, r, want, want)
}
