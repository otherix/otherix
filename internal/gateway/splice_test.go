// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/auth"
)

const (
	testMACA = "02:00:00:00:00:0a"
	testMACB = "02:00:00:00:00:0b"
)

// startEcho starts a single-connection TCP echo server on loopback. It returns
// the listen address and a channel closed when the accepted connection ends
// (EOF or error) - the teardown signal the splice tests assert on.
func startEcho(t *testing.T) (addr string, connEnded <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	ended := make(chan struct{})
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			close(ended)
			return
		}
		_, _ = io.Copy(c, c)
		_ = c.Close()
		close(ended)
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), ended
}

// netDial is the production dial seam: a plain TCP dialer. Tests inject it so
// the connect handler reaches the loopback echo server. The bridge argument is
// ignored: the loopback target needs no interface binding.
func netDial(ctx context.Context, network, addr, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// newTestSessionCA generates a throwaway ingress-session CA, returning the signer
// the test mints credentials with and the PEM public half a gateway learns from
// heartbeat.
func newTestSessionCA(t *testing.T) (crypto.Signer, string) {
	t.Helper()
	mat, err := auth.GenerateSessionCA()
	if err != nil {
		t.Fatalf("generate session ca: %v", err)
	}
	signer, err := auth.ParseSessionCASigner(mat.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("parse session ca signer: %v", err)
	}
	return signer, string(mat.PublicKeyPEM)
}

// storeWith returns a session-CA store armed with pubPEM, as if a heartbeat
// carrying that public half had arrived.
func storeWith(t *testing.T, pubPEM string) *sessionCAStore {
	t.Helper()
	s := newSessionCAStore(discardLogger())
	s.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SessionCAPublicPEM: &pubPEM})
	if s.current() == nil {
		t.Fatal("session-CA store did not accept the public half")
	}
	return s
}

// signCred mints a credential for claims with the test signer.
func signCred(t *testing.T, signer crypto.Signer, c auth.SessionCredClaims) string {
	t.Helper()
	tok, err := auth.SignSessionCred(signer, c, time.Now())
	if err != nil {
		t.Fatalf("sign session cred: %v", err)
	}
	return tok
}

// fakeOverlays is a static overlayResolver: it returns Bridge/NetworkID/OK for
// every IP. Its session counters are no-ops; tests that assert session
// accounting use spyOverlays instead.
type fakeOverlays struct {
	bridge    string
	networkID string
	ok        bool
}

func (f fakeOverlays) OverlayNetworkForIP(netip.Addr) (string, string, bool) {
	return f.bridge, f.networkID, f.ok
}

func (fakeOverlays) AcquireSession(string) {}
func (fakeOverlays) ReleaseSession(string) {}

// fabricResolving returns a FakeFabric whose neighbor table resolves ip on bridge
// to mac (OK).
func fabricResolving(t *testing.T, bridge string, ip netip.Addr, mac string) *netfabric.FakeFabric {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("parse mac %q: %v", mac, err)
	}
	return &netfabric.FakeFabric{
		NeighborResult: map[string]netfabric.NeighborOutcome{
			netfabric.NeighborKey(bridge, ip): {MAC: hw, OK: true},
		},
	}
}

// rawConnectCred opens a raw TCP connection to the hijacking connect handler,
// sends the connect request carrying the bearer credential, consumes the status
// line + headers up to the blank line, and returns the connection, a reader
// positioned at the spliced byte stream, and the status line.
func rawConnectCred(t *testing.T, srvAddr, bearer string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatalf("dial connect handler: %v", err)
	}
	req := "POST /v1/connect HTTP/1.1\r\nHost: gw\r\n"
	if bearer != "" {
		req += "Authorization: Bearer " + bearer + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatalf("write connect request: %v", err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header line: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return c, br, status
}

// doConnect posts a connect request carrying the (optional) bearer and returns
// the response. Used for the non-hijacking refusal paths, which write a normal
// JSON error before any hijack.
func doConnect(t *testing.T, baseURL, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/connect", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post connect: %v", err)
	}
	return resp
}

// errorCode decodes the standard error envelope and returns error.code.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}

