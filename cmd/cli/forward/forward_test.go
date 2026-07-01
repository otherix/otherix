// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package forward

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/otherix/otherix/cmd/internal/sshconn"
)

// certPEM renders an x509 certificate to its PEM bundle form so a test can hand
// the connector the stub server's own certificate as the cluster CA bundle.
func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// fakeGateway stubs the gateway's POST /v1/connect: it records the bearer the
// client presents, finishes the hijack handshake (200 then raw bytes), and
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

// TestForwardBrokersAndSplicesGateway drives the full forward path against a stub
// Control Plane that brokers a gateway transport and a stub gateway: a byte
// written to the local listener round-trips through the brokered gateway
// connection, and the gateway sees the brokered session credential as its bearer.
func TestForwardBrokersAndSplicesGateway(t *testing.T) {
	gw := &fakeGateway{}
	gwTS := httptest.NewTLSServer(gw.handler())
	defer gwTS.Close()
	// The broker reports the gateway's advertised endpoint as a full https URL,
	// so the client derives the dial host:port and the TLS ServerName from it.
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

	cfg := sshconn.Config{
		ServerURL:   cpTS.URL,
		BearerToken: "otx_clitoken",
		CACertPEM:   certPEM(cpTS.Certificate()),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = forwardLoop(ctx, cfg, "vm1", 22, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial local listener: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.(*net.TCPConn).CloseWrite()

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("round-tripped bytes = %q, want %q", got, "ping")
	}

	gw.mu.Lock()
	bearer := gw.bearer
	gw.mu.Unlock()
	if bearer != "Bearer otx_ingress_faketoken" {
		t.Errorf("gateway bearer = %q, want %q", bearer, "Bearer otx_ingress_faketoken")
	}
}
