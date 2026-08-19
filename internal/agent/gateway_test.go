// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
)

// gatewayCerts holds the on-disk paths a gateway RunGateway needs plus the
// cluster CA the test dials against.
type gatewayCerts struct {
	caPath   string
	certPath string
	keyPath  string
	caPool   *x509.CertPool
	caKey    *ecdsa.PrivateKey
	caCert   *x509.Certificate
}

// newGatewayCerts mints a throwaway cluster CA and a node-<name> gateway leaf
// signed by it (with localhost / 127.0.0.1 SANs so a real TLS dial verifies),
// and writes ca.crt / gw.crt / gw.key under a temp dir.
func newGatewayCerts(t *testing.T, nodeName string) gatewayCerts {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "otherix-cluster-ca-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}

	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, caPath, "CERTIFICATE", caDER)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "node-" + nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPath := filepath.Join(dir, "gw.crt")
	writePEM(t, certPath, "CERTIFICATE", leafDER)
	keyPath := filepath.Join(dir, "gw.key")
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", leafKeyDER)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return gatewayCerts{
		caPath:   caPath,
		certPath: certPath,
		keyPath:  keyPath,
		caPool:   pool,
		caKey:    caKey,
		caCert:   caCert,
	}
}

// clientCert mints a client leaf with the given CN signed by the test cluster
// CA, for presenting to the control listener.
func (g gatewayCerts) clientCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, g.caCert, &key.PublicKey, g.caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// freeAddr grabs a free loopback TCP address by binding :0 and releasing it. A
// brief race window exists before RunGateway rebinds, acceptable in a test.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// gatewayTestConfig builds a runnable gateway agent config over the given
// listen addresses and cert material.
func gatewayTestConfig(t *testing.T, certs gatewayCerts, controlListen, ingressListen string) *config.AgentConfig {
	t.Helper()
	cfg := &config.AgentConfig{}
	cfg.Server.Listen = controlListen
	cfg.Server.ReadTimeout = 5 * time.Second
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.ShutdownGrace = 5 * time.Second
	cfg.TLS = config.TLSConfig{CACertPath: certs.caPath, CertPath: certs.certPath, KeyPath: certs.keyPath}
	cfg.WireGuard.PrivateKeyPath = filepath.Join(t.TempDir(), "wg.key")
	cfg.Gateway = config.GatewayConfig{Enabled: true, Listen: ingressListen}
	// ControlPlane.URL left empty: heartbeat stays disabled in the test.
	return cfg
}

// waitForTLS blocks until a TLS dial to addr succeeds (server is up) or the
// deadline elapses.
func waitForTLS(t *testing.T, addr string, tlsCfg *tls.Config) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, tlsCfg)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not come up within deadline", addr)
}

