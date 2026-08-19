// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// relayStoreStub satisfies the handler's Store interface for the
// Relay tests: the grant lookup, the VM-by-name load, and the owning
// node read. Any other call panics (embedded nil Store), which proves the
// handler bailed before touching the rest of the store on a rejection.
type relayStoreStub struct {
	Store
	grant    store.IngressGrant
	grantErr error
	vm       store.VM
	vmErr    error
	node     store.Node
}

func (s *relayStoreStub) IngressGrantByTokenHash(context.Context, []byte) (store.IngressGrant, error) {
	return s.grant, s.grantErr
}

func (s *relayStoreStub) VMByName(context.Context, string) (store.VM, error) {
	return s.vm, s.vmErr
}

func (s *relayStoreStub) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	return s.node, nil
}

// dialSpyClient records whether the relay ever dialed an agent. A
// rejection path must answer before any dial, so dialed must stay false
// on the out-of-scope / revoked / missing-token cases.
type dialSpyClient struct {
	dialed bool
}

type dialSpyTransport struct{ spy *dialSpyClient }

func (t *dialSpyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.spy.dialed = true
	return nil, io.EOF
}

func (c *dialSpyClient) IssueConsoleToken(context.Context, string, string, string) (agentclient.IssueConsoleTokenResponse, error) {
	return agentclient.IssueConsoleTokenResponse{}, io.EOF
}

func (c *dialSpyClient) HTTPClient() *http.Client {
	return &http.Client{Transport: &dialSpyTransport{spy: c}}
}

func relayHandler(s Store, c consoleClient) *Handler {
	return New(s,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{},
		ConsoleDeps{AgentClient: c, AccessMode: "proxy"},
		SSHDeps{})
}

// relayRequest builds a GET request carrying the chi route context so
// chi.URLParam(r, "id") resolves to vmName, and an optional bearer token.
func relayRequest(vmName, token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/vms/"+url.PathEscape(vmName)+"/relay", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// assertGenericRelayRejection asserts the single response shape every
// reachable rejection on the relay endpoint must produce: 401 with
// the uniform ssh_session_rejected code. Identical shapes are the point -
// any divergence is a VM-name enumeration oracle.
func assertGenericRelayRejection(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error.Code != response.CodeSSHSessionRejected {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeSSHSessionRejected)
	}
}

// grantFor builds a stored grant authorizing vmName on the SSH port for login,
// not revoked and not expired.
func grantFor(vmName, login string) store.IngressGrant {
	return store.IngressGrant{
		ID:  uuid.New(),
		VMs: []store.IngressGrantVM{{VMName: vmName, Ports: []int{22}, Login: login}},
	}
}

// TestRelayMissingTokenRejected: no bearer -> uniform 401, no dial.
func TestRelayMissingTokenRejected(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	h := relayHandler(&relayStoreStub{}, spy)

	rec := httptest.NewRecorder()
	h.Relay(rec, relayRequest("demo", ""))

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on missing token, want no dial")
	}
}

// TestRelayOutOfScopeGrantRejectedNoDial: a grant that does not list
// the requested VM -> uniform 401, no upstream dial.
func TestRelayOutOfScopeGrantRejectedNoDial(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	st := &relayStoreStub{grant: grantFor("other", "ubuntu")}
	h := relayHandler(st, spy)

	rec := httptest.NewRecorder()
	h.Relay(rec, relayRequest("demo", "otx_ingressgrant_abc"))

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on out-of-scope grant, want no dial")
	}
}

// TestRelayRevokedGrantRejected: a revoked grant in scope -> uniform
// 401, no dial.
func TestRelayRevokedGrantRejected(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	g := grantFor("demo", "ubuntu")
	g.Revoked = true
	st := &relayStoreStub{grant: g}
	h := relayHandler(st, spy)

	rec := httptest.NewRecorder()
	h.Relay(rec, relayRequest("demo", "otx_ingressgrant_abc"))

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on revoked grant, want no dial")
	}
}

// TestRelayUnknownGrantRejected: the token does not resolve to any
// grant -> uniform 401.
func TestRelayUnknownGrantRejected(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	st := &relayStoreStub{grantErr: store.ErrNotFound}
	h := relayHandler(st, spy)

	rec := httptest.NewRecorder()
	h.Relay(rec, relayRequest("demo", "otx_ingressgrant_abc"))

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on unknown grant, want no dial")
	}
}

