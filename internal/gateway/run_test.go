// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
)

// nilConnectDeps is the connect wiring for router tests that never exercise the
// connect route. newConnectHandler only stores the deps, so nil collaborators
// are safe here.
func nilConnectDeps() connectDeps {
	return connectDeps{caStore: newSessionCAStore(discardLogger())}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildReconcilersWiring confirms the gateway assembly builds exactly a
// WireGuard reconciler and a gateway-mode network reconciler — and nothing
// VM-shaped. The gatewayReconcilers type carries no VM or pool reconciler,
// so the absence of a VM manager is structural; this test pins the live
// fields and the gateway mode.
func TestBuildReconcilersWiring(t *testing.T) {
	cfg := &config.AgentConfig{}
	cfg.WireGuard.PrivateKeyPath = filepath.Join(t.TempDir(), "wg.key")

	recs, err := buildReconcilers(cfg, netfabric.New(), discardLogger())
	if err != nil {
		t.Fatalf("buildReconcilers: %v", err)
	}
	if recs.wireGuard == nil {
		t.Error("wireGuard reconciler is nil; gateway must join the WireGuard mesh")
	}
	if recs.networks == nil {
		t.Fatal("networks reconciler is nil; gateway must bring up overlays")
	}
	if !recs.networks.GatewayMode() {
		t.Error("networks reconciler is not in gateway mode; the services plane would not be stripped")
	}
}

// spyNudger records Nudge calls.
type spyNudger struct{ nudges int }

func (s *spyNudger) Nudge() { s.nudges++ }

// cpIdentityTLSState builds the TLS connection state a request carries after
// an mTLS handshake: a single verified peer cert whose Subject CN is cn.
func cpIdentityTLSState(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		},
	}
}

// TestNudgeRouteUnderCPIdentity drives the real gateway router and confirms
// POST /v1/heartbeat/nudge is gated by CP identity: the control plane reaches
// it and triggers a heartbeat (204 + recorded nudge), while a node cert
// (valid under the same cluster CA) is rejected with 403 before the handler
// runs, so it never nudges.
func TestNudgeRouteUnderCPIdentity(t *testing.T) {
	cfg := &config.AgentConfig{}
	cfg.Server.ReadTimeout = 5 * time.Second

	tests := []struct {
		name       string
		cn         string
		wantStatus int
		wantNudges int
	}{
		{name: "cp identity nudges", cn: auth.CPCertCommonName, wantStatus: http.StatusNoContent, wantNudges: 1},
		{name: "node identity rejected", cn: "node-evil", wantStatus: http.StatusForbidden, wantNudges: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyNudger{}
			handler := buildRouter(cfg, "edge1", discardLogger(), spy, nilConnectDeps())

			req := httptest.NewRequest(http.MethodPost, "/v1/heartbeat/nudge", nil)
			req.TLS = cpIdentityTLSState(tt.cn)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("POST /v1/heartbeat/nudge (cn=%s) status = %d, want %d", tt.cn, rec.Code, tt.wantStatus)
			}
			if spy.nudges != tt.wantNudges {
				t.Errorf("nudges = %d, want %d", spy.nudges, tt.wantNudges)
			}
		})
	}
}

