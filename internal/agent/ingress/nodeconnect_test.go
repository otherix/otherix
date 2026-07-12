// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// failNodeDial is a node-connect dial seam that fails the test if it is ever
// called: every rejected target must be refused before any dial.
func failNodeDial(t *testing.T) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	return func(context.Context, string, string) (net.Conn, error) {
		t.Error("dial called on a target that must be refused before dialing")
		return nil, io.EOF
	}
}

// knownNodeConnectDeps builds deps whose only known-node overlay IP is 10.0.0.9
// and whose sole spliceable port is 9443, with a dial that must never fire.
func rejectingNodeConnectDeps(t *testing.T) NodeConnectDeps {
	return NodeConnectDeps{
		IsKnownNodeOverlayIP: func(ip netip.Addr) bool { return ip == netip.MustParseAddr("10.0.0.9") },
		ControlPort:          9443,
		dial:                 failNodeDial(t),
		Log:                  discardLogger(),
	}
}

// TestNodeConnect_RejectsNonNodeTarget is the load-bearing anti-SSRF test: only a
// known-NODE overlay IP on the control port may be spliced. A guest/anycast IP,
// an unknown IP, the wrong port, and an unparseable IP all fail closed with 403
// and NEVER reach the dial.
func TestNodeConnect_RejectsNonNodeTarget(t *testing.T) {
	h := NewNodeConnectHandler(rejectingNodeConnectDeps(t))

	cases := []struct {
		name string
		body string
	}{
		{"anycast service ip", `{"overlay_ip":"169.254.1.1","port":9443}`},
		{"unknown (guest vm) ip", `{"overlay_ip":"10.0.0.50","port":9443}`},
		{"known ip wrong port", `{"overlay_ip":"10.0.0.9","port":22}`},
		{"unparseable ip", `{"overlay_ip":"not-an-ip","port":9443}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			h.Connect(rw, httptest.NewRequest(http.MethodPost, "/v1/connect-node", strings.NewReader(tc.body)))
			if rw.Code != http.StatusForbidden {
				t.Errorf("target %s: status = %d, want 403", tc.body, rw.Code)
			}
		})
	}
}

// TestNodeConnect_RejectsMalformedBody proves a body that is not the expected
// JSON object fails closed (4xx) without dialing.
func TestNodeConnect_RejectsMalformedBody(t *testing.T) {
	h := NewNodeConnectHandler(rejectingNodeConnectDeps(t))
	rw := httptest.NewRecorder()
	h.Connect(rw, httptest.NewRequest(http.MethodPost, "/v1/connect-node", strings.NewReader("this is not json")))
	if rw.Code < 400 {
		t.Errorf("malformed body: status = %d, want a 4xx refusal", rw.Code)
	}
}

// TestNodeConnect_SplicesToKnownNode is the happy path: a known-node overlay IP
// on the control port yields 200 and pipes bytes through to the target. It also
// asserts the dial target is rebuilt from the validated (ip, port), never any
// other request input.
func TestNodeConnect_SplicesToKnownNode(t *testing.T) {
	echoAddr, _ := startEcho(t)

	dialedAddr := make(chan string, 1)
	h := NewNodeConnectHandler(NodeConnectDeps{
		IsKnownNodeOverlayIP: func(ip netip.Addr) bool { return ip == netip.MustParseAddr("10.0.0.9") },
		ControlPort:          9443,
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			select {
			case dialedAddr <- addr:
			default:
			}
			return (&net.Dialer{}).DialContext(ctx, network, echoAddr)
		},
		Log: discardLogger(),
	})

	srv := httptest.NewServer(http.HandlerFunc(h.Connect))
	t.Cleanup(srv.Close)

	c, br, status := rawNodeConnect(t, srv.Listener.Addr().String(), `{"overlay_ip":"10.0.0.9","port":9443}`)
	defer func() { _ = c.Close() }()
	if !strings.Contains(status, "200") {
		t.Fatalf("status = %q, want 200", strings.TrimSpace(status))
	}

	select {
	case got := <-dialedAddr:
		if got != "10.0.0.9:9443" {
			t.Errorf("dial target = %q, want 10.0.0.9:9443 (built from the validated ip+port)", got)
		}
	default:
		t.Fatal("dial never happened")
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

// rawNodeConnect opens a raw TCP connection to the hijacking connect-node handler,
// sends the POST carrying the JSON target body, consumes the status line + headers
// up to the blank line, and returns the connection, a reader positioned at the
// spliced byte stream, and the status line.
func rawNodeConnect(t *testing.T, srvAddr, body string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatalf("dial connect-node handler: %v", err)
	}
	req := "POST /v1/connect-node HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatalf("write connect-node request: %v", err)
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
