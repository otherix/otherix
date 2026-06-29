// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// consoleStreamStoreStub satisfies the handler's Store interface for the
// ConsoleStream rejection tests. It embeds Store (nil) so only VMByName
// has a body; any other call panics, which proves the handler bailed
// before touching the rest of the store.
type consoleStreamStoreStub struct {
	Store
	vmByNameErr error
}

func (s *consoleStreamStoreStub) VMByName(context.Context, string) (store.VM, error) {
	return store.VM{}, s.vmByNameErr
}

// consoleClientStub satisfies consoleClient so the handler's nil-check
// passes. The rejection paths under test return before any dial, so
// both methods are inert.
type consoleClientStub struct{}

func (consoleClientStub) IssueConsoleToken(context.Context, string, string, string) (agentclient.IssueConsoleTokenResponse, error) {
	return agentclient.IssueConsoleTokenResponse{}, errors.New("consoleClientStub: not implemented")
}

func (consoleClientStub) HTTPClient() *http.Client { return http.DefaultClient }

func consoleStreamHandler(s Store) *Handler {
	return New(s,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{},
		ConsoleDeps{AgentClient: consoleClientStub{}, AccessMode: "proxy"},
		SSHDeps{})
}

// consoleStreamRequest builds a GET request carrying the chi route
// context so chi.URLParam(r, "id") resolves to vmName inside the
// handler. token == "" omits the query parameter entirely.
func consoleStreamRequest(vmName, token string) *http.Request {
	target := "/v1/vms/" + url.PathEscape(vmName) + "/console-stream"
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// assertGenericConsoleRejection asserts the single response shape every
// unauthenticated-reachable rejection on the anonymous console-stream
// endpoint must produce: 401 unauthenticated with the fixed generic
// message. Identical shapes are the whole point - any divergence is a
// VM-name enumeration oracle.
func assertGenericConsoleRejection(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error.Code != response.CodeUnauthenticated {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeUnauthenticated)
	}
	const want = "invalid or expired console session"
	if body.Error.Message != want {
		t.Errorf("error.message = %q, want %q", body.Error.Message, want)
	}
}

// TestConsoleStreamUnknownVMIsGeneric401 is the enumeration-teeth test:
// an unknown VM name on the anonymous endpoint must produce the same
// 401 a rejected token produces, not a 404 that confirms the name does
// not exist.
func TestConsoleStreamUnknownVMIsGeneric401(t *testing.T) {
	t.Parallel()
	h := consoleStreamHandler(&consoleStreamStoreStub{vmByNameErr: store.ErrNotFound})

	rec := httptest.NewRecorder()
	h.ConsoleStream(rec, consoleStreamRequest("ghost", "bogus-token"))

	assertGenericConsoleRejection(t, rec)
}

// TestConsoleStreamMissingTokenIsGeneric401 locks the missing-token
// branch to the same generic rejection shape.
func TestConsoleStreamMissingTokenIsGeneric401(t *testing.T) {
	t.Parallel()
	h := consoleStreamHandler(&consoleStreamStoreStub{vmByNameErr: store.ErrNotFound})

	rec := httptest.NewRecorder()
	h.ConsoleStream(rec, consoleStreamRequest("ghost", ""))

	assertGenericConsoleRejection(t, rec)
}

// TestRelayUpstreamDialErrorTokenRejectedIsGeneric locks the
// agent-rejected-token relay (upstream 401) to the same generic
// rejection shape as the unknown-VM and missing-token branches.
func TestRelayUpstreamDialErrorTokenRejectedIsGeneric(t *testing.T) {
	t.Parallel()
	h := consoleStreamHandler(&consoleStreamStoreStub{})

	rec := httptest.NewRecorder()
	h.relayUpstreamDialError(rec, consoleStreamRequest("demo", "tok"),
		&http.Response{StatusCode: http.StatusUnauthorized},
		errors.New("upgrade failed"))

	assertGenericConsoleRejection(t, rec)
}

// TestBuildAgentConsoleURLSafeInputsUnchanged pins the historical wire
// format: URL-safe inputs (base64url token, DNS-label VM name) must
// emit byte-identical URLs after the escaping hardening.
func TestBuildAgentConsoleURLSafeInputsUnchanged(t *testing.T) {
	t.Parallel()
	got := buildAgentConsoleURL("agent.example.com:9090", "demo", "tok-123")
	want := "wss://agent.example.com:9090/v1/vms/demo/console-stream?token=tok-123"
	if got != want {
		t.Errorf("buildAgentConsoleURL(safe) = %q, want %q", got, want)
	}
}

