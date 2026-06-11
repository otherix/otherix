// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// TestBuildAgentServerTLSConfigMinVersion pins the agent-facing mTLS
// listener to a TLS 1.3 floor. Every client of this listener is Go
// crypto/tls (real agents, the anonymous bootstrap dialer, the
// cluster-join path), so 1.3 is always available; a 1.2 floor would let
// the listener accept a downgrade after the dialers were raised to 1.3.
func TestBuildAgentServerTLSConfigMinVersion(t *testing.T) {
	ca, err := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, caDER, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}

	material := TLSMaterial{
		// Cert content is irrelevant to the MinVersion assertion; the
		// constructor only checks the leaf byte slice is non-empty.
		Cert:      tls.Certificate{Certificate: [][]byte{caDER}},
		ClusterCA: caCert,
		Source:    "auto_generate",
	}

	cfg, err := buildAgentServerTLSConfig(material)
	if err != nil {
		t.Fatalf("buildAgentServerTLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want tls.VersionTLS13 (%#x)", cfg.MinVersion, tls.VersionTLS13)
	}
}
