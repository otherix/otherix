// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"crypto/tls"
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
	"github.com/otherix/otherix/internal/store"
)

// logsClientStub satisfies the handler's console-client dependency. Only
// HTTPClient is exercised by the logs path; it returns a client that
// trusts the httptest TLS servers the tests stand up.
type logsClientStub struct{}

func (logsClientStub) IssueConsoleToken(context.Context, string, string, string) (agentclient.IssueConsoleTokenResponse, error) {
	return agentclient.IssueConsoleTokenResponse{}, nil
}

func (logsClientStub) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test dials httptest TLS servers
	}}
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
// (clean EOF). It records how many times it was dialed.
type logsAgent struct {
	srv   *httptest.Server
	calls int
	mu    sync.Mutex
}

func newLogsAgent(t *testing.T, line string) *logsAgent {
	t.Helper()
	a := &logsAgent{}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		a.calls++
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

func logsFollowHandler(s Store) *Handler {
	return New(s,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{},
		ConsoleDeps{AgentClient: logsClientStub{}, AccessMode: "proxy"})
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
			nodeA: {ID: nodeA, AdvertisedEndpoint: agentA.srv.URL},
			nodeB: {ID: nodeB, AdvertisedEndpoint: agentB.srv.URL},
		},
	}
	h := logsFollowHandler(stub)

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
		nodes:       map[uuid.UUID]store.Node{nodeA: {ID: nodeA, AdvertisedEndpoint: agentA.srv.URL}},
		migrating:   false,
	}
	h := logsFollowHandler(stub)

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
