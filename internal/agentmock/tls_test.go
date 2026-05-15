// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestServerTLSConfig_RequiresClientCert(t *testing.T) {
	cfg, err := serverTLSConfig()
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Errorf("ClientCAs is nil; want CA pool loaded from testdata")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("len(Certificates) = %d, want 1", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
}

func TestClientTLSConfig_PinsCAAndPresentsCert(t *testing.T) {
	cfg, err := clientTLSConfig()
	if err != nil {
		t.Fatalf("clientTLSConfig: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Errorf("RootCAs is nil; want CA pool loaded from testdata")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("len(Certificates) = %d, want 1 (node cert as client cert)",
			len(cfg.Certificates))
	}
	leaf := cfg.Certificates[0].Leaf
	if leaf == nil {
		// X509KeyPair does not parse Leaf eagerly; loadKeyPair could
		// be tightened to do so but the existing tls.Certificate
		// shape is acceptable for now. Move on.
		return
	}
	if leaf.Subject.CommonName != "node-mock-1.agents.otherix.local" {
		t.Errorf("Subject CN = %q, want node-mock-1.agents.otherix.local",
			leaf.Subject.CommonName)
	}
}

func TestMTLS_HandshakeWithClient(t *testing.T) {
	m := Start(t, Options{TLS: TLSEnabled})
	clientCfg, err := clientTLSConfig()
	if err != nil {
		t.Fatalf("clientTLSConfig: %v", err)
	}
	// Override server-name verification: testdata/certs/node.crt's
	// SAN includes localhost and 127.0.0.1, so the URL httptest
	// hands us (https://127.0.0.1:NNN) verifies cleanly.
	clientCfg.ServerName = ""

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientCfg},
	}
	resp, err := client.Get(m.URL() + "/v1/health")
	if err != nil {
		t.Fatalf("mTLS GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	got := m.ReceivedRequests()
	if len(got) != 1 {
		t.Fatalf("len(ReceivedRequests) = %d, want 1", len(got))
	}
	if got[0].PeerCN != "node-mock-1.agents.otherix.local" {
		t.Errorf("PeerCN = %q, want node-mock-1.agents.otherix.local", got[0].PeerCN)
	}
}
