// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
)

// cpIdentityTLSState builds the minimal TLS connection state a request
// carries after mTLS handshake: a single verified peer cert whose
// Subject CN is cn.
func cpIdentityTLSState(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		},
	}
}

// TestBuildRouterPinsCPIdentity drives the real buildRouter output (the
// full middleware chain, not a direct middleware call) and verifies the
// CP-identity gate: a peer cert with the CP CN reaches /health, while a
// node cert (valid under the same cluster CA) is rejected with 403
// before any handler runs.
func TestBuildRouterPinsCPIdentity(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.AgentConfig{}
	cfg.Server.ReadTimeout = 5 * time.Second

	handler := buildRouter(cfg, "node-test", log, nil, nil, nil, noopNudger{}, nil)

	tests := []struct {
		name       string
		cn         string
		wantStatus int
	}{
		{name: "cp identity reaches health", cn: auth.CPCertCommonName, wantStatus: http.StatusOK},
		{name: "node identity rejected at gate", cn: "node-evil", wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			req.TLS = cpIdentityTLSState(tt.cn)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("GET /health (cn=%s) status = %d, want %d", tt.cn, rec.Code, tt.wantStatus)
			}
		})
	}
}

// TestBuildRouterRejectsNodeCertOnConsoleStream pins the worst H1
// capability route: a stolen node cert opening a peer's serial console.
// A node-identity peer cert must be rejected with 403 at the
// RequireCPIdentity gate, before the console-stream handler runs.
// Only the node case is exercised here - a CP cert would proceed into
// the websocket/proxy handler with nil deps, which is not the seam
// being pinned.
func TestBuildRouterRejectsNodeCertOnConsoleStream(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.AgentConfig{}
	cfg.Server.ReadTimeout = 5 * time.Second
	handler := buildRouter(cfg, "node-test", log, nil, nil, nil, noopNudger{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/vms/some-vm/console-stream?token=x", nil)
	req.TLS = cpIdentityTLSState("node-evil")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("console-stream with node cert: status = %d, want 403", rec.Code)
	}
}

// TestBuildRouterRejectsNodeCertOnSSHPipe pins that the relayed splice route
// stays CP-only after the port generalization: a node-identity peer cert
// opening a peer's ssh-pipe - now carrying an arbitrary ?port - must be
// rejected with 403 at the RequireCPIdentity gate, before the SSHPipe handler
// runs. The agent never becomes a client-facing edge; only the CP relay reaches
// this route, regardless of the requested port.
func TestBuildRouterRejectsNodeCertOnSSHPipe(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.AgentConfig{}
	cfg.Server.ReadTimeout = 5 * time.Second
	handler := buildRouter(cfg, "node-test", log, nil, nil, nil, noopNudger{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/vms/some-vm/ssh-pipe?port=5432", nil)
	req.TLS = cpIdentityTLSState("node-evil")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("ssh-pipe with node cert: status = %d, want 403", rec.Code)
	}
}