// TestRelayGrantRelaysEndToEnd is the headline seam test: an in-scope
// grant authorizes the connect, the CP dials the owning agent's ssh-pipe
// over the (insecure-test) mTLS client, and bytes the operator writes are
// echoed back through the relay.
func TestRelayGrantRelaysEndToEnd(t *testing.T) {
	t.Parallel()
	agent := newWSAgentServer(t, true)
	node := store.Node{ID: uuid.New(), Name: "a", AdvertisedEndpoint: "https://" + agent.host()}
	vm := store.VM{ID: uuid.New(), Name: "demo", PinnedNodeID: &node.ID}
	st := &relayStoreStub{grant: grantFor("demo", "ubuntu"), vm: vm, node: node}

	r := chi.NewRouter()
	r.Get("/v1/vms/{id}/relay", relayHandler(st, &recordingConsoleClient{dialMap: nodeDialMap(node)}).Relay)
	cp := httptest.NewServer(r)
	t.Cleanup(cp.Close)

	u := "ws" + cp.URL[len("http"):] + "/v1/vms/demo/relay"
	op, _, err := websocket.Dial(context.Background(),
		u, &websocket.DialOptions{HTTPHeader: bearerHeader("otx_ingressgrant_abc")})
	if err != nil {
		t.Fatalf("operator dial: %v", err)
	}
	t.Cleanup(func() { _ = op.Close(websocket.StatusNormalClosure, "") })

	if err := op.Write(context.Background(), websocket.MessageBinary, []byte("ssh-bytes")); err != nil {
		t.Fatalf("operator write: %v", err)
	}
	typ, data, err := op.Read(context.Background())
	if err != nil || typ != websocket.MessageBinary || string(data) != "ssh-bytes" {
		t.Fatalf("echo read = %q,%v,%v, want ssh-bytes", data, typ, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if string(agent.received()) == "ssh-bytes" {
			// The CP must have dialed the agent's ssh-pipe at the node identity
			// SAN so the geo resolver can route it, not the raw loopback host.
			if got, want := agent.lastHost(), auth.NodeIdentitySAN("a"); got != want {
				t.Errorf("agent saw Host = %q, want %q", got, want)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("agent received %q, want ssh-bytes", agent.received())
}

// TestRelaySourceIPPinnedGrantOutOfPinRejectedNoDial: a grant carrying a
// source-IP pin dialed from a RemoteAddr OUTSIDE the pin is the uniform 401
// with no upstream dial. The relay IS the data path for a bridge VM, so the
// pin must be enforced here and not only at the broker.
//
// The VM and its owning node are seeded so the source-IP check is the ONLY
// thing standing between the request and a dial: were the pin wrongly ignored,
// VM load and node resolve would both succeed and the relay would reach the
// dial - flipping spy.dialed to true. So the spy.dialed==false assertion is
// driven by the source-IP check itself, not by a downstream resolve failure.
func TestRelaySourceIPPinnedGrantOutOfPinRejectedNoDial(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	node := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://agent.example.com:9090"}
	vm := store.VM{ID: uuid.New(), Name: "demo", PinnedNodeID: &node.ID}
	g := grantFor("demo", "ubuntu")
	pin := "203.0.113.0/24"
	g.SourceIP = &pin
	st := &relayStoreStub{grant: g, vm: vm, node: node}
	h := relayHandler(st, spy)

	req := relayRequest("demo", "otx_ingressgrant_abc")
	req.RemoteAddr = "198.51.100.7:12345" // outside 203.0.113.0/24

	rec := httptest.NewRecorder()
	h.Relay(rec, req)

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on out-of-pin source IP, want no dial")
	}
}

// TestRelaySourceIPPinnedGrantInPinRelays is the positive counterpart: a grant
// whose pin admits the loopback dial address still relays end to end, proving
// the source-IP enforcement is not over-broad.
func TestRelaySourceIPPinnedGrantInPinRelays(t *testing.T) {
	t.Parallel()
	agent := newWSAgentServer(t, true)
	node := store.Node{ID: uuid.New(), Name: "a", AdvertisedEndpoint: "https://" + agent.host()}
	vm := store.VM{ID: uuid.New(), Name: "demo", PinnedNodeID: &node.ID}
	g := grantFor("demo", "ubuntu")
	// The httptest server accepts the operator over loopback, so RemoteAddr is
	// 127.0.0.1; a pin covering loopback must still authorize the connect.
	pin := "127.0.0.0/8"
	g.SourceIP = &pin
	st := &relayStoreStub{grant: g, vm: vm, node: node}

	r := chi.NewRouter()
	r.Get("/v1/vms/{id}/relay", relayHandler(st, &recordingConsoleClient{dialMap: nodeDialMap(node)}).Relay)
	cp := httptest.NewServer(r)
	t.Cleanup(cp.Close)

	u := "ws" + cp.URL[len("http"):] + "/v1/vms/demo/relay"
	op, _, err := websocket.Dial(context.Background(),
		u, &websocket.DialOptions{HTTPHeader: bearerHeader("otx_ingressgrant_abc")})
	if err != nil {
		t.Fatalf("operator dial: %v", err)
	}
	t.Cleanup(func() { _ = op.Close(websocket.StatusNormalClosure, "") })

	if err := op.Write(context.Background(), websocket.MessageBinary, []byte("ssh-bytes")); err != nil {
		t.Fatalf("operator write: %v", err)
	}
	typ, data, err := op.Read(context.Background())
	if err != nil || typ != websocket.MessageBinary || string(data) != "ssh-bytes" {
		t.Fatalf("echo read = %q,%v,%v, want ssh-bytes", data, typ, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if string(agent.received()) == "ssh-bytes" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("agent received %q, want ssh-bytes", agent.received())
}

// TestRelayVMIDMatchRelays is the positive counterpart to the mismatch test: a
// grant whose bound VMID equals the name-resolved VM's id relays end to end,
// proving the identity assertion is not over-broad (a correctly-bound grant is
// never rejected).
func TestRelayVMIDMatchRelays(t *testing.T) {
	t.Parallel()
	agent := newWSAgentServer(t, true)
	node := store.Node{ID: uuid.New(), Name: "a", AdvertisedEndpoint: "https://" + agent.host()}
	vmID := uuid.New()
	vm := store.VM{ID: vmID, Name: "demo", PinnedNodeID: &node.ID}
	grant := store.IngressGrant{
		ID:  uuid.New(),
		VMs: []store.IngressGrantVM{{VMName: "demo", VMID: vmID, Ports: []int{22}, Login: "ubuntu"}},
	}
	st := &relayStoreStub{grant: grant, vm: vm, node: node}

	r := chi.NewRouter()
	r.Get("/v1/vms/{id}/relay", relayHandler(st, &recordingConsoleClient{dialMap: nodeDialMap(node)}).Relay)
	cp := httptest.NewServer(r)
	t.Cleanup(cp.Close)

	u := "ws" + cp.URL[len("http"):] + "/v1/vms/demo/relay"
	op, _, err := websocket.Dial(context.Background(),
		u, &websocket.DialOptions{HTTPHeader: bearerHeader("otx_ingressgrant_abc")})
	if err != nil {
		t.Fatalf("operator dial: %v", err)
	}
	t.Cleanup(func() { _ = op.Close(websocket.StatusNormalClosure, "") })

	if err := op.Write(context.Background(), websocket.MessageBinary, []byte("ssh-bytes")); err != nil {
		t.Fatalf("operator write: %v", err)
	}
	typ, data, err := op.Read(context.Background())
	if err != nil || typ != websocket.MessageBinary || string(data) != "ssh-bytes" {
		t.Fatalf("echo read = %q,%v,%v, want ssh-bytes", data, typ, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if string(agent.received()) == "ssh-bytes" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("agent received %q, want ssh-bytes", agent.received())
}

// TestRelayNameReuseVMIDMismatchRejectedNoDial: a grant bound to VM "demo" at
// UUID X must not authorize a session once the name "demo" resolves to a
// DIFFERENT VM (UUID Y) - the case where the original VM was deleted and its
// name reused, potentially by another owner. The connect-time VM-ID assertion
// rejects the stale binding with the uniform 401 and no upstream dial.
//
// The VM and its owning node are seeded so the VM-ID check is the ONLY thing
// standing between the request and a dial: were the stale name binding honored,
// VM load and node resolve would both succeed and the relay would reach the
// dial - flipping spy.dialed to true. So spy.dialed==false is driven by the
// ID check itself, not a downstream resolve failure.
func TestRelayNameReuseVMIDMismatchRejectedNoDial(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	node := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://agent.example.com:9090"}
	boundID := uuid.New()  // the VM the grant was created against
	reusedID := uuid.New() // the name "demo" now points at a different VM
	vm := store.VM{ID: reusedID, Name: "demo", PinnedNodeID: &node.ID}
	grant := store.IngressGrant{
		ID:  uuid.New(),
		VMs: []store.IngressGrantVM{{VMName: "demo", VMID: boundID, Ports: []int{22}, Login: "ubuntu"}},
	}
	st := &relayStoreStub{grant: grant, vm: vm, node: node}
	h := relayHandler(st, spy)

	rec := httptest.NewRecorder()
	h.Relay(rec, relayRequest("demo", "otx_ingressgrant_abc"))

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on VM-ID mismatch, want no dial")
	}
}

func bearerHeader(tok string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	return h
}

// TestBuildSSHPipeURLSafeInputsUnchanged pins the wire format for URL-safe
// inputs: a DNS-label VM name plus a guest port must emit a byte-identical URL,
// with the port carried as a query parameter so the route stays unchanged.
func TestBuildSSHPipeURLSafeInputsUnchanged(t *testing.T) {
	t.Parallel()
	got := agentclient.BuildSSHPipeURL("agent.example.com:9090", "demo", 5432)
	want := "wss://agent.example.com:9090/v1/vms/demo/ssh-pipe?port=5432"
	if got != want {
		t.Errorf("BuildSSHPipeURL(safe) = %q, want %q", got, want)
	}
}

// TestBuildSSHPipeURLEscapesName drives a VM name needing percent-encoding
// and asserts it lands escaped in the path while the port rides the query.
func TestBuildSSHPipeURLEscapesName(t *testing.T) {
	t.Parallel()
	got := agentclient.BuildSSHPipeURL("agent.example.com:9090", "demo vm", 22)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	wantPath := "/v1/vms/demo%20vm/ssh-pipe"
	if u.EscapedPath() != wantPath {
		t.Errorf("escaped path = %q, want %q", u.EscapedPath(), wantPath)
	}
	if got := u.Query().Get("port"); got != "22" {
		t.Errorf("port query = %q, want %q", got, "22")
	}
}

// TestRelayForwardsPort proves the CP relay forwards the requested guest
// port to the agent's ssh-pipe: the operator dials the relay with ?port=5432
// and the agent leg receives that same port on its query string. The IP the
// agent dials stays lease-derived on the agent side; the port is the only
// wire-influenced input the relay carries through.
func TestRelayForwardsPort(t *testing.T) {
	t.Parallel()
	agent := newWSAgentServer(t, true)
	node := store.Node{ID: uuid.New(), Name: "a", AdvertisedEndpoint: "https://" + agent.host()}
	vm := store.VM{ID: uuid.New(), Name: "demo", PinnedNodeID: &node.ID}
	// The grant must authorize the exact port the relay forwards: the relay
	// enforces the per-VM port scope, so a grant that lists 5432 is required for
	// the ?port=5432 dial to be accepted.
	grant := store.IngressGrant{
		ID:  uuid.New(),
		VMs: []store.IngressGrantVM{{VMName: "demo", Ports: []int{5432}, Login: "ubuntu"}},
	}
	st := &relayStoreStub{grant: grant, vm: vm, node: node}

	r := chi.NewRouter()
	r.Get("/v1/vms/{id}/relay", relayHandler(st, &recordingConsoleClient{dialMap: nodeDialMap(node)}).Relay)
	cp := httptest.NewServer(r)
	t.Cleanup(cp.Close)

	u := "ws" + cp.URL[len("http"):] + "/v1/vms/demo/relay?port=5432"
	op, _, err := websocket.Dial(context.Background(),
		u, &websocket.DialOptions{HTTPHeader: bearerHeader("otx_ingressgrant_abc")})
	if err != nil {
		t.Fatalf("operator dial: %v", err)
	}
	t.Cleanup(func() { _ = op.Close(websocket.StatusNormalClosure, "") })

	// Drive one byte so the agent leg is definitely accepted before we read
	// back the query it observed.
	if err := op.Write(context.Background(), websocket.MessageBinary, []byte("x")); err != nil {
		t.Fatalf("operator write: %v", err)
	}
	if _, _, err := op.Read(context.Background()); err != nil {
		t.Fatalf("operator read: %v", err)
	}

	if got, want := agent.lastQuery(), "port=5432"; got != want {
		t.Errorf("agent ssh-pipe query = %q, want %q", got, want)
	}
}

// TestRelayPortNotInGrantRejectedNoDial: a grant that lists the VM on the
// SSH port only must not authorize a bridge session to a different guest port.
// Dialing ?port=5432 against a port-22 grant is the uniform 401 with no dial.
//
// The VM and its owning node are seeded so the port check is the ONLY thing
// standing between the request and a dial: were the port scope wrongly passed,
// VM load and node resolve would both succeed and the relay would reach the
// dial - flipping spy.dialed to true. So the spy.dialed==false assertion is
// driven by the port check itself, not by a downstream resolve failure.
func TestRelayPortNotInGrantRejectedNoDial(t *testing.T) {
	t.Parallel()
	spy := &dialSpyClient{}
	node := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://agent.example.com:9090"}
	vm := store.VM{ID: uuid.New(), Name: "demo", PinnedNodeID: &node.ID}
	// grantFor lists the VM on port 22 only; the request asks for 5432, which
	// the port check must reject before any VM load, node resolve, or dial.
	st := &relayStoreStub{grant: grantFor("demo", "ubuntu"), vm: vm, node: node}
	h := relayHandler(st, spy)

	req := relayRequest("demo", "otx_ingressgrant_abc")
	req.URL.RawQuery = "port=5432"

	rec := httptest.NewRecorder()
	h.Relay(rec, req)

	assertGenericRelayRejection(t, rec)
	if spy.dialed {
		t.Errorf("dialed upstream on out-of-scope port, want no dial")
	}
}
