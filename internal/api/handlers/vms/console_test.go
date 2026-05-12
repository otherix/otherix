// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"crypto/tls"
	"net/http"
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
			name:           "invalid value falls back к tls",
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

func TestBuildConsoleWebsocketURLDirectAlwaysWSS(t *testing.T) {
	agentResp := agentclient.IssueConsoleTokenResponse{
		Token:         "tok-direct",
		WebsocketPath: "/v1/vms/demo/console-stream",
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/vms/demo/console", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Even с plain HTTP к the CP, direct mode emits wss:// — agent
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
