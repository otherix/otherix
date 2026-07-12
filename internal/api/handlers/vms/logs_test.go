// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// logsClientStub satisfies the handler's console-client dependency. Only
// HTTPClient is exercised by the logs path; its transport dials the httptest
// TLS agents through the identity-SAN dial map (the CP now dials the node's
// identity SAN, not the raw advertised host).
type logsClientStub struct {
	dialMap map[string]string
}

func (logsClientStub) IssueConsoleToken(context.Context, string, string, string) (agentclient.IssueConsoleTokenResponse, error) {
	return agentclient.IssueConsoleTokenResponse{}, nil
}

func (s logsClientStub) HTTPClient() *http.Client {
	return &http.Client{Transport: identityDialTransport(s.dialMap)}
}

// logsStoreStub satisfies the handler's Store interface for the follow
// tests. It embeds Store (nil) so only the methods the reconnect loop
// touches have bodies; any other call panics. VMByName returns a VM
// whose PinnedNodeID advances through pinSequence (one entry consumed per
// call, last entry repeats), letting a test simulate the cutover flip.
type logsStoreStub struct {
	Store

	mu          sync.Mutex
	vm          store.VM
	pinSequence []uuid.UUID // PinnedNodeID returned by successive VMByName calls
	vmCalls     int
	nodes       map[uuid.UUID]store.Node
	migrating   bool
}

func (s *logsStoreStub) VMByName(context.Context, string) (store.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.vmCalls
	if idx >= len(s.pinSequence) {
		idx = len(s.pinSequence) - 1
	}
	pin := s.pinSequence[idx]
	s.vmCalls++
	v := s.vm
	v.PinnedNodeID = &pin
	return v, nil
}

func (s *logsStoreStub) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

func (s *logsStoreStub) ActiveMigrationForVM(context.Context, uuid.UUID) (store.Migration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.migrating {
		return store.Migration{}, false, nil
	}
	return store.Migration{}, true, nil
}

// logsAgent stands up an httptest TLS server that emits the given line
// (already newline-terminated) on every /logs request, then returns
// (clean EOF). It records how many times it was dialed and the inbound
// Host of the last request (to prove the CP dialed the identity SAN).
type logsAgent struct {
	srv     *httptest.Server
	calls   int
	reqHost string
	mu      sync.Mutex
}

func newLogsAgent(t *testing.T, line string) *logsAgent {
	t.Helper()
	a := &logsAgent{}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.calls++
		a.reqHost = r.Host
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, line)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *logsAgent) dialCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// host returns the agent's loopback listener addr (the real dial target the
// identity SAN maps to).
func (a *logsAgent) host() string { return a.srv.Listener.Addr().String() }

// lastHost returns the inbound Host of the last request the agent served.
func (a *logsAgent) lastHost() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reqHost
}

func logsFollowHandler(s Store, dialMap map[string]string) *Handler {
	return New(s,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{},
		ConsoleDeps{AgentClient: logsClientStub{dialMap: dialMap}, AccessMode: "proxy"},
		SSHDeps{})
}

// logsDialMap maps each node's identity SAN (the host the CP now dials
// through the geo transport) to the loopback addr of its httptest agent, so
// the stub transport reaches the real server after the handler swaps the raw
// endpoint for the identity SAN.
func logsDialMap(nodes map[uuid.UUID]store.Node) map[string]string {
	m := make(map[string]string, len(nodes))
	for _, n := range nodes {
		host, err := stripScheme(n.AdvertisedEndpoint)
		if err != nil {
			continue
		}
		m[auth.NodeIdentitySAN(n.Name)] = host
	}
	return m
}

// followRequest builds a GET /logs?follow=true request carrying a cancel
// context so the test can stop a stuck stream.
func followRequest(t *testing.T, vmName string) (*http.Request, context.CancelFunc) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/vms/"+vmName+"/logs?follow=true", nil)
	ctx, cancel := context.WithCancel(req.Context())
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	return req.WithContext(ctx), cancel
}

func logsTestVM(t *testing.T, owner, pin uuid.UUID) store.VM {
	t.Helper()
	p := pin
	return store.VM{
		ID: uuid.New(), OwnerID: owner, Name: "logs-" + uuid.NewString()[:8],
		PinnedNodeID: &p,
	}
}