// TestRunGatewayTwoListeners stands up RunGateway with real listeners and pins
// the two-plane trust boundary: the control listener requires and CP-gates a
// client cert, the ingress listener accepts a certless bearer client, and the
// two route surfaces are disjoint.
func TestRunGatewayTwoListeners(t *testing.T) {
	certs := newGatewayCerts(t, "edge1")
	controlListen := freeAddr(t)
	ingressListen := freeAddr(t)
	cfg := gatewayTestConfig(t, certs, controlListen, ingressListen)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- RunGateway(ctx, cfg, discardLogger()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("RunGateway did not return within deadline after cancel")
		}
	})

	// Bootstrap-verify both servers are up. The ingress listener is dialed
	// certless (VerifyClientCertIfGiven), the control listener with a CP cert.
	cpCert := certs.clientCert(t, auth.CPCertCommonName)
	waitForTLS(t, ingressListen, &tls.Config{RootCAs: certs.caPool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	waitForTLS(t, controlListen, &tls.Config{RootCAs: certs.caPool, ServerName: "localhost", MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cpCert}})

	t.Run("control rejects a no-cert client at handshake", func(t *testing.T) {
		client := httpsClient(certs.caPool, nil)
		_, err := client.Get("https://" + controlListen + "/health")
		if err == nil {
			t.Error("control /health with no client cert: got nil error, want mTLS handshake failure")
		}
	})

	t.Run("control rejects a non-CP cert with 403", func(t *testing.T) {
		nodeCert := certs.clientCert(t, "node-evil")
		client := httpsClient(certs.caPool, &nodeCert)
		resp, err := client.Get("https://" + controlListen + "/health")
		if err != nil {
			t.Fatalf("control /health with node cert: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("control /health (node cert) status = %d, want 403", resp.StatusCode)
		}

		// The highest-consequence route (raw-byte splice to another node) must sit
		// behind the same CP-identity gate: a node leaf is refused before routing,
		// so it never reaches the hijacking handler.
		connResp, err := client.Post("https://"+controlListen+"/v1/connect-node", "application/json", nil)
		if err != nil {
			t.Fatalf("control POST /v1/connect-node with node cert: %v", err)
		}
		defer connResp.Body.Close()
		if connResp.StatusCode != http.StatusForbidden {
			t.Errorf("control POST /v1/connect-node (node cert) status = %d, want 403", connResp.StatusCode)
		}
	})

	t.Run("control serves health to CP identity", func(t *testing.T) {
		client := httpsClient(certs.caPool, &cpCert)
		resp, err := client.Get("https://" + controlListen + "/health")
		if err != nil {
			t.Fatalf("control /health with cp cert: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("control /health (cp cert) status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("ingress accepts certless connect and 401s a missing credential", func(t *testing.T) {
		client := httpsClient(certs.caPool, nil)
		resp, err := client.Post("https://"+ingressListen+"/v1/connect", "application/octet-stream", nil)
		if err != nil {
			t.Fatalf("ingress /v1/connect certless: %v (handshake should have succeeded)", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("ingress /v1/connect (no bearer) status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("planes are disjoint", func(t *testing.T) {
		// Ingress does not serve the control routes.
		ingressClient := httpsClient(certs.caPool, nil)
		for _, path := range []string{"/health", "/v1/heartbeat/nudge"} {
			resp, err := ingressClient.Get("https://" + ingressListen + path)
			if err != nil {
				t.Fatalf("ingress GET %s: %v", path, err)
			}
			got := resp.StatusCode
			resp.Body.Close()
			if got != http.StatusNotFound {
				t.Errorf("ingress GET %s status = %d, want 404 (control route must not be on ingress)", path, got)
			}
		}
		// The node-connect splice is a control-plane route: it must not live on the
		// ingress (bearer) plane, even by method.
		connResp, err := ingressClient.Post("https://"+ingressListen+"/v1/connect-node", "application/json", nil)
		if err != nil {
			t.Fatalf("ingress POST /v1/connect-node: %v", err)
		}
		connGot := connResp.StatusCode
		connResp.Body.Close()
		if connGot != http.StatusNotFound {
			t.Errorf("ingress POST /v1/connect-node status = %d, want 404 (control route must not be on ingress)", connGot)
		}
		// Control does not serve the connect route.
		controlClient := httpsClient(certs.caPool, &cpCert)
		resp, err := controlClient.Post("https://"+controlListen+"/v1/connect", "application/octet-stream", nil)
		if err != nil {
			t.Fatalf("control POST /v1/connect: %v", err)
		}
		got := resp.StatusCode
		resp.Body.Close()
		if got != http.StatusNotFound {
			t.Errorf("control POST /v1/connect status = %d, want 404 (connect route must not be on control)", got)
		}
	})
}

// TestRunGatewayReturnsNilOnCancel confirms a clean context cancel shuts both
// listeners down and RunGateway returns nil without hanging.
func TestRunGatewayReturnsNilOnCancel(t *testing.T) {
	certs := newGatewayCerts(t, "edge1")
	cfg := gatewayTestConfig(t, certs, freeAddr(t), freeAddr(t))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- RunGateway(ctx, cfg, discardLogger()) }()

	// Let both listeners bind, then cancel.
	waitForTLS(t, cfg.Gateway.Listen, &tls.Config{RootCAs: certs.caPool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("RunGateway on cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunGateway did not return within deadline after cancel (hang)")
	}
}

// TestRunGatewayErrorsWhenIngressCannotBind confirms a failed ingress bind
// returns an error (not a hang) and tears down the already-up control listener.
func TestRunGatewayErrorsWhenIngressCannotBind(t *testing.T) {
	certs := newGatewayCerts(t, "edge1")

	// Occupy a port and point the ingress listener at it so its bind fails.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer occupied.Close()
	ingressListen := occupied.Addr().String()
	controlListen := freeAddr(t)

	cfg := gatewayTestConfig(t, certs, controlListen, ingressListen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- RunGateway(ctx, cfg, discardLogger()) }()

	select {
	case err := <-runErr:
		if err == nil {
			t.Error("RunGateway with unbindable ingress = nil, want error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunGateway did not return within deadline on ingress bind failure (hang)")
	}

	// The control listener must be torn down: a fresh bind to its address
	// should now succeed.
	l, err := net.Listen("tcp", controlListen)
	if err != nil {
		t.Errorf("control listener still bound after teardown: %v", err)
		return
	}
	_ = l.Close()
}

// httpsClient builds an HTTPS client trusting caPool and optionally presenting
// clientCert. ServerName is pinned to localhost to match the leaf SAN.
func httpsClient(caPool *x509.CertPool, clientCert *tls.Certificate) *http.Client {
	tlsCfg := &tls.Config{
		RootCAs:    caPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}
	if clientCert != nil {
		tlsCfg.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
}
