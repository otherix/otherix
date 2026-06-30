// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteGatewayConfig_WireGuardBlock confirms the generated gateway.yaml
// carries a WireGuard block with a DEDICATED key path (never the agent's
// shared /var/lib/otherix/wg/private.key), the operator-supplied listen port,
// and the advertised endpoint peers dial for the handshake. Without these a
// gateway either collides with a co-located agent's WG identity or cannot
// join the mesh.
func TestWriteGatewayConfig_WireGuardBlock(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "gateway.yaml")

	in := gatewayConfigInputs{
		CPURL:               "https://cp.example:8443",
		HeartbeatInterval:   30 * time.Second,
		ListenAddr:          "0.0.0.0:9443",
		CertPath:            "/var/lib/otherix/certs/gateway.crt",
		KeyPath:             "/var/lib/otherix/certs/gateway.key",
		CAPath:              "/var/lib/otherix/certs/ca.crt",
		WireguardEndpoint:   "192.0.2.7:51820",
		WireguardListenPort: 51820,
	}
	if err := writeGatewayConfig(dest, in); err != nil {
		t.Fatalf("writeGatewayConfig: %v", err)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	got := string(raw)

	const dedicatedKeyPath = `private_key_path: "/var/lib/otherix/wg-gateway/private.key"`
	if !strings.Contains(got, dedicatedKeyPath) {
		t.Errorf("rendered config missing dedicated WG key path %q\n---\n%s", dedicatedKeyPath, got)
	}
	// The dedicated path must NOT be the agent default — a shared key would
	// collide with a co-located agent's WG pubkey.
	if strings.Contains(got, `private_key_path: "/var/lib/otherix/wg/private.key"`) {
		t.Errorf("rendered config uses the agent default WG key path; gateway must own a dedicated key\n---\n%s", got)
	}
	if !strings.Contains(got, "listen_port: 51820") {
		t.Errorf("rendered config missing listen_port: 51820\n---\n%s", got)
	}
	if !strings.Contains(got, `advertised_endpoint: "192.0.2.7:51820"`) {
		t.Errorf("rendered config missing advertised_endpoint: \"192.0.2.7:51820\"\n---\n%s", got)
	}
}