// TestRelayLogsFollowing_ReattachesOnCutover is the main seam test: node A
// streams then ends, the store reports the PinnedNodeID flipped to node B,
// and the loop must re-dial B and deliver B's bytes on the SAME client
// response after A's bytes. Revert-to-confirm: replacing relayLogsFollowing
// with a single-shot relay drops everything after A's bytes.
func TestRelayLogsFollowing_ReattachesOnCutover(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	nodeA, nodeB := uuid.New(), uuid.New()
	agentA := newLogsAgent(t, "A-line\n")
	agentB := newLogsAgent(t, "B-line\n")

	stub := &logsStoreStub{
		vm: logsTestVM(t, owner, nodeA),
		// The initial resolve uses the passed-in vm (pinned to A). This test
		// drives relayLogsFollowing directly, so VMByName is only called AFTER
		// the break - it returns the flipped owner (B). The last entry repeats,
		// so the post-reattach poll also sees B and the stream then ends.
		pinSequence: []uuid.UUID{nodeB},
		nodes: map[uuid.UUID]store.Node{
			nodeA: {ID: nodeA, Name: "a", AdvertisedEndpoint: agentA.srv.URL},
			nodeB: {ID: nodeB, Name: "b", AdvertisedEndpoint: agentB.srv.URL},
		},
	}
	h := logsFollowHandler(stub, logsDialMap(stub.nodes))

	rec := httptest.NewRecorder()
	req, cancel := followRequest(t, stub.vm.Name)
	defer cancel()
	h.relayLogsFollowing(rec, req, stub.vm)

	got := rec.Body.String()
	if !strings.Contains(got, "A-line") || !strings.Contains(got, "B-line") {
		t.Errorf("body = %q, want both A-line and B-line", got)
	}
	if i, j := strings.Index(got, "A-line"), strings.Index(got, "B-line"); i > j {
		t.Errorf("ordering wrong: A at %d, B at %d in %q", i, j, got)
	}
	if agentB.dialCount() == 0 {
		t.Errorf("node B never dialed; loop did not reattach")
	}
}

// TestRelayLogsFollowing_CleanEnd locks today's contract: same node, no
// migration -> the stream ends after the agent EOFs, the loop does NOT
// re-dial a stopped VM.
func TestRelayLogsFollowing_CleanEnd(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	nodeA := uuid.New()
	agentA := newLogsAgent(t, "only-line\n")

	stub := &logsStoreStub{
		vm:          logsTestVM(t, owner, nodeA),
		pinSequence: []uuid.UUID{nodeA}, // never flips
		nodes:       map[uuid.UUID]store.Node{nodeA: {ID: nodeA, Name: "a", AdvertisedEndpoint: agentA.srv.URL}},
		migrating:   false,
	}
	h := logsFollowHandler(stub, logsDialMap(stub.nodes))

	rec := httptest.NewRecorder()
	req, cancel := followRequest(t, stub.vm.Name)
	defer cancel()

	done := make(chan struct{})
	go func() { h.relayLogsFollowing(rec, req, stub.vm); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("relayLogsFollowing did not return on clean end")
	}

	if got := rec.Body.String(); !strings.Contains(got, "only-line") {
		t.Errorf("body = %q, want only-line", got)
	}
	if c := agentA.dialCount(); c != 1 {
		t.Errorf("agent A dialed %d times, want 1 (no re-dial of a stopped VM)", c)
	}
}

// flipAfterStub flips PinnedNodeID from `from` to `to` on the Nth VMByName
// call (1-based), modelling a cutover that commits a couple of polls after
// the upstream broke. It reports migrating=true until the flip.
type flipAfterStub struct {
	logsStoreStub
	from, to uuid.UUID
	flipOn   int
}

func (s *flipAfterStub) VMByName(context.Context, string) (store.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vmCalls++
	pin := s.from
	if s.vmCalls >= s.flipOn {
		pin = s.to
	}
	v := s.vm
	v.PinnedNodeID = &pin
	return v, nil
}

func (s *flipAfterStub) ActiveMigrationForVM(context.Context, uuid.UUID) (store.Migration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vmCalls >= s.flipOn {
		return store.Migration{}, false, nil
	}
	return store.Migration{}, true, nil
}

// TestRelayLogsFollowing_WaitsForFlip drives the safety net: the upstream
// breaks while the node is unchanged but a migration is active; after a few
// polls the store flips to node B and the loop must reattach to B.
func TestRelayLogsFollowing_WaitsForFlip(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	nodeA, nodeB := uuid.New(), uuid.New()
	agentA := newLogsAgent(t, "A-line\n")
	agentB := newLogsAgent(t, "B-line\n")

	stub := &flipAfterStub{
		logsStoreStub: logsStoreStub{
			vm: logsTestVM(t, owner, nodeA),
			nodes: map[uuid.UUID]store.Node{
				nodeA: {ID: nodeA, Name: "a", AdvertisedEndpoint: agentA.srv.URL},
				nodeB: {ID: nodeB, Name: "b", AdvertisedEndpoint: agentB.srv.URL},
			},
		},
		from: nodeA, to: nodeB, flipOn: 3, // flips on the 3rd VMByName call
	}

	hh := logsFollowHandler(stub, logsDialMap(stub.nodes))

	rec := httptest.NewRecorder()
	req, cancel := followRequest(t, stub.vm.Name)
	defer cancel()
	hh.relayLogsFollowing(rec, req, stub.vm)

	got := rec.Body.String()
	if !strings.Contains(got, "B-line") {
		t.Errorf("body = %q, want B-line after wait-for-flip", got)
	}
}

