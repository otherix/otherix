// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
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
// the connect handler reaches the loopback echo server.
func netDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// rawConnect opens a raw TCP connection to the hijacking connect handler,
// sends the connect request, and consumes the status line + headers up to the
// blank line, leaving the returned reader positioned at the start of the
// spliced byte stream. It returns the connection, the buffered reader, and the
// status line.
func rawConnect(t *testing.T, srvAddr, query string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatalf("dial connect handler: %v", err)
	}
	if _, err := io.WriteString(c, "POST /v1/connect?"+query+" HTTP/1.1\r\nHost: gw\r\n\r\n"); err != nil {
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

// TestConnectSplicesBytesBothWays drives the real connect handler against a
// stub TCP echo target and proves a byte written by the client comes back
// echoed - the forward path carries traffic in both directions.
func TestConnectSplicesBytesBothWays(t *testing.T) {
	echoAddr, _ := startEcho(t)
	host, port, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}

	h := &connectHandler{dial: netDial, log: discardLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.Connect))
	defer srv.Close()

	c, br, status := rawConnect(t, srv.Listener.Addr().String(), "guest_ip="+host+"&port="+port)
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
// when the client side closes, the handler tears down the upstream leg too, so
// no goroutine or socket survives. The echo server's connection-ended channel
// firing after the client close is the teardown signal.
func TestConnectTearsDownOnClientClose(t *testing.T) {
	echoAddr, ended := startEcho(t)
	host, port, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}

	h := &connectHandler{dial: netDial, log: discardLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.Connect))
	defer srv.Close()

	c, br, status := rawConnect(t, srv.Listener.Addr().String(), "guest_ip="+host+"&port="+port)
	if !strings.Contains(status, "200") {
		t.Fatalf("connect status = %q, want a 200", strings.TrimSpace(status))
	}
	if _, err := io.WriteString(c, "x\n"); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	// Close the client leg; the handler must close the upstream leg in turn.
	_ = c.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream leg not torn down within 5s after client close")
	}
}

// TestConnectRejectsInvalidTarget proves the handler validates the target
// before hijacking: a missing or malformed guest_ip/port returns a normal
// 400 validation error rather than dialing or hijacking the connection.
func TestConnectRejectsInvalidTarget(t *testing.T) {
	h := &connectHandler{dial: netDial, log: discardLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.Connect))
	defer srv.Close()

	tests := []struct {
		name  string
		query string
	}{
		{name: "missing guest_ip", query: "port=9000"},
		{name: "missing port", query: "guest_ip=10.0.0.5"},
		{name: "guest_ip not an ip", query: "guest_ip=example.com&port=9000"},
		{name: "port not a number", query: "guest_ip=10.0.0.5&port=ssh"},
		{name: "port out of range", query: "guest_ip=10.0.0.5&port=70000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/v1/connect?"+tt.query, "", nil)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Code != "validation_failed" {
				t.Errorf("error code = %q, want validation_failed", body.Error.Code)
			}
		})
	}
}

// TestSpliceConnsTeardownOnEitherClose pins the spliceConns invariant directly:
// closing either leg returns the splice and closes the other leg, leaving no
// copy goroutine running.
func TestSpliceConnsTeardownOnEitherClose(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		spliceConns(ctx, cancel, a, b)
		close(done)
	}()

	// Close the peer of leg a so leg a sees EOF; the splice must tear down.
	_ = aPeer.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("spliceConns did not return within 5s of one leg closing")
	}

	// Leg b must have been closed by the splice: a write on its peer fails.
	_ = bPeer.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := bPeer.Write([]byte("z")); err == nil {
		t.Error("upstream leg still open after teardown; spliceConns must close both legs")
	}
}

// TestConnectRouteUnderCPIdentity drives the real gateway router and confirms
// POST /v1/connect is gated by CP identity exactly like the heartbeat nudge: a
// node cert (valid under the cluster CA) is rejected with 403 before the
// handler runs, while the control plane reaches the handler and is rejected
// only on the request itself (400 for a missing target).
func TestConnectRouteUnderCPIdentity(t *testing.T) {
	cfg := &config.AgentConfig{}
	cfg.Server.ReadTimeout = 5 * time.Second

	tests := []struct {
		name       string
		cn         string
		wantStatus int
	}{
		{name: "node identity rejected", cn: "node-evil", wantStatus: http.StatusForbidden},
		{name: "cp identity reaches handler", cn: auth.CPCertCommonName, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := buildRouter(cfg, "edge1", discardLogger(), &spyNudger{})
			// No guest_ip/port: a CP-identity caller reaches the handler and is
			// rejected 400; a node-identity caller is rejected 403 first.
			req := httptest.NewRequest(http.MethodPost, "/v1/connect", nil)
			req.TLS = cpIdentityTLSState(tt.cn)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("POST /v1/connect (cn=%s) status = %d, want %d", tt.cn, rec.Code, tt.wantStatus)
			}
		})
	}
}
