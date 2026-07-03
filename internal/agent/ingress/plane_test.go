// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingress

import (
	"context"
	"crypto/tls"
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

// writeIngressTLSMaterial writes a self-consistent cert/key/ca trio to a temp dir
// and returns the TLS config pointing at them. The cluster CA cert doubles as the
// leaf here: BuildPlane only needs a loadable keypair and a parseable CA
// pool, and the TLS handshake itself is not exercised by this router-level test.
func writeIngressTLSMaterial(t *testing.T) config.TLSConfig {
	t.Helper()
	ca, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "leaf.crt")
	keyPath := filepath.Join(dir, "leaf.key")
	if err := os.WriteFile(caPath, ca.CertPEM, 0o644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, ca.KeyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return config.TLSConfig{CACertPath: caPath, CertPath: certPath, KeyPath: keyPath}
}

// TestBuildIngressPlaneRefusesUndeclaredMembership proves the extracted builder
// wires the bearer-only ingress plane end to end AND that the plane is inert
// until the CP declares a membership: with the session CA armed and a valid
// credential, a connect whose guest IP resolves to NO overlay membership
// (Overlays returns ok=false) is refused. This is the co-located node's safety
// property - the listener serves /v1/connect but splices nothing the CP has not
// declared. It also pins the TLS config to VerifyClientCertIfGiven so a
// certificate-less client completes the handshake (the connect client presents no
// mTLS cert, only a session credential).
func TestBuildIngressPlaneRefusesUndeclaredMembership(t *testing.T) {
	tlsCfg := writeIngressTLSMaterial(t)

	plane, err := BuildPlane(PlaneDeps{
		Listen:       "127.0.0.1:0",
		TLS:          tlsCfg,
		Fabric:       &netfabric.FakeFabric{},
		Overlays:     fakeOverlays{ok: false}, // guest IP resolves to no membership
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		Log:          discardLogger(),
	})
	if err != nil {
		t.Fatalf("BuildPlane() error = %v", err)
	}

	if plane.Server.TLSConfig.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ingress TLS ClientAuth = %v, want VerifyClientCertIfGiven (bearer-only, cert optional)",
			plane.Server.TLSConfig.ClientAuth)
	}
	if plane.Server.Addr != "127.0.0.1:0" {
		t.Errorf("ingress server Addr = %q, want %q", plane.Server.Addr, "127.0.0.1:0")
	}

	// Arm the credential gate exactly as a heartbeat carrying the session CA
	// public half would, then present a well-formed credential. The refusal must
	// come from the missing membership, not the gate.
	signer, pubPEM := newTestSessionCA(t)
	plane.CAStore.HandleHeartbeatResponse(context.Background(),
		&heartbeat.Response{SessionCAPublicPEM: &pubPEM})
	if plane.CAStore.Current() == nil {
		t.Fatal("session-CA store did not accept the public half")
	}

	// Drive the plane's router directly (the wiring under test); the TLS handshake
	// is not the subject of this router-level assertion.
	srv := httptest.NewServer(plane.Server.Handler)
	t.Cleanup(srv.Close)

	token := signCred(t, signer, auth.SessionCredClaims{
		VMID: uuid.New(), NICMAC: testMACA, GuestIP: netip.MustParseAddr("10.42.0.5"),
		Port: 22, ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	resp := doConnect(t, srv.URL, token)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("connect with no declared membership status = %d, want 403 (refused)", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "permission_denied" {
		t.Errorf("error code = %q, want permission_denied", code)
	}
}