// TestWaitForFlip_DeadlineReturnsFalse drives waitForFlip directly with a
// tiny deadline against a store that never flips -> it must return false.
func TestWaitForFlip_DeadlineReturnsFalse(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	nodeA := uuid.New()
	agentA := newLogsAgent(t, "x\n")
	stub := &logsStoreStub{
		vm:          logsTestVM(t, owner, nodeA),
		pinSequence: []uuid.UUID{nodeA},
		nodes:       map[uuid.UUID]store.Node{nodeA: {ID: nodeA, Name: "a", AdvertisedEndpoint: agentA.srv.URL}},
	}
	h := logsFollowHandler(stub, logsDialMap(stub.nodes))

	ctx := context.Background()
	_, ok := h.waitForFlip(ctx, stub.vm.Name, nodeA, 60*time.Millisecond, 10*time.Millisecond)
	if ok {
		t.Errorf("waitForFlip ok = true, want false on a node that never flips")
	}
}

// authedLogsRequest builds a GET /logs request with a principal that owns
// the VM, so CheckOwnership passes and Logs reaches the relay.
func authedLogsRequest(t *testing.T, owner uuid.UUID, vmName, rawQuery string) *http.Request {
	t.Helper()
	target := "/v1/vms/" + vmName + "/logs"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, &auth.User{ID: owner, Role: auth.RoleAdmin, Type: auth.TypeJWT})
	return req.WithContext(ctx)
}

// TestLogs_NonFollowSingleShot locks the single-shot contract: a request
// WITHOUT follow=true streams once and returns even though the store would
// report a node flip - the reconnect loop is not entered.
func TestLogs_NonFollowSingleShot(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	nodeA, nodeB := uuid.New(), uuid.New()
	agentA := newLogsAgent(t, "A-line\n")
	agentB := newLogsAgent(t, "B-line\n")

	stub := &logsStoreStub{
		vm:          logsTestVM(t, owner, nodeA),
		pinSequence: []uuid.UUID{nodeA, nodeB}, // would flip if the loop ran
		nodes: map[uuid.UUID]store.Node{
			nodeA: {ID: nodeA, Name: "a", AdvertisedEndpoint: agentA.srv.URL},
			nodeB: {ID: nodeB, Name: "b", AdvertisedEndpoint: agentB.srv.URL},
		},
	}
	stub.vm.OwnerID = owner
	h := logsFollowHandler(stub, logsDialMap(stub.nodes))

	rec := httptest.NewRecorder()
	h.Logs(rec, authedLogsRequest(t, owner, stub.vm.Name, "tail=10"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "A-line") || strings.Contains(got, "B-line") {
		t.Errorf("body = %q, want A-line only (no reconnect)", got)
	}
	if c := agentB.dialCount(); c != 0 {
		t.Errorf("node B dialed %d times, want 0 (single-shot must not reconnect)", c)
	}
}

// TestLogs_DialsIdentitySAN is the geo-routing seam test: the CP must dial the
// agent at the node's cluster-CA identity SAN (so the geo route resolver can
// reach it - direct for a public node, gateway splice for a NAT'd one), NOT at
// the raw AdvertisedEndpoint host. The node's Name differs from the httptest
// listener host, and the dial map maps only the identity SAN to the real addr,
// so the assertion has teeth: before the fix the agent sees the loopback Host,
// after it the identity SAN.
func TestLogs_DialsIdentitySAN(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	nodeID := uuid.New()
	agent := newLogsAgent(t, "line\n")

	stub := &logsStoreStub{
		vm:          logsTestVM(t, owner, nodeID),
		pinSequence: []uuid.UUID{nodeID},
		nodes:       map[uuid.UUID]store.Node{nodeID: {ID: nodeID, Name: "a", AdvertisedEndpoint: agent.srv.URL}},
	}
	stub.vm.OwnerID = owner
	h := logsFollowHandler(stub, map[string]string{auth.NodeIdentitySAN("a"): agent.host()})

	rec := httptest.NewRecorder()
	h.Logs(rec, authedLogsRequest(t, owner, stub.vm.Name, "tail=10"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got, want := agent.lastHost(), auth.NodeIdentitySAN("a"); got != want {
		t.Errorf("agent saw Host = %q, want %q (CP must dial the identity SAN, not the raw endpoint)", got, want)
	}
}