// gatedConnect builds the connect handler with the given collaborators and mounts
// it behind its real credential gate, returning a live test server.
func gatedConnect(t *testing.T, h *connectHandler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h.verifyCred(http.HandlerFunc(h.Connect)))
	t.Cleanup(srv.Close)
	return srv
}

// TestConnectSplicesBytesBothWays drives the full gate + handler against a stub
// echo target with a valid credential whose guest IP resolves (in the neighbor
// table) to the credential MAC, and proves a byte written by the client comes
// back echoed - the forward path carries traffic both ways.
func TestConnectSplicesBytesBothWays(t *testing.T) {
	echoAddr, _ := startEcho(t)
	host, portStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	ip := netip.MustParseAddr(host)

	signer, pubPEM := newTestSessionCA(t)
	bridge := "otvb100"
	h := &connectHandler{
		dial:     netDial,
		fabric:   fabricResolving(t, bridge, ip, testMACA),
		overlays: fakeOverlays{bridge: bridge, ok: true},
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
	defer func() { _ = c.Close() }()
	if !strings.Contains(status, "200") {
		t.Fatalf("connect status = %q, want a 200", strings.TrimSpace(status))
	}
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if got != "ping\n" {
		t.Errorf("echo = %q, want %q", got, "ping\n")
	}
}

// TestConnectTearsDownOnClientClose proves the kill-implies-teardown invariant:
// when the client side closes, the handler tears down the upstream leg too.
func TestConnectTearsDownOnClientClose(t *testing.T) {
	echoAddr, ended := startEcho(t)
	host, portStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	ip := netip.MustParseAddr(host)

	signer, pubPEM := newTestSessionCA(t)
	bridge := "otvb100"
	h := &connectHandler{
		dial:     netDial,
		fabric:   fabricResolving(t, bridge, ip, testMACA),
		overlays: fakeOverlays{bridge: bridge, ok: true},
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
	if _, err := io.WriteString(c, "x\n"); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	_ = c.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream leg not torn down within 5s after client close")
	}
}

// TestConnectAntiSSRFRefusesIPReuse is the load-bearing anti-SSRF test. A
// credential binds (MAC-A, guest IP); the guest IP has since been reassigned to a
// different NIC, so the gateway's neighbor table now resolves that IP to MAC-B.
// The handler must REFUSE: it dials only when the IP resolves to exactly the
// credential MAC.
//
// Revert-to-confirm: a weaker "is MAC-A anywhere in the table" check would dial
// MAC-B's VM and connect a stale credential to a different tenant. This test
// fails (the connect would be allowed) if the binding is not a forward
// IP -> resolved-MAC == credential-MAC equality, so it has teeth against that
// regression.
func TestConnectAntiSSRFRefusesIPReuse(t *testing.T) {
	signer, pubPEM := newTestSessionCA(t)
	ip := netip.MustParseAddr("10.42.0.5")
	bridge := "otvb100"

	// The guest IP now resolves to MAC-B, not the credential's MAC-A.
	h := &connectHandler{
		dial:     failDial(t),
		fabric:   fabricResolving(t, bridge, ip, testMACB),
		overlays: fakeOverlays{bridge: bridge, ok: true},
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
		t.Fatalf("ip-reuse connect status = %d, want 403 (refused)", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "permission_denied" {
		t.Errorf("error code = %q, want permission_denied", code)
	}
	if got := len(h.fabric.(*netfabric.FakeFabric).NeighborMACCalls); got != 1 {
		t.Errorf("NeighborMAC calls = %d, want exactly 1 (forward IP resolution)", got)
	}
}

// TestConnectAntiSSRFRefusesUnresolvedAndOffOverlay covers the other two binding
// failures: a guest IP with no resolved neighbor, and a guest IP on no declared
// overlay. Both must refuse without dialing.
func TestConnectAntiSSRFRefusesUnresolvedAndOffOverlay(t *testing.T) {
	signer, pubPEM := newTestSessionCA(t)
	ip := netip.MustParseAddr("10.42.0.5")
	bridge := "otvb100"

	tests := []struct {
		name     string
		fabric   *netfabric.FakeFabric
		overlays overlayResolver
	}{
		{
			name:     "unresolved neighbor",
			fabric:   &netfabric.FakeFabric{}, // no NeighborResult -> OK=false
			overlays: fakeOverlays{bridge: bridge, ok: true},
		},
		{
			name:     "guest ip off overlay",
			fabric:   fabricResolving(t, bridge, ip, testMACA),
			overlays: fakeOverlays{ok: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &connectHandler{
				dial:     failDial(t),
				fabric:   tt.fabric,
				overlays: tt.overlays,
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
		})
	}
}

// TestConnectCredFailures covers every credential-gate rejection: an absent
// bearer, a non-ingress token, a tampered signature, and an expired credential
// all collapse to 401; a credential that cannot be verified because no session CA
// has been received yet fails closed with 503.
func TestConnectCredFailures(t *testing.T) {
	signer, pubPEM := newTestSessionCA(t)
	ip := netip.MustParseAddr("10.42.0.5")
	bridge := "otvb100"

	validClaims := auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: ip, Port: 22,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	expiredClaims := validClaims
	expiredClaims.ExpiresAt = time.Now().Add(-time.Minute)

	valid := signCred(t, signer, validClaims)
	expired := signCred(t, signer, expiredClaims)
	tampered := corruptCredBody(valid)

	tests := []struct {
		name       string
		bearer     string
		armCA      bool
		wantStatus int
		wantCode   string
	}{
		{name: "absent bearer", bearer: "", armCA: true, wantStatus: 401, wantCode: "unauthenticated"},
		{name: "non-ingress token", bearer: "otx_anapitoken", armCA: true, wantStatus: 401, wantCode: "unauthenticated"},
		{name: "tampered signature", bearer: tampered, armCA: true, wantStatus: 401, wantCode: "unauthenticated"},
		{name: "expired credential", bearer: expired, armCA: true, wantStatus: 401, wantCode: "unauthenticated"},
		{name: "no session ca yet", bearer: valid, armCA: false, wantStatus: 503, wantCode: "ingress_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSessionCAStore(discardLogger())
			if tt.armCA {
				store = storeWith(t, pubPEM)
			}
			h := &connectHandler{
				dial:     failDial(t),
				fabric:   fabricResolving(t, bridge, ip, testMACA),
				overlays: fakeOverlays{bridge: bridge, ok: true},
				caStore:  store,
				slots:    newConnectSlots(8, 256),
				log:      discardLogger(),
			}
			srv := gatedConnect(t, h)
			resp := doConnect(t, srv.URL, tt.bearer)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if code := errorCode(t, resp); code != tt.wantCode {
				t.Errorf("error code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// TestConnectSlots pins the per-VM and per-gateway slot accounting directly.
func TestConnectSlots(t *testing.T) {
	s := newConnectSlots(2, 3)

	// Per-VM cap: vm-a may take 2, the third is refused.
	if err := s.acquire("vm-a"); err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := s.acquire("vm-a"); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if err := s.acquire("vm-a"); err == nil {
		t.Error("acquire 3 for vm-a: want per-VM cap error, got nil")
	}

	// Global cap: vm-b takes one (total 3), the next is refused even though
	// vm-b is under its own per-VM cap.
	if err := s.acquire("vm-b"); err != nil {
		t.Fatalf("acquire vm-b: %v", err)
	}
	if err := s.acquire("vm-b"); err == nil {
		t.Error("acquire vm-b at global cap: want gateway cap error, got nil")
	}

	// Releasing frees a slot for a later acquire.
	s.release("vm-a")
	if err := s.acquire("vm-b"); err != nil {
		t.Errorf("acquire after release: %v", err)
	}
}

// TestConnectRefusedAtCapacityThenReleased proves the handler enforces the slot
// cap: with the gateway's only slot already taken, a connect is refused 503;
// after the slot is released a connect splices through.
func TestConnectRefusedAtCapacityThenReleased(t *testing.T) {
	echoAddr, _ := startEcho(t)
	host, portStr, _ := net.SplitHostPort(echoAddr)
	port, _ := strconv.Atoi(portStr)
	ip := netip.MustParseAddr(host)

	signer, pubPEM := newTestSessionCA(t)
	bridge := "otvb100"
	slots := newConnectSlots(1, 1)
	h := &connectHandler{
		dial:     netDial,
		fabric:   fabricResolving(t, bridge, ip, testMACA),
		overlays: fakeOverlays{bridge: bridge, ok: true},
		caStore:  storeWith(t, pubPEM),
		slots:    slots,
		log:      discardLogger(),
	}
	srv := gatedConnect(t, h)
	token := signCred(t, signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: ip, Port: port,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	// Exhaust the single gateway slot out from under the handler.
	if err := slots.acquire("other-vm"); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	resp := doConnect(t, srv.URL, token)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("at-capacity status = %d, want 503", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "ingress_unavailable" {
		t.Errorf("error code = %q, want ingress_unavailable", code)
	}
	_ = resp.Body.Close()

	// Free the slot; a connect now splices through.
	slots.release("other-vm")
	c, _, status := rawConnectCred(t, srv.Listener.Addr().String(), token)
	defer func() { _ = c.Close() }()
	if !strings.Contains(status, "200") {
		t.Fatalf("post-release status = %q, want 200", strings.TrimSpace(status))
	}
}

// TestSpliceConnsTeardownOnEitherClose pins the spliceConns invariant directly:
// closing either leg returns the splice and closes the other leg.
func TestSpliceConnsTeardownOnEitherClose(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		spliceConns(ctx, cancel, a, b)
		close(done)
	}()

	_ = aPeer.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("spliceConns did not return within 5s of one leg closing")
	}

	_ = bPeer.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := bPeer.Write([]byte("z")); err == nil {
		t.Error("upstream leg still open after teardown; spliceConns must close both legs")
	}
}

// TestSessionCredIsNotAnX509Identity proves the trust isolation between the
// ingress-session credential and the mesh mTLS identities: the credential is a
// plain bearer string with no x509 form, so it can never be presented as a client
// certificate to RequireCPIdentity or any cluster-CA mTLS gate; and the session
// CA that signs it is a distinct key from the cluster CA, so a leaked credential
// never widens mesh trust. (The router-level proof that a bearer at a CP route is
// rejected lives in TestGatewayRouterTrustBoundaries.)
func TestSessionCredIsNotAnX509Identity(t *testing.T) {
	signer, _ := newTestSessionCA(t)
	token := signCred(t, signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: netip.MustParseAddr("10.42.0.5"),
		Port: 22, ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	if !auth.IsIngressCredFormat(token) {
		t.Fatal("token is not in ingress-credential format")
	}
	// The bearer is not a certificate: no mTLS gate could ever accept it as a
	// client identity, because it is not an x509 object at all.
	if _, err := x509.ParseCertificate([]byte(token)); err == nil {
		t.Error("ingress credential parsed as an x509 certificate; it must not be a certificate")
	}

	// The session CA key is distinct from the cluster CA key.
	clusterCA, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("generate cluster ca: %v", err)
	}
	clusterCert, _, err := auth.ParseClusterCACert(clusterCA.CertPEM)
	if err != nil {
		t.Fatalf("parse cluster ca cert: %v", err)
	}
	sessionPub, ok := signer.Public().(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("session ca public key is %T, want *ecdsa.PublicKey", signer.Public())
	}
	clusterPub, ok := clusterCert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("cluster ca public key is %T, want *ecdsa.PublicKey", clusterCert.PublicKey)
	}
	if sessionPub.Equal(clusterPub) {
		t.Error("session CA public key equals the cluster CA public key; they must be distinct")
	}
}

// failDial is a dial seam that fails the test if it is ever called: the
// anti-SSRF refusal paths must reject before any dial.
func failDial(t *testing.T) dialFunc {
	t.Helper()
	return func(context.Context, string, string, string) (net.Conn, error) {
		t.Error("dial called on a path that must refuse before dialing")
		return nil, io.EOF
	}
}

// corruptCredBody flips one base64url character in the middle of a session
// credential so it can no longer verify. Flipping the LAST character is
// unreliable: an ECDSA signature's byte length varies per signing, so the low
// bits the final base64 character encodes can fall beyond the signature's real
// bytes and be dropped on decode, leaving the decoded credential unchanged. A
// middle character always maps to significant bytes (payload or signature), so
// the flip reliably breaks verification.
func corruptCredBody(s string) string {
	b := []byte(s)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}