// TestBuildAgentConsoleURLEscapesNameAndToken drives inputs that need
// percent-encoding and asserts the token round-trips through net/url
// and the VM name lands escaped in the path.
func TestBuildAgentConsoleURLEscapesNameAndToken(t *testing.T) {
	t.Parallel()
	const rawToken = "a b+c/d=="
	got := buildAgentConsoleURL("agent.example.com:9090", "demo vm", rawToken)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	if tok := u.Query().Get("token"); tok != rawToken {
		t.Errorf("token round-trip = %q, want %q", tok, rawToken)
	}
	wantPath := "/v1/vms/demo%20vm/console-stream"
	if u.EscapedPath() != wantPath {
		t.Errorf("escaped path = %q, want %q", u.EscapedPath(), wantPath)
	}
	want := "wss://agent.example.com:9090/v1/vms/demo%20vm/console-stream?token=a+b%2Bc%2Fd%3D%3D"
	if got != want {
		t.Errorf("buildAgentConsoleURL = %q, want %q", got, want)
	}
}

// resolveConsoleNodeStoreStub returns a fixed node for NodeByID so
// resolveConsoleNode can be exercised without etcd. VMByName is not
// called by resolveConsoleNode (the caller passes the vm in).
type resolveConsoleNodeStoreStub struct {
	Store
	node store.Node
}

func (s *resolveConsoleNodeStoreStub) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	return s.node, nil
}

func TestResolveConsoleNodeSplitsHostAndEndpoint(t *testing.T) {
	t.Parallel()
	nid := uuid.New()
	h := consoleStreamHandler(&resolveConsoleNodeStoreStub{
		node: store.Node{ID: nid, AdvertisedEndpoint: "https://agent.example.com:9090"},
	})
	vm := store.VM{ID: uuid.New(), PinnedNodeID: &nid}

	got, err := h.resolveConsoleNode(context.Background(), vm)
	if err != nil {
		t.Fatalf("resolveConsoleNode: %v", err)
	}
	if got.id != nid {
		t.Errorf("id = %v, want %v", got.id, nid)
	}
	if got.host != "agent.example.com:9090" {
		t.Errorf("host = %q, want agent.example.com:9090", got.host)
	}
	if got.endpoint != "https://agent.example.com:9090" {
		t.Errorf("endpoint = %q, want https://agent.example.com:9090", got.endpoint)
	}
}

// --- WS seam-test harness (shared by Task 3 and Task 4) ---

// wsAgentServer is a TLS httptest server standing in for an agent's
// console-stream endpoint. It accepts a WS, optionally echoes the first
// byte it reads, records bytes it receives, and closes when told.
type wsAgentServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []byte
	accept chan struct{} // closed once a connection is accepted
	closed chan struct{} // close it to make the handler drop the upstream
}

func newWSAgentServer(t *testing.T, echo bool) *wsAgentServer {
	t.Helper()
	a := &wsAgentServer{accept: make(chan struct{}), closed: make(chan struct{})}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()
		closeOnce(a.accept)
		ctx := r.Context()
		go func() {
			for {
				typ, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				a.mu.Lock()
				a.got = append(a.got, data...)
				a.mu.Unlock()
				if echo {
					_ = c.Write(ctx, typ, data)
				}
			}
		}()
		select {
		case <-a.closed:
		case <-ctx.Done():
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *wsAgentServer) host() string { return a.srv.Listener.Addr().String() }

func (a *wsAgentServer) received() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]byte, len(a.got))
	copy(out, a.got)
	return out
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// followStoreStub is a flippable store: VMByName returns the VM with
// PinnedNodeID = nodes[idx], advancing idx by one per call up to the
// last (so the first read sees node A, later reads see node B). It also
// drives ActiveMigrationForVM.
type followStoreStub struct {
	Store
	mu        sync.Mutex
	vm        store.VM
	nodes     []store.Node
	idx       int
	migrating bool
}

func (s *followStoreStub) VMByName(context.Context, string) (store.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[s.idx]
	if s.idx < len(s.nodes)-1 {
		s.idx++
	}
	vm := s.vm
	id := n.ID
	vm.PinnedNodeID = &id
	return vm, nil
}

func (s *followStoreStub) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return store.Node{}, store.ErrNotFound
}

func (s *followStoreStub) ActiveMigrationForVM(context.Context, uuid.UUID) (store.Migration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.Migration{}, s.migrating, nil
}

// recordingConsoleClient routes IssueConsoleToken to a recorded endpoint
// and returns a fixed token; HTTPClient is an InsecureSkipVerify client
// so the CP can dial the TLS agent test servers.
type recordingConsoleClient struct {
	mu        sync.Mutex
	endpoints []string
	token     string
	issueErr  error
}

func (c *recordingConsoleClient) IssueConsoleToken(_ context.Context, endpoint, _, _ string) (agentclient.IssueConsoleTokenResponse, error) {
	c.mu.Lock()
	c.endpoints = append(c.endpoints, endpoint)
	err := c.issueErr
	c.mu.Unlock()
	if err != nil {
		return agentclient.IssueConsoleTokenResponse{}, err
	}
	return agentclient.IssueConsoleTokenResponse{Token: c.token, Protocol: "serial"}, nil
}

