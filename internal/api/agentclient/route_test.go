// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/config"
)

func TestDialURL(t *testing.T) {
	t.Parallel()
	got := agentclient.DialURL("foo")
	want := "https://node-foo.agents.otherix.local"
	if got != want {
		t.Errorf("DialURL(%q) = %q, want %q", "foo", got, want)
	}
}

// stubResolver returns a fixed route for any name.
type stubResolver struct{ route agentclient.AgentRoute }

func (s stubResolver) Resolve(string) (agentclient.AgentRoute, error) { return s.route, nil }

// TestGeoDial_DirectPinsIdentityServerName proves the direct route dials the
// resolved address while the agent TLS ServerName is the identity URL host
// (NodeIdentitySAN), not the dialed IP - the identity pin, for free.
func TestGeoDial_DirectPinsIdentityServerName(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	var gotSNI atomic.Value
	agentSrv := startTLSServer(t, ca, "node-agent.agents.otherix.local", &gotSNI,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))

	cli := routedClient(t, ca, stubResolver{route: agentclient.AgentRoute{Direct: hostPort(agentSrv.URL)}})

	resp, err := cli.HTTPClient().Get(agentclient.DialURL("agent") + "/probe")
	if err != nil {
		t.Fatalf("GET via direct route: %v", err)
	}
	_ = resp.Body.Close()

	if got := gotSNI.Load(); got != "node-agent.agents.otherix.local" {
		t.Errorf("agent observed ServerName = %v, want the identity SAN node-agent.agents.otherix.local", got)
	}
}

// TestGeoDial_ViaGatewayTwoTLS is the load-bearing proof: a NAT'd node reached
// through a gateway CONNECT splice completes a TWO-TLS-layer tunnel (outer CP->gw
// TLS, inner CP->agent TLS) and an agent request round-trips. It also pins the
// exact /v1/connect-node request shape the gateway's splice handler must accept.
func TestGeoDial_ViaGatewayTwoTLS(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)

	// Inner agent: a real mTLS server presenting the node identity leaf.
	var agentSNI atomic.Value
	agentSrv := startTLSServer(t, ca, "node-agent.agents.otherix.local", &agentSNI,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"agent":"reached"}`))
		}))
	agentAddr := hostPort(agentSrv.URL)

	// Outer gateway: an mTLS server that validates the CONNECT body, hijacks, and
	// splices raw bytes to the agent - it never terminates the inner TLS.
	var gotConnectBody atomic.Value
	gwMux := http.NewServeMux()
	gwMux.HandleFunc("/v1/connect-node", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotConnectBody.Store(string(raw))
		var body struct {
			OverlayIP string `json:"overlay_ip"`
			Port      int    `json:"port"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.OverlayIP == "" || body.Port == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstream, err := net.Dial("tcp", net.JoinHostPort(body.OverlayIP, strconv.Itoa(body.Port)))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
	})
	gwSrv := startTLSMux(t, ca, "node-gw.agents.otherix.local", gwMux)

	cli := routedClient(t, ca, stubResolver{route: agentclient.AgentRoute{
		GatewayDial:   hostPort(gwSrv.URL),
		GatewaySAN:    "node-gw.agents.otherix.local",
		TargetOverlay: agentAddr,
	}})

	resp, err := cli.HTTPClient().Get(agentclient.DialURL("agent") + "/probe")
	if err != nil {
		t.Fatalf("GET via gateway splice: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != `{"agent":"reached"}` {
		t.Errorf("tunnelled agent body = %q, want the agent's response", got)
	}

	// Inner TLS ServerName reached the agent as the identity SAN (not the gw).
	if sni := agentSNI.Load(); sni != "node-agent.agents.otherix.local" {
		t.Errorf("inner ServerName = %v, want node-agent.agents.otherix.local", sni)
	}

	// The CONNECT body is exactly {"overlay_ip":"<host>","port":<port>}.
	host, portStr, _ := net.SplitHostPort(agentAddr)
	wantBody := `{"overlay_ip":"` + host + `","port":` + portStr + `}`
	if body := gotConnectBody.Load(); body != wantBody {
		t.Errorf("connect-node body = %v, want %q", body, wantBody)
	}
}

// TestGeoDial_GatewayErrorClosesConn proves the close-on-error seam: when the
// connect-node exchange fails (here a non-200 from the gateway), the CP closes
// the outer conn rather than leaking it. The gateway observes that close as EOF,
// so there is no fd/goroutine leak on the failure path. (The time bound on a
// stalling gateway is proven at the connectNode level in route_internal_test.go,
// where the ctx deadline is not stripped by the transport.)
func TestGeoDial_GatewayErrorClosesConn(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)

	connClosed := make(chan struct{})
	gwMux := http.NewServeMux()
	gwMux.HandleFunc("/v1/connect-node", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // consume the CONNECT body
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// Reject the splice: a non-200 status fails connectNode. The CP must then
		// close the outer conn, observed here as EOF once io.Copy returns.
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		close(connClosed)
	})
	gwSrv := startTLSMux(t, ca, "node-gw.agents.otherix.local", gwMux)

	cli := routedClient(t, ca, stubResolver{route: agentclient.AgentRoute{
		GatewayDial:   hostPort(gwSrv.URL),
		GatewaySAN:    "node-gw.agents.otherix.local",
		TargetOverlay: "10.0.0.5:9443",
	}})

	resp, err := cli.HTTPClient().Get(agentclient.DialURL("agent") + "/probe")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Get via rejecting gateway = nil error, want a dial failure on non-200 connect-node")
	}

	select {
	case <-connClosed:
	case <-time.After(3 * time.Second):
		t.Error("gateway conn was never closed after connect-node failed - fd/goroutine leak")
	}
}

// --- test scaffolding: a throwaway CA + leaves and TLS servers -------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca keygen: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "route-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca create: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ca parse: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leaf signs a server+client-auth leaf carrying dnsSANs (its first entry is also
// the Subject CN).
func (ca *testCA) leaf(t *testing.T, dnsSANs ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf keygen: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("leaf serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsSANs[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsSANs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("leaf create: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("leaf parse: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}
}

// startTLSServer starts an mTLS server presenting the identitySAN leaf and, when
// sni is non-nil, records the ServerName each client offers.
func startTLSServer(t *testing.T, ca *testCA, identitySAN string, sni *atomic.Value, h http.Handler) *httptest.Server {
	t.Helper()
	leaf := ca.leaf(t, identitySAN)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool,
		MinVersion:   tls.VersionTLS13,
	}
	if sni != nil {
		srv.TLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			sni.Store(chi.ServerName)
			return nil, nil
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func startTLSMux(t *testing.T, ca *testCA, identitySAN string, h http.Handler) *httptest.Server {
	return startTLSServer(t, ca, identitySAN, nil, h)
}

// routedClient builds a *agentclient.Client whose CP material is signed by ca and
// whose transport routes through res. Timeouts are generous so a two-TLS tunnel
// completes under the race detector.
func routedClient(t *testing.T, ca *testCA, res agentclient.RouteResolver) *agentclient.Client {
	t.Helper()
	cli, err := agentclient.New(config.AgentClientConfig{
		Enabled:         true,
		Timeout:         5 * time.Second,
		PollInterval:    10 * time.Millisecond,
		PollMaxInterval: 5 * time.Second,
	}, ca.leaf(t, "otherix-cp-replica"), ca.cert, res)
	if err != nil {
		t.Fatalf("agentclient.New: %v", err)
	}
	return cli
}

func hostPort(rawURL string) string {
	return strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
}
