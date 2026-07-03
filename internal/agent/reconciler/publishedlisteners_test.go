// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// fakeListener is a net.Listener whose Accept blocks until Close, then
// returns an error on every subsequent call. It counts Accept invocations
// so a test can distinguish a well-behaved goroutine that returns on the
// first error (acceptCalls stays 1) from a busy-spin loop that keeps
// calling Accept after Close (acceptCalls grows unbounded).
type fakeListener struct {
	port        int32
	acceptCalls atomic.Int32
	closeCalls  atomic.Int32
	entered     chan struct{} // closed on the first Accept entry
	enterOnce   sync.Once
	returned    chan struct{} // closed when Accept first returns its error
	returnOnce  sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
}

func newFakeListener(port int32) *fakeListener {
	return &fakeListener{
		port:     port,
		entered:  make(chan struct{}),
		returned: make(chan struct{}),
		closed:   make(chan struct{}),
	}
}

var errFakeListenerClosed = errors.New("fake listener closed")

func (l *fakeListener) Accept() (net.Conn, error) {
	l.acceptCalls.Add(1)
	l.enterOnce.Do(func() { close(l.entered) })
	<-l.closed
	l.returnOnce.Do(func() { close(l.returned) })
	return nil, errFakeListenerClosed
}

func (l *fakeListener) Close() error {
	l.closeCalls.Add(1)
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{Port: int(l.port)} }

// fakeListenerManager records every Listen call and hands back a
// fakeListener (or a canned error) so a test can assert bind/close
// bookkeeping without touching a real socket.
type fakeListenerManager struct {
	mu        sync.Mutex
	listens   []int32
	listeners map[int32]*fakeListener
	listenErr error
}

func newFakeListenerManager() *fakeListenerManager {
	return &fakeListenerManager{listeners: map[int32]*fakeListener{}}
}

func (m *fakeListenerManager) Listen(_ context.Context, port int32) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listens = append(m.listens, port)
	if m.listenErr != nil {
		return nil, m.listenErr
	}
	l := newFakeListener(port)
	m.listeners[port] = l
	return l, nil
}

func (m *fakeListenerManager) listenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.listens)
}

func (m *fakeListenerManager) listener(port int32) *fakeListener {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listeners[port]
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func lbResponse(ports ...int32) *heartbeat.Response {
	lbs := make([]heartbeat.DeclaredLoadBalancer, 0, len(ports))
	for _, p := range ports {
		lbs = append(lbs, heartbeat.DeclaredLoadBalancer{
			LBID:          uuid.New(),
			PublishedPort: p,
			Protocol:      "tcp",
			BackendPort:   22,
		})
	}
	return &heartbeat.Response{DeclaredLoadBalancers: lbs}
}

func (r *PublishedListeners) boundPorts() []int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int32, 0, len(r.bound))
	for p := range r.bound {
		out = append(out, p)
	}
	return out
}

