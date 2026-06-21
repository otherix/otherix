// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"crypto/tls"
	"testing"
)

func TestUserListenerTLSConfig_Disabled(t *testing.T) {
	cfg, err := userListenerTLSConfig(false, TLSMaterial{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cfg != nil {
		t.Errorf("config = non-nil, want nil when disabled")
	}
}

func TestUserListenerTLSConfig_EnabledNoMaterial(t *testing.T) {
	_, err := userListenerTLSConfig(true, TLSMaterial{Source: "skipped"})
	if err == nil {
		t.Fatal("err = nil, want error when TLS enabled but material empty")
	}
}

func TestUserListenerTLSConfig_EnabledWithMaterial(t *testing.T) {
	mat := TLSMaterial{Cert: tls.Certificate{Certificate: [][]byte{{0x01}}}, Source: "auto_generate"}
	cfg, err := userListenerTLSConfig(true, mat)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cfg == nil {
		t.Fatal("config = nil, want non-nil")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS13 (%x)", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert (users present no client cert)", cfg.ClientAuth)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1", len(cfg.Certificates))
	}
}