func (c *recordingConsoleClient) mintedEndpoints() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.endpoints...)
}

func (c *recordingConsoleClient) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test client
	}}
}

func newFollowHandler(s Store, c consoleClient) *Handler {
	return New(s, slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{}, ConsoleDeps{AgentClient: c, AccessMode: "proxy"}, SSHDeps{})
}

func cpConsoleServer(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/v1/vms/{id}/console-stream", h.ConsoleStream)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func dialOperator(t *testing.T, cp *httptest.Server, vmName, token string) *websocket.Conn {
	t.Helper()
	u := "ws" + cp.URL[len("http"):] + "/v1/vms/" + vmName + "/console-stream?token=" + token
	c, _, err := websocket.Dial(context.Background(), u, nil)
	if err != nil {
		t.Fatalf("operator dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// TestConsoleRelayBridgesAndClosesCleanly is the Task 3 seam test: agent
// A echoes a byte over the bridge, then A closes the upstream; with no
// flip and no migration the operator WS closes normally (no re-attach).
func TestConsoleRelayBridgesAndClosesCleanly(t *testing.T) {
	t.Parallel()
	agentA := newWSAgentServer(t, true)
	nodeA := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentA.host()}
	st := &followStoreStub{vm: store.VM{ID: uuid.New(), Name: "demo"}, nodes: []store.Node{nodeA}}
	cl := &recordingConsoleClient{token: "tok"}
	cp := cpConsoleServer(t, newFollowHandler(st, cl))

	op := dialOperator(t, cp, "demo", "tok")
	if err := op.Write(context.Background(), websocket.MessageBinary, []byte("p")); err != nil {
		t.Fatalf("operator write: %v", err)
	}
	typ, data, err := op.Read(context.Background())
	if err != nil || typ != websocket.MessageBinary || string(data) != "p" {
		t.Fatalf("echo read = %q,%v,%v, want p", data, typ, err)
	}
	if got := agentA.received(); string(got) != "p" {
		t.Errorf("agent received = %q, want p", got)
	}
	if eps := cl.mintedEndpoints(); len(eps) != 0 {
		t.Errorf("minted endpoints = %v, want none (no re-attach on first attempt)", eps)
	}
	closeOnce(agentA.closed)
	if _, _, err := op.Read(context.Background()); err == nil {
		t.Errorf("operator read after clean close = nil err, want close")
	}
}

// TestConsoleFollowReattachesAtCutover is the headline seam test: agent
// A echoes, then drops the upstream (cutover); the store flips
// PinnedNodeID A -> B; the CP must mint a token against B's endpoint,
// re-dial B, and echo a byte written AFTER the cutover on the SAME
// operator WS.
func TestConsoleFollowReattachesAtCutover(t *testing.T) {
	t.Parallel()
	agentA := newWSAgentServer(t, true)
	agentB := newWSAgentServer(t, true)
	nodeA := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentA.host()}
	nodeB := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentB.host()}
	st := &followStoreStub{
		vm:    store.VM{ID: uuid.New(), Name: "demo"},
		nodes: []store.Node{nodeA, nodeB},
	}
	cl := &recordingConsoleClient{token: "tok"}
	cp := cpConsoleServer(t, newFollowHandler(st, cl))

	op := dialOperator(t, cp, "demo", "tok")
	_ = op.Write(context.Background(), websocket.MessageBinary, []byte("a"))
	if _, data, err := op.Read(context.Background()); err != nil || string(data) != "a" {
		t.Fatalf("pre-cutover echo = %q,%v, want a", data, err)
	}
	closeOnce(agentA.closed)

	deadline := time.Now().Add(5 * time.Second)
	var echoed bool
	for time.Now().Before(deadline) {
		_ = op.Write(context.Background(), websocket.MessageBinary, []byte("b"))
		typ, data, err := op.Read(context.Background())
		if err == nil && typ == websocket.MessageBinary && string(data) == "b" {
			echoed = true
			break
		}
		if err != nil {
			t.Fatalf("post-cutover read error before reattach: %v", err)
		}
	}
	if !echoed {
		t.Errorf("post-cutover byte never echoed from B")
	}
	if eps := cl.mintedEndpoints(); len(eps) == 0 || eps[len(eps)-1] != nodeB.AdvertisedEndpoint {
		t.Errorf("minted endpoints = %v, want last == %q", eps, nodeB.AdvertisedEndpoint)
	}
}