// TestGatewayRouterTrustBoundaries is the load-bearing non-regression test for
// the listener decision. After the inbound listener was lowered from
// RequireAndVerifyClientCert to VerifyClientCertIfGiven (so the certificate-less
// connect client can complete the handshake), the CP-only control routes MUST
// still fail closed on a missing or non-CP client certificate - lowering the
// listener must NOT open them. It also proves the connect route is gated by the
// session-credential bearer, not by a client certificate: a valid bearer with no
// client cert passes the gate, while an absent bearer is rejected 401.
func TestGatewayRouterTrustBoundaries(t *testing.T) {
	cfg := &config.AgentConfig{}
	cfg.Server.ReadTimeout = 5 * time.Second

	// Arm the connect route's deps with a real session CA and a neighbor table
	// that resolves the credential, so a valid bearer reaches the splice handler
	// (and a ResponseRecorder, which is not a Hijacker, then yields 500 - proving
	// the gate accepted the bearer).
	mat, err := auth.GenerateSessionCA()
	if err != nil {
		t.Fatalf("generate session ca: %v", err)
	}
	signer, err := auth.ParseSessionCASigner(mat.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("parse signer: %v", err)
	}
	ip := netip.MustParseAddr("10.42.0.5")
	bridge := "otvb100"
	hw, _ := net.ParseMAC("02:00:00:00:00:0a")
	pubPEM := string(mat.PublicKeyPEM)
	store := newSessionCAStore(discardLogger())
	store.HandleHeartbeatResponse(t.Context(), &heartbeat.Response{SessionCAPublicPEM: &pubPEM})
	deps := connectDeps{
		fabric: &netfabric.FakeFabric{NeighborResult: map[string]netfabric.NeighborOutcome{
			netfabric.NeighborKey(bridge, ip): {MAC: hw, OK: true},
		}},
		overlays: fakeOverlays{bridge: bridge, ok: true},
		caStore:  store,
	}
	validBearer, err := auth.SignSessionCred(signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: "02:00:00:00:00:0a", GuestIP: ip, Port: 22,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}, time.Now())
	if err != nil {
		t.Fatalf("sign cred: %v", err)
	}

	handler := buildRouter(cfg, "edge1", discardLogger(), &spyNudger{}, deps)

	tests := []struct {
		name       string
		method     string
		path       string
		tls        *tls.ConnectionState // nil = no client cert presented
		bearer     string
		wantStatus int
	}{
		// Control routes stay authoritative-gated by CP identity after the
		// ClientAuth lowering: no cert and a non-CP cert are both rejected 403.
		{name: "health no client cert rejected", method: http.MethodGet, path: "/health", wantStatus: http.StatusForbidden},
		{name: "health bearer is not an identity", method: http.MethodGet, path: "/health", bearer: validBearer, wantStatus: http.StatusForbidden},
		{name: "nudge no client cert rejected", method: http.MethodPost, path: "/v1/heartbeat/nudge", wantStatus: http.StatusForbidden},
		{name: "nudge non-cp cert rejected", method: http.MethodPost, path: "/v1/heartbeat/nudge", tls: cpIdentityTLSState("node-evil"), wantStatus: http.StatusForbidden},
		// Connect is bearer-gated: a valid bearer with no client cert passes the
		// gate (reaching the splice handler -> 500 on a non-hijackable recorder),
		// while an absent bearer is rejected 401 by the credential gate.
		{name: "connect valid bearer passes gate", method: http.MethodPost, path: "/v1/connect", bearer: validBearer, wantStatus: http.StatusInternalServerError},
		{name: "connect absent bearer rejected", method: http.MethodPost, path: "/v1/connect", wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.TLS = tt.tls
			if tt.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s status = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
			}
		})
	}
}

// TestLoadTLSClientAuth pins the listener decision at the TLS layer: the gateway
// verifies a client cert if one is presented but does not require one, while
// keeping the cluster CA pool so a presented cert is still verified.
func TestLoadTLSClientAuth(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "gw.crt")
	keyPath := filepath.Join(dir, "gw.key")
	caPath := filepath.Join(dir, "ca.crt")
	writeSelfSignedCert(t, certPath, keyPath)
	// Reuse the same cert as a stand-in cluster CA pool source.
	caPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert for ca: %v", err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	cfg, err := loadTLS(config.TLSConfig{CertPath: certPath, KeyPath: keyPath, CACertPath: caPath})
	if err != nil {
		t.Fatalf("loadTLS: %v", err)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v, want VerifyClientCertIfGiven", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil; a presented client cert would not be verified")
	}
}

// writeSelfSignedCert writes a throwaway ECDSA self-signed leaf cert+key to the
// given paths, for loadTLS to load.
func writeSelfSignedCert(t *testing.T, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "otherix-gateway-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = certOut.Close()
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = keyOut.Close()
}