// TestPublishedListenersBindsThenClosesAndGoroutineExits drives the
// core desired-vs-observed diff through the fake seam: one declared LB
// binds its published port and spawns an accept goroutine; a later
// response without that LB closes the listener and the goroutine exits
// on the first Accept error (no busy-spin).
func TestPublishedListenersBindsThenClosesAndGoroutineExits(t *testing.T) {
	mgr := newFakeListenerManager()
	r := NewPublishedListeners(mgr, testLogger(), time.Hour)
	ctx := context.Background()

	const port int32 = 40000
	r.HandleHeartbeatResponse(ctx, lbResponse(port))
	r.reconcile(ctx)

	if got := mgr.listenCount(); got != 1 {
		t.Fatalf("Listen calls = %d, want 1", got)
	}
	ln := mgr.listener(port)
	if ln == nil {
		t.Fatalf("no fake listener bound on port %d", port)
	}
	if got := r.boundPorts(); len(got) != 1 || got[0] != port {
		t.Fatalf("bound ports = %v, want [%d]", got, port)
	}

	// Wait for the accept goroutine to enter Accept (blocking).
	select {
	case <-ln.entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("accept goroutine never entered Accept")
	}

	// Second response drops the LB: the listener must close and unbind.
	r.HandleHeartbeatResponse(ctx, lbResponse())
	r.reconcile(ctx)

	if got := ln.closeCalls.Load(); got != 1 {
		t.Errorf("Close calls = %d, want 1", got)
	}
	if got := r.boundPorts(); len(got) != 0 {
		t.Errorf("bound ports after unpublish = %v, want []", got)
	}

	// The accept goroutine must return on the first Accept error.
	select {
	case <-ln.returned:
	case <-time.After(2 * time.Second):
		t.Fatalf("accept goroutine did not unblock after Close")
	}
	// Give a busy-spin bug time to reveal itself, then assert the
	// goroutine called Accept exactly once (entered, unblocked, returned).
	time.Sleep(50 * time.Millisecond)
	if got := ln.acceptCalls.Load(); got != 1 {
		t.Errorf("Accept calls = %d, want 1 (busy-spin on closed listener)", got)
	}
}

// TestPublishedListenersNilDesiredBindsNothing proves the pointer-vs-length
// guard: before any heartbeat response the desired pointer is nil and a
// reconcile pass binds nothing (fail toward inaction).
func TestPublishedListenersNilDesiredBindsNothing(t *testing.T) {
	mgr := newFakeListenerManager()
	r := NewPublishedListeners(mgr, testLogger(), time.Hour)

	r.reconcile(context.Background())

	if got := mgr.listenCount(); got != 0 {
		t.Errorf("Listen calls = %d, want 0", got)
	}
	if got := r.boundPorts(); len(got) != 0 {
		t.Errorf("bound ports = %v, want []", got)
	}
}

// TestPublishedListenersRunClosesOnCtxCancel proves Run returns on ctx
// cancel and releases every bound socket on the way out.
func TestPublishedListenersRunClosesOnCtxCancel(t *testing.T) {
	mgr := newFakeListenerManager()
	r := NewPublishedListeners(mgr, testLogger(), 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	const port int32 = 40001
	r.HandleHeartbeatResponse(ctx, lbResponse(port))

	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()

	// Wait until the reconcile pass has bound the listener.
	deadline := time.After(2 * time.Second)
	for mgr.listener(port) == nil {
		select {
		case <-deadline:
			t.Fatalf("listener never bound")
		case <-time.After(5 * time.Millisecond):
		}
	}
	ln := mgr.listener(port)

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}
	if got := ln.closeCalls.Load(); got < 1 {
		t.Errorf("Close calls on shutdown = %d, want >= 1", got)
	}
}

// TestPublishedListenersRealAcceptThenClose exercises the production
// net-backed binder end to end: a dialed connection is accepted and
// immediately closed (client read returns EOF), and after the LB is
// unpublished the port stops accepting.
func TestPublishedListenersRealAcceptThenClose(t *testing.T) {
	port := freeTCPPort(t)
	r := NewPublishedListeners(nil, testLogger(), time.Hour) // nil mgr -> real binder
	ctx := context.Background()

	r.HandleHeartbeatResponse(ctx, lbResponse(port))
	r.reconcile(ctx)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial published port: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	if !errors.Is(rerr, io.EOF) {
		t.Errorf("read on accepted-then-closed conn = %v, want EOF", rerr)
	}

	// Unpublish: the port must stop accepting.
	r.HandleHeartbeatResponse(ctx, lbResponse())
	r.reconcile(ctx)

	c2, derr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if derr == nil {
		c2.Close()
		t.Errorf("dial after unpublish succeeded, want refused")
	}
}

func freeTCPPort(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer l.Close()
	return int32(l.Addr().(*net.TCPAddr).Port)
}