// TestConsoleFollowBuffersKeystrokesDuringGap: bytes written while B has
// not yet accepted are buffered and delivered to B in order on reattach.
func TestConsoleFollowBuffersKeystrokesDuringGap(t *testing.T) {
	t.Parallel()
	agentA := newWSAgentServer(t, false)
	agentB := newWSAgentServer(t, false)
	nodeA := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentA.host()}
	nodeB := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentB.host()}
	st := &followStoreStub{vm: store.VM{ID: uuid.New(), Name: "demo"}, nodes: []store.Node{nodeA, nodeB}}
	cp := cpConsoleServer(t, newFollowHandler(st, &recordingConsoleClient{token: "tok"}))

	op := dialOperator(t, cp, "demo", "tok")
	<-agentA.accept
	closeOnce(agentA.closed)
	_ = op.Write(context.Background(), websocket.MessageBinary, []byte("buffered"))

	<-agentB.accept
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if string(agentB.received()) == "buffered" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("agentB received %q, want buffered", agentB.received())
}

// TestConsoleFollowGivesUpOnWindowExpiry: ActiveMigrationForVM stays true
// but PinnedNodeID never flips -> the loop gives up at the (shrunk)
// window and closes with consoleMigratedCloseCode.
func TestConsoleFollowGivesUpOnWindowExpiry(t *testing.T) {
	// NOT t.Parallel: shrinkConsoleReattach mutates the package-level
	// consoleReattachWindowVar/consoleReattachPollVar that the other
	// (parallel) ConsoleFollow tests read via decideReattach. Running this
	// writer sequentially keeps its mutate+restore in the non-parallel
	// phase, before the parallel readers resume - no data race on the vars.
	defer shrinkConsoleReattach(t, 300*time.Millisecond, 50*time.Millisecond)()

	agentA := newWSAgentServer(t, false)
	nodeA := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentA.host()}
	st := &followStoreStub{
		vm: store.VM{ID: uuid.New(), Name: "demo"}, nodes: []store.Node{nodeA}, migrating: true,
	}
	cp := cpConsoleServer(t, newFollowHandler(st, &recordingConsoleClient{token: "tok"}))

	op := dialOperator(t, cp, "demo", "tok")
	<-agentA.accept
	closeOnce(agentA.closed)

	_, _, err := op.Read(context.Background())
	if websocket.CloseStatus(err) != consoleMigratedCloseCode {
		t.Errorf("close status = %v, want %v (err=%v)", websocket.CloseStatus(err), consoleMigratedCloseCode, err)
	}
}

// TestConsoleFollowTransientThenSuccess: B's IssueConsoleToken errors for
// the first polls (target Running-before-attachMux race) then succeeds;
// the loop retries within the window and re-bridges.
func TestConsoleFollowTransientThenSuccess(t *testing.T) {
	t.Parallel()
	agentB := newWSAgentServer(t, true)
	agentA := newWSAgentServer(t, true)
	nodeA := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentA.host()}
	nodeB := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://" + agentB.host()}
	st := &followStoreStub{vm: store.VM{ID: uuid.New(), Name: "demo"}, nodes: []store.Node{nodeA, nodeB}}
	cl := &flakyConsoleClient{recordingConsoleClient: recordingConsoleClient{token: "tok"}, failFirst: 2}
	cp := cpConsoleServer(t, newFollowHandler(st, cl))

	op := dialOperator(t, cp, "demo", "tok")
	<-agentA.accept
	closeOnce(agentA.closed)

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		_ = op.Write(context.Background(), websocket.MessageBinary, []byte("z"))
		if _, data, err := op.Read(context.Background()); err == nil && string(data) == "z" {
			return
		} else if err != nil {
			t.Fatalf("read before reattach: %v", err)
		}
	}
	t.Errorf("never re-bridged after transient mint failures")
}

// flakyConsoleClient fails IssueConsoleToken failFirst times (per the
// target Running-before-attachMux race) then behaves like its embedded
// recording client.
type flakyConsoleClient struct {
	recordingConsoleClient
	mu        sync.Mutex
	failFirst int
}

func (c *flakyConsoleClient) IssueConsoleToken(ctx context.Context, endpoint, vm, proto string) (agentclient.IssueConsoleTokenResponse, error) {
	c.mu.Lock()
	fail := c.failFirst > 0
	if fail {
		c.failFirst--
	}
	c.mu.Unlock()
	if fail {
		return agentclient.IssueConsoleTokenResponse{}, errors.New("vm_not_running")
	}
	return c.recordingConsoleClient.IssueConsoleToken(ctx, endpoint, vm, proto)
}

// shrinkConsoleReattach swaps the package-level reattach tunables to fast
// test values and returns a restore func.
func shrinkConsoleReattach(t *testing.T, window, poll time.Duration) func() {
	t.Helper()
	ow, op := consoleReattachWindowVar, consoleReattachPollVar
	consoleReattachWindowVar, consoleReattachPollVar = window, poll
	return func() { consoleReattachWindowVar, consoleReattachPollVar = ow, op }
}
