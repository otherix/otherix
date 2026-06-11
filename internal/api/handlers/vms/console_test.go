// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api/agentclient"
)

func TestDetectScheme(t *testing.T) {
	tests := []struct {
		name           string
		forwardedProto string
		tlsState       *tls.ConnectionState
		want           string
	}{
		{
			name: "no header no tls → http",
			want: "http",
		},
		{
			name:     "no header tls set → https",
			tlsState: &tls.ConnectionState{},
			want:     "https",
		},
		{
			name:           "forwarded https no tls → https",
			forwardedProto: "https",
			want:           "https",
		},
		{
			name:           "forwarded http with tls → http (header wins)",
			forwardedProto: "http",
			tlsState:       &tls.ConnectionState{},
			want:           "http",
		},
		{
			name:           "multi-hop https,http → https (leftmost)",
			forwardedProto: "https,http",
			want:           "https",
		},
		{
			name:           "multi-hop http, https → http (leftmost)",
			forwardedProto: "http, https",
			want:           "http",
		},
		{
			name:           "whitespace https → https",
			forwardedProto: "  https  ",
			want:           "https",
		},
		{
			name:           "uppercase HTTPS → https",
			forwardedProto: "HTTPS",
			want:           "https",
		},
		{
			name:           "invalid value falls back to tls",
			forwardedProto: "wss",
			tlsState:       &tls.ConnectionState{},
			want:           "https",
		},
		{
			name:           "invalid value no tls → http fallback",
			forwardedProto: "ftp",
			want:           "http",
		},
		{
			name:           "empty leftmost in chain falls back",
			forwardedProto: ",https",
			tlsState:       &tls.ConnectionState{},
			want:           "https",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/v1/vms/x/console", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.forwardedProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.forwardedProto)
			}
			req.TLS = tc.tlsState

			got := detectScheme(req)
			if got != tc.want {
				t.Errorf("detectScheme(%q, tls=%v) = %q, want %q",
					tc.forwardedProto, tc.tlsState != nil, got, tc.want)
			}
		})
	}
}

func TestHTTPSchemeToWebSocket(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"http", "ws"},
		{"https", "wss"},
		{"", "ws"},
		{"ftp", "ws"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := httpSchemeToWebSocket(tc.in)
			if got != tc.want {
				t.Errorf("httpSchemeToWebSocket(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildConsoleWebsocketURLProxyScheme(t *testing.T) {
	agentResp := agentclient.IssueConsoleTokenResponse{
		Token:         "tok-123",
		WebsocketPath: "/v1/vms/demo/console-stream",
	}

	tests := []struct {
		name           string
		host           string
		forwardedProto string
		tlsState       *tls.ConnectionState
		wantPrefix     string
	}{
		{
			name:       "plain http listener → ws://",
			host:       "localhost:8080",
			wantPrefix: "ws://localhost:8080/v1/vms/demo/console-stream?token=tok-123",
		},
		{
			name:       "tls listener → wss://",
			host:       "api.example.com",
			tlsState:   &tls.ConnectionState{},
			wantPrefix: "wss://api.example.com/v1/vms/demo/console-stream?token=tok-123",
		},
		{
			name:           "reverse proxy forwarded https → wss://",
			host:           "api.example.com",
			forwardedProto: "https",
			wantPrefix:     "wss://api.example.com/v1/vms/demo/console-stream?token=tok-123",
		},
		{
			name:           "multi-hop https,http → wss://",
			host:           "api.example.com",
			forwardedProto: "https,http",
			wantPrefix:     "wss://api.example.com/v1/vms/demo/console-stream?token=tok-123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/v1/vms/demo/console", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Host = tc.host
			if tc.forwardedProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.forwardedProto)
			}
			req.TLS = tc.tlsState

			got, err := buildConsoleWebsocketURL("proxy", req, "https://agent.example.com:9090", "demo", agentResp)
			if err != nil {
				t.Fatalf("buildConsoleWebsocketURL: %v", err)
			}
			if got != tc.wantPrefix {
				t.Errorf("buildConsoleWebsocketURL = %q, want %q", got, tc.wantPrefix)
			}
		})
	}
}

// TestBuildConsoleWebsocketURLEscapesToken drives a token that needs
// percent-encoding through both access modes and asserts it round-trips
// via net/url - a raw fmt interpolation would corrupt the `+` and `=`
// bytes on the wire.
func TestBuildConsoleWebsocketURLEscapesToken(t *testing.T) {
	const rawToken = "a b+c/d=="
	agentResp := agentclient.IssueConsoleTokenResponse{
		Token:         rawToken,
		WebsocketPath: "/v1/vms/demo/console-stream",
	}

	for _, mode := range []string{"proxy", "direct"} {
		t.Run(mode, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/v1/vms/demo/console", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Host = "api.example.com"

			got, err := buildConsoleWebsocketURL(mode, req, "https://agent.example.com:9090", "demo", agentResp)
			if err != nil {
				t.Fatalf("buildConsoleWebsocketURL: %v", err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got, err)
			}
			if tok := u.Query().Get("token"); tok != rawToken {
				t.Errorf("token round-trip = %q, want %q", tok, rawToken)
			}
		})
	}
}

// TestBuildConsoleWebsocketURLProxyEscapesVMName asserts the VM name is
// path-escaped in proxy mode (direct mode takes the agent-supplied
// WebsocketPath verbatim, so there is nothing to escape there).
func TestBuildConsoleWebsocketURLProxyEscapesVMName(t *testing.T) {
	agentResp := agentclient.IssueConsoleTokenResponse{Token: "tok-123"}
	req, err := http.NewRequest(http.MethodPost, "/v1/vms/demo/console", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "api.example.com"

	got, err := buildConsoleWebsocketURL("proxy", req, "https://agent.example.com:9090", "demo vm", agentResp)
	if err != nil {
		t.Fatalf("buildConsoleWebsocketURL: %v", err)
	}
	want := "ws://api.example.com/v1/vms/demo%20vm/console-stream?token=tok-123"
	if got != want {
		t.Errorf("buildConsoleWebsocketURL = %q, want %q", got, want)
	}
}

func TestBuildConsoleWebsocketURLDirectAlwaysWSS(t *testing.T) {
	agentResp := agentclient.IssueConsoleTokenResponse{
		Token:         "tok-direct",
		WebsocketPath: "/v1/vms/demo/console-stream",
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/vms/demo/console", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Even with plain HTTP to the CP, direct mode emits wss:// — agent
	// always serves HTTPS via mTLS.
	req.Host = "localhost:8080"

	got, err := buildConsoleWebsocketURL("direct", req, "https://agent.example.com:9090", "demo", agentResp)
	if err != nil {
		t.Fatalf("buildConsoleWebsocketURL: %v", err)
	}
	if !strings.HasPrefix(got, "wss://agent.example.com:9090/") {
		t.Errorf("direct mode URL = %q, want wss://agent.example.com:9090/...", got)
	}
}
