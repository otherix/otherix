// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshconn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// certMintServer is a stub of POST /v1/vms/{vm}/ssh-cert. It signs the
// posted public key with a throwaway CA and records every request so the
// caching/refresh behaviour can be asserted.
type certMintServer struct {
	caSigner   ssh.Signer
	ttl        time.Duration
	requests   atomic.Int64
	lastBearer string
	lastPubKey string
	lastLogin  string
	mu         sync.Mutex
}

func newCertMintServer(t *testing.T, ttl time.Duration) *certMintServer {
	t.Helper()
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(caPriv)
	if err != nil {
		t.Fatalf("ca signer: %v", err)
	}
	return &certMintServer{caSigner: signer, ttl: ttl}
}

func (s *certMintServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		var body struct {
			PublicKey string `json:"public_key"`
			Login     string `json:"login"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		userPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body.PublicKey))
		if err != nil {
			http.Error(w, "bad pubkey", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.lastBearer = r.Header.Get("Authorization")
		s.lastPubKey = body.PublicKey
		s.lastLogin = body.Login
		s.mu.Unlock()

		now := time.Now()
		login := body.Login
		if login == "" {
			login = "root"
		}
		cert := &ssh.Certificate{
			Key:             userPub,
			CertType:        ssh.UserCert,
			KeyId:           "user:test",
			ValidPrincipals: []string{login},
			ValidAfter:      uint64(now.Add(-30 * time.Second).Unix()),
			ValidBefore:     uint64(now.Add(s.ttl).Unix()),
		}
		if err := cert.SignCert(rand.Reader, s.caSigner); err != nil {
			http.Error(w, "sign", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"certificate": string(ssh.MarshalAuthorizedKey(cert)),
			"login":       login,
			"expires_at":  now.Add(s.ttl).UTC().Format(time.RFC3339),
		})
	}
}

// fingerprintOf returns the lowercase hex sha256 of a TLS server's leaf cert.
func fingerprintOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	cert := ts.Certificate()
	if cert == nil {
		t.Fatal("test server has no leaf certificate")
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func TestEnsureGuestCertMintsAndCaches(t *testing.T) {
	stub := newCertMintServer(t, 5*time.Minute)
	ts := httptest.NewTLSServer(stub.handler())
	defer ts.Close()

	dir := t.TempDir()
	cfg := Config{
		ServerURL:     ts.URL,
		CAFingerprint: fingerprintOf(t, ts),
		BearerToken:   "otx_secrettoken",
		KnownDir:      dir,
	}

	certPath, keyPath, err := EnsureGuestCert(context.Background(), cfg, "vm1", "alice")
	if err != nil {
		t.Fatalf("EnsureGuestCert: %v", err)
	}
	if certPath == "" || keyPath == "" {
		t.Fatalf("empty paths: cert=%q key=%q", certPath, keyPath)
	}

	// Bearer + public key reached the server, and nothing leaked the key file
	// over the wire (we send the public key only).
	stub.mu.Lock()
	gotBearer, gotPub, gotLogin := stub.lastBearer, stub.lastPubKey, stub.lastLogin
	stub.mu.Unlock()
	if want := "Bearer otx_secrettoken"; gotBearer != want {
		t.Errorf("bearer = %q, want %q", gotBearer, want)
	}
	if !strings.HasPrefix(gotPub, "ssh-ed25519 ") {
		t.Errorf("posted public key = %q, want ssh-ed25519 line", gotPub)
	}
	if gotLogin != "alice" {
		t.Errorf("login = %q, want alice", gotLogin)
	}

	// The cert file parses as a user cert for the requested principal.
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(certBytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		t.Fatalf("cert file is not an ssh certificate")
	}
	if got := cert.ValidPrincipals; len(got) != 1 || got[0] != "alice" {
		t.Errorf("principals = %v, want [alice]", got)
	}

	// The private key never goes over the wire: it must equal a private-key
	// PEM on disk and the posted material must be the public half only.
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if _, err := ssh.ParseRawPrivateKey(keyBytes); err != nil {
		t.Fatalf("private key not parseable: %v", err)
	}
	if strings.Contains(gotPub, "PRIVATE") {
		t.Errorf("private key material leaked into the cert request")
	}

	// A second call within validity is served from cache: no new mint.
	if _, _, err := EnsureGuestCert(context.Background(), cfg, "vm1", "alice"); err != nil {
		t.Fatalf("EnsureGuestCert (cached): %v", err)
	}
	if got := stub.requests.Load(); got != 1 {
		t.Errorf("mint requests = %d, want 1 (second call should be cached)", got)
	}
}

func TestEnsureGuestCertRefreshesNearExpiry(t *testing.T) {
	// A short-lived cert (10s) sits inside the refresh window (~30s), so a
	// follow-up call re-mints rather than reusing the cached cert.
	stub := newCertMintServer(t, 10*time.Second)
	ts := httptest.NewTLSServer(stub.handler())
	defer ts.Close()

	dir := t.TempDir()
	cfg := Config{
		ServerURL:     ts.URL,
		CAFingerprint: fingerprintOf(t, ts),
		BearerToken:   "otx_token",
		KnownDir:      dir,
	}

	if _, _, err := EnsureGuestCert(context.Background(), cfg, "vm1", "root"); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if _, _, err := EnsureGuestCert(context.Background(), cfg, "vm1", "root"); err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if got := stub.requests.Load(); got != 2 {
		t.Errorf("mint requests = %d, want 2 (near-expiry cert must refresh)", got)
	}
}

func TestEnsureGuestCertRejectsFingerprintMismatch(t *testing.T) {
	stub := newCertMintServer(t, 5*time.Minute)
	ts := httptest.NewTLSServer(stub.handler())
	defer ts.Close()

	cfg := Config{
		ServerURL:     ts.URL,
		CAFingerprint: strings.Repeat("ab", 32), // not the server's leaf
		BearerToken:   "otx_token",
		KnownDir:      t.TempDir(),
	}
	if _, _, err := EnsureGuestCert(context.Background(), cfg, "vm1", "root"); err == nil {
		t.Fatal("EnsureGuestCert accepted a mismatched pinned fingerprint")
	}
}

func TestPinnedTLSConfigAcceptsAndRejects(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	good := fingerprintOf(t, ts)
	bad := strings.Repeat("cd", 32)

	// Matching fingerprint connects.
	cfgGood, err := wsHTTPClient(Config{CAFingerprint: good})
	if err != nil {
		t.Fatalf("wsHTTPClient good: %v", err)
	}
	resp, err := cfgGood.Get(ts.URL)
	if err != nil {
		t.Fatalf("pinned client rejected the matching leaf: %v", err)
	}
	_ = resp.Body.Close()

	// Mismatched fingerprint is refused at the TLS layer.
	cfgBad, err := wsHTTPClient(Config{CAFingerprint: bad})
	if err != nil {
		t.Fatalf("wsHTTPClient bad: %v", err)
	}
	if _, err := cfgBad.Get(ts.URL); err == nil {
		t.Fatal("pinned client accepted a mismatched leaf fingerprint")
	}
}

// certPEM renders an x509 certificate to its PEM bundle form.
func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// foreignCAPEM builds a self-signed CA unrelated to the test server's leaf, so
// the reject case exercises a genuinely different trust anchor (httptest
// reuses one built-in localhost cert across servers, so a second server's cert
// would not differ).
func foreignCAPEM(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate foreign ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "foreign-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create foreign ca cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestEnsureGuestCertTrustsCABundle drives the real cert-mint round-trip over
// TLS and asserts the CACertPEM trust path: a CP leaf is accepted when the
// bundle is its signing CA (here the server's own self-signed root) and
// rejected when the bundle is a different CA, mirroring the credentialed CLI
// client's cluster-CA trust.
func TestEnsureGuestCertTrustsCABundle(t *testing.T) {
	stub := newCertMintServer(t, 5*time.Minute)
	ts := httptest.NewTLSServer(stub.handler())
	defer ts.Close()

	good := Config{
		ServerURL:   ts.URL,
		CACertPEM:   certPEM(ts.Certificate()),
		BearerToken: "otx_token",
		KnownDir:    t.TempDir(),
	}
	if _, _, err := EnsureGuestCert(context.Background(), good, "vm1", "root"); err != nil {
		t.Fatalf("EnsureGuestCert with the server's own CA bundle: %v", err)
	}

	bad := Config{
		ServerURL:   ts.URL,
		CACertPEM:   foreignCAPEM(t), // a different CA
		BearerToken: "otx_token",
		KnownDir:    t.TempDir(),
	}
	if _, _, err := EnsureGuestCert(context.Background(), bad, "vm1", "root"); err == nil {
		t.Fatal("EnsureGuestCert accepted a CP leaf signed by a different CA")
	}
}

// TestResolveTLSConfigPrecedence pins the documented trust order: an
// insecure-skip opt-out disables verification outright; a fingerprint wins
// over a CA bundle (the stricter single-cert pin); an unparseable CA bundle is
// a hard error rather than a silent fall-through to system roots.
func TestResolveTLSConfigPrecedence(t *testing.T) {
	t.Run("insecure skip disables verification", func(t *testing.T) {
		cfg, err := resolveTLSConfig(Config{
			InsecureSkipTLSVerify: true,
			CAFingerprint:         strings.Repeat("ab", 32),
			CACertPEM:             []byte("ignored"),
		})
		if err != nil {
			t.Fatalf("resolveTLSConfig: %v", err)
		}
		if !cfg.InsecureSkipVerify {
			t.Error("InsecureSkipTLSVerify did not set InsecureSkipVerify")
		}
		if cfg.VerifyConnection != nil {
			t.Error("insecure-skip path must not install a leaf-pin VerifyConnection")
		}
	})

	t.Run("fingerprint wins over CA bundle", func(t *testing.T) {
		cfg, err := resolveTLSConfig(Config{
			CAFingerprint: strings.Repeat("ab", 32),
			CACertPEM:     []byte("ignored"),
		})
		if err != nil {
			t.Fatalf("resolveTLSConfig: %v", err)
		}
		if cfg.VerifyConnection == nil || !cfg.InsecureSkipVerify {
			t.Error("a set fingerprint must take the leaf-pin path")
		}
		if cfg.RootCAs != nil {
			t.Error("CACertPEM must be ignored when a fingerprint is set")
		}
	})

	t.Run("unparseable CA bundle errors", func(t *testing.T) {
		if _, err := resolveTLSConfig(Config{CACertPEM: []byte("not a certificate")}); err == nil {
			t.Fatal("resolveTLSConfig accepted a CA bundle with no valid certificate")
		}
	})

	t.Run("empty trust uses system roots", func(t *testing.T) {
		cfg, err := resolveTLSConfig(Config{})
		if err != nil {
			t.Fatalf("resolveTLSConfig: %v", err)
		}
		if cfg.InsecureSkipVerify || cfg.RootCAs != nil || cfg.VerifyConnection != nil {
			t.Errorf("empty config must use system roots, got %+v", cfg)
		}
	})
}

func TestWriteSSHConfigBlockIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{KnownDir: dir}

	if err := WriteSSHConfigBlock(cfg, "cluster.example", "/usr/bin/otherix-ssh"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteSSHConfigBlock(cfg, "cluster.example", "/usr/bin/otherix-ssh"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	out := string(data)
	if n := strings.Count(out, "# >>> otherix-ssh cluster.example"); n != 1 {
		t.Errorf("open-marker count = %d, want 1 (block must not duplicate)", n)
	}
	if !strings.Contains(out, "Host *.cluster.example") {
		t.Errorf("missing wildcard Host line:\n%s", out)
	}
	if !strings.Contains(out, "ProxyCommand /usr/bin/otherix-ssh proxy %h %p") {
		t.Errorf("missing/incorrect ProxyCommand line:\n%s", out)
	}
}

func TestWriteSSHConfigBlockMultipleSuffixesCoexist(t *testing.T) {
	// Markers are suffix-keyed, so an operator with two clusters keeps one
	// managed block per suffix; a re-write of one suffix must not disturb the
	// other.
	dir := t.TempDir()
	cfg := Config{KnownDir: dir}
	if err := WriteSSHConfigBlock(cfg, "a.example", "/usr/bin/otherix-ssh"); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := WriteSSHConfigBlock(cfg, "b.example", "/usr/bin/otherix-ssh"); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := WriteSSHConfigBlock(cfg, "a.example", "/usr/bin/otherix-ssh"); err != nil {
		t.Fatalf("re-write a: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	out := string(data)
	if n := strings.Count(out, "# >>> otherix-ssh a.example"); n != 1 {
		t.Errorf("suffix a open-marker count = %d, want 1", n)
	}
	if n := strings.Count(out, "# >>> otherix-ssh b.example"); n != 1 {
		t.Errorf("suffix b open-marker count = %d, want 1", n)
	}
}

// relayStub is a minimal Control Plane stub for the relay transport: it brokers
// every ingress request as transport "relay" and serves the relay
// WebSocket as a one-shot echo. It records the brokered port and the ?port=N
// query the relay leg threads so a test can prove the real guest port reaches
// both legs.
type relayStub struct {
	mu              sync.Mutex
	brokeredPort    int
	streamPortQuery string
}

func (s *relayStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/vm1/ingress", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Port int `json:"port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.brokeredPort = body.Port
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"transport": "relay",
			"vm_id":     "11111111-1111-1111-1111-111111111111",
			"port":      body.Port,
		})
	})
	mux.HandleFunc("/v1/vms/vm1/relay", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.streamPortQuery = r.URL.Query().Get("port")
		s.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		typ, data, rerr := c.Read(r.Context())
		if rerr != nil {
			_ = c.Close(websocket.StatusInternalError, "")
			return
		}
		_ = c.Write(r.Context(), typ, data)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	return httptest.NewServer(mux)
}

