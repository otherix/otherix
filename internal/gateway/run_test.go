// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
)

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
			handler := buildRouter(cfg, "edge1", discardLogger(), spy)

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