func TestProxySplicesBytesBothWays(t *testing.T) {
	// Proxy brokers an ingress connection, the stub selects the relay transport,
	// and the one-shot echo WebSocket round-trips the spliced bytes.
	stub := &relayStub{}
	srv := stub.server(t)
	defer srv.Close()

	cfg := Config{
		ServerURL:   srv.URL,
		BearerToken: "otx_token",
	}

	clientToServer := "hello-guest\n"
	stdin := io.NopCloser(strings.NewReader(clientToServer))
	var stdout strings.Builder

	done := make(chan error, 1)
	go func() {
		done <- Proxy(context.Background(), cfg, "vm1", 22, stdin, &stdout)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Proxy did not return after stdin EOF")
	}

	if got := stdout.String(); got != clientToServer {
		t.Errorf("echoed bytes = %q, want %q", got, clientToServer)
	}
}

// TestProxyThreadsPortToRelay proves Proxy no longer drops the port: the real
// guest port reaches both the broker body and the relay leg's ?port=N query.
func TestProxyThreadsPortToRelay(t *testing.T) {
	stub := &relayStub{}
	srv := stub.server(t)
	defer srv.Close()

	cfg := Config{ServerURL: srv.URL, BearerToken: "otx_token"}

	const wantPort = 2222
	stdin := io.NopCloser(strings.NewReader("x"))
	var stdout strings.Builder

	done := make(chan error, 1)
	go func() {
		done <- Proxy(context.Background(), cfg, "vm1", wantPort, stdin, &stdout)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Proxy did not return")
	}

	stub.mu.Lock()
	gotBroker, gotStream := stub.brokeredPort, stub.streamPortQuery
	stub.mu.Unlock()
	if gotBroker != wantPort {
		t.Errorf("brokered port = %d, want %d", gotBroker, wantPort)
	}
	if gotStream != strconv.Itoa(wantPort) {
		t.Errorf("relay ?port = %q, want %q", gotStream, strconv.Itoa(wantPort))
	}
}

// fakeGateway is a stub of the gateway's POST /v1/connect: it records the bearer
// the client presented, completes the hijack handshake (200 then raw bytes), and
// echoes the spliced stream.
type fakeGateway struct {
	mu     sync.Mutex
	bearer string
}

func (g *fakeGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.bearer = r.Header.Get("Authorization")
		g.mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		_, _ = io.Copy(conn, conn)
	}
}

// TestDialIngressGatewayPresentsCred proves the gateway leg: DialIngress brokers
// a gateway transport, TLS-dials the returned splicer address, presents the
// session credential as the bearer, and returns a conn that splices bytes to the
// gateway.
func TestDialIngressGatewayPresentsCred(t *testing.T) {
	gw := &fakeGateway{}
	gwTS := httptest.NewTLSServer(gw.handler())
	defer gwTS.Close()
	// The broker reports the gateway's advertised endpoint as a full https URL,
	// so the client must derive the dial host:port and the TLS ServerName from it.
	splicer := "https://" + gwTS.Listener.Addr().String()

	cpTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Port int `json:"port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"transport":    "gateway",
			"vm_id":        "11111111-1111-1111-1111-111111111111",
			"port":         body.Port,
			"splicer_addr": splicer,
			"session_cred": "otx_ingress_faketoken",
			"expires_at":   time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
		})
	}))
	defer cpTS.Close()

	cfg := Config{
		ServerURL:   cpTS.URL,
		BearerToken: "otx_clitoken",
		CACertPEM:   certPEM(cpTS.Certificate()),
	}

	conn, err := DialIngress(context.Background(), cfg, "vm1", 22)
	if err != nil {
		t.Fatalf("DialIngress: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echoed bytes = %q, want %q", got, "ping")
	}

	gw.mu.Lock()
	bearer := gw.bearer
	gw.mu.Unlock()
	if bearer != "Bearer otx_ingress_faketoken" {
		t.Errorf("gateway bearer = %q, want %q", bearer, "Bearer otx_ingress_faketoken")
	}
}

// TestGatewayTLSConfigUsesRootCAsAndServerName pins the gateway leg's trust: it
// verifies against the cluster CA bundle with ServerName set to the splicer host,
// never InsecureSkipVerify.
func TestGatewayTLSConfigUsesRootCAsAndServerName(t *testing.T) {
	caPEM := foreignCAPEM(t)
	cfg, err := gatewayTLSConfig(Config{CACertPEM: caPEM}, "gw.example")
	if err != nil {
		t.Fatalf("gatewayTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("gateway leg must not skip TLS verification")
	}
	if cfg.RootCAs == nil {
		t.Error("gateway leg must verify against the cluster CA bundle")
	}
	if cfg.ServerName != "gw.example" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "gw.example")
	}
}
