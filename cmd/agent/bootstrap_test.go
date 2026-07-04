// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// baseGatewayConfigInputs returns an agentConfigInputs populated with valid
// non-gateway fields so a round-trip through config.LoadAgent validates.
func baseGatewayConfigInputs() agentConfigInputs {
	return agentConfigInputs{
		CPURL:                   "https://cp.example:8443",
		HeartbeatInterval:       30 * time.Second,
		ListenAddr:              "0.0.0.0:9443",
		CertPath:                "/var/lib/otherix/certs/agent.crt",
		KeyPath:                 "/var/lib/otherix/certs/agent.key",
		CAPath:                  "/var/lib/otherix/certs/ca.crt",
		MigrationHost:           "0.0.0.0",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
	}
}

func TestWriteAgentConfigGatewayEmitsGatewayBlock(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.yaml")

	in := baseGatewayConfigInputs()
	in.Gateway = true
	in.GatewayListen = "0.0.0.0:9444"
	in.GatewayAdvertisedEndpoint = "https://gw-1:9444"

	if err := writeAgentConfig(dest, in); err != nil {
		t.Fatalf("writeAgentConfig() error = %v", err)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"gateway:", "enabled: true", `listen: "0.0.0.0:9444"`, `advertised_endpoint: "https://gw-1:9444"`} {
		if !strings.Contains(text, want) {
			t.Errorf("written gateway agent.yaml missing %q\n---\n%s", want, text)
		}
	}
}

func TestWriteAgentConfigGatewayRoundTrips(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.yaml")

	in := baseGatewayConfigInputs()
	in.Gateway = true
	in.GatewayListen = "0.0.0.0:9444"
	in.GatewayAdvertisedEndpoint = "https://gw-1:9444"

	if err := writeAgentConfig(dest, in); err != nil {
		t.Fatalf("writeAgentConfig() error = %v", err)
	}

	// LoadAgent overlays defaults, decodes the YAML, and runs Validate.
	cfg, err := config.LoadAgent(dest)
	if err != nil {
		t.Fatalf("LoadAgent() on gateway config error = %v", err)
	}
	if !cfg.Gateway.Enabled {
		t.Errorf("Gateway.Enabled = false, want true")
	}
	if cfg.Gateway.Listen != "0.0.0.0:9444" {
		t.Errorf("Gateway.Listen = %q, want %q", cfg.Gateway.Listen, "0.0.0.0:9444")
	}
	if cfg.Gateway.Listen == cfg.Server.Listen {
		t.Errorf("Gateway.Listen == Server.Listen = %q, want distinct ports", cfg.Gateway.Listen)
	}
	if cfg.Gateway.AdvertisedEndpoint != "https://gw-1:9444" {
		t.Errorf("Gateway.AdvertisedEndpoint = %q, want %q", cfg.Gateway.AdvertisedEndpoint, "https://gw-1:9444")
	}
}

func TestWriteAgentConfigGatewayUsesDistinctWireGuardKeyPath(t *testing.T) {
	dir := t.TempDir()

	gwDest := filepath.Join(dir, "gateway.yaml")
	in := baseGatewayConfigInputs()
	in.Gateway = true
	in.GatewayListen = "0.0.0.0:9444"
	in.GatewayAdvertisedEndpoint = "https://gw-1:9444"
	if err := writeAgentConfig(gwDest, in); err != nil {
		t.Fatalf("writeAgentConfig() gateway error = %v", err)
	}
	gwCfg, err := config.LoadAgent(gwDest)
	if err != nil {
		t.Fatalf("LoadAgent() gateway error = %v", err)
	}

	const want = "/var/lib/otherix/wg-gateway/private.key"
	if gwCfg.WireGuard.PrivateKeyPath != want {
		t.Errorf("gateway WireGuard.PrivateKeyPath = %q, want %q (a gateway is its own node identity and must not adopt a co-resident agent's WireGuard key)",
			gwCfg.WireGuard.PrivateKeyPath, want)
	}

	// A hypervisor config must keep the shared default path, so the two
	// identities on one host never share a key file.
	hypDest := filepath.Join(dir, "hyp.yaml")
	if err := writeAgentConfig(hypDest, baseGatewayConfigInputs()); err != nil {
		t.Fatalf("writeAgentConfig() hypervisor error = %v", err)
	}
	hypCfg, err := config.LoadAgent(hypDest)
	if err != nil {
		t.Fatalf("LoadAgent() hypervisor error = %v", err)
	}
	if gwCfg.WireGuard.PrivateKeyPath == hypCfg.WireGuard.PrivateKeyPath {
		t.Errorf("gateway and hypervisor WireGuard key paths must differ, both = %q", gwCfg.WireGuard.PrivateKeyPath)
	}
}

func TestWriteAgentConfigNonGatewayHasNoGatewayBlock(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.yaml")

	if err := writeAgentConfig(dest, baseGatewayConfigInputs()); err != nil {
		t.Fatalf("writeAgentConfig() error = %v", err)
	}

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if strings.Contains(string(raw), "gateway:") {
		t.Errorf("non-gateway agent.yaml unexpectedly contains a gateway block\n---\n%s", raw)
	}

	cfg, err := config.LoadAgent(dest)
	if err != nil {
		t.Fatalf("LoadAgent() on hypervisor config error = %v", err)
	}
	if cfg.Gateway.Enabled {
		t.Errorf("Gateway.Enabled = true, want false for a hypervisor agent")
	}
}

func TestWriteAgentConfigEmitsWireGuardAdvertisedEndpoint(t *testing.T) {
	dir := t.TempDir()

	// A non-gateway agent with an explicit WG endpoint round-trips to
	// WireGuard.AdvertisedEndpoint so overlay peers can dial this node - the
	// generated config was previously mesh-incapable (no endpoint emitted).
	hypDest := filepath.Join(dir, "hyp.yaml")
	hypIn := baseGatewayConfigInputs()
	hypIn.WireGuardAdvertisedEndpoint = "10.0.0.5:51820"
	if err := writeAgentConfig(hypDest, hypIn); err != nil {
		t.Fatalf("writeAgentConfig() hypervisor error = %v", err)
	}
	hypCfg, err := config.LoadAgent(hypDest)
	if err != nil {
		t.Fatalf("LoadAgent() hypervisor error = %v", err)
	}
	if hypCfg.WireGuard.AdvertisedEndpoint != "10.0.0.5:51820" {
		t.Errorf("hypervisor WireGuard.AdvertisedEndpoint = %q, want %q", hypCfg.WireGuard.AdvertisedEndpoint, "10.0.0.5:51820")
	}

	// A gateway with an explicit WG endpoint round-trips too, added alongside
	// the gateway-distinct private key path (both must survive).
	gwDest := filepath.Join(dir, "gw.yaml")
	gwIn := baseGatewayConfigInputs()
	gwIn.Gateway = true
	gwIn.GatewayListen = "0.0.0.0:9444"
	gwIn.GatewayAdvertisedEndpoint = "https://gw-1:9444"
	gwIn.WireGuardAdvertisedEndpoint = "10.0.0.6:51820"
	if err := writeAgentConfig(gwDest, gwIn); err != nil {
		t.Fatalf("writeAgentConfig() gateway error = %v", err)
	}
	gwCfg, err := config.LoadAgent(gwDest)
	if err != nil {
		t.Fatalf("LoadAgent() gateway error = %v", err)
	}
	if gwCfg.WireGuard.AdvertisedEndpoint != "10.0.0.6:51820" {
		t.Errorf("gateway WireGuard.AdvertisedEndpoint = %q, want %q", gwCfg.WireGuard.AdvertisedEndpoint, "10.0.0.6:51820")
	}
	if gwCfg.WireGuard.PrivateKeyPath != "/var/lib/otherix/wg-gateway/private.key" {
		t.Errorf("gateway WireGuard.PrivateKeyPath = %q, want the gateway-distinct path (must survive the added endpoint line)", gwCfg.WireGuard.PrivateKeyPath)
	}
}

func TestWriteAgentConfigOmitsEmptyWireGuardEndpoint(t *testing.T) {
	// An unset WG endpoint (single-node fabric) must not emit an
	// advertised_endpoint, leaving WireGuard.AdvertisedEndpoint empty.
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.yaml")
	if err := writeAgentConfig(dest, baseGatewayConfigInputs()); err != nil {
		t.Fatalf("writeAgentConfig() error = %v", err)
	}
	cfg, err := config.LoadAgent(dest)
	if err != nil {
		t.Fatalf("LoadAgent() error = %v", err)
	}
	if cfg.WireGuard.AdvertisedEndpoint != "" {
		t.Errorf("WireGuard.AdvertisedEndpoint = %q, want empty for a single-node config", cfg.WireGuard.AdvertisedEndpoint)
	}
}

func TestReadBootstrapInputsGatewayRequiresIngressEndpoint(t *testing.T) {
	cmd := newBootstrapCommand()
	if err := cmd.Flags().Set("token", "otx_join_test"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	if err := cmd.Flags().Set("gateway", "true"); err != nil {
		t.Fatalf("set gateway: %v", err)
	}

	if _, err := readBootstrapInputs(cmd); err == nil {
		t.Fatalf("readBootstrapInputs() with --gateway and no --ingress-advertised-endpoint returned nil error, want validation error")
	}
}

func TestReadBootstrapInputsGatewayOK(t *testing.T) {
	cmd := newBootstrapCommand()
	if err := cmd.Flags().Set("token", "otx_join_test"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	if err := cmd.Flags().Set("gateway", "true"); err != nil {
		t.Fatalf("set gateway: %v", err)
	}
	if err := cmd.Flags().Set("ingress-advertised-endpoint", "https://gw-1:9444"); err != nil {
		t.Fatalf("set ingress-advertised-endpoint: %v", err)
	}

	in, err := readBootstrapInputs(cmd)
	if err != nil {
		t.Fatalf("readBootstrapInputs() error = %v", err)
	}
	if !in.gateway {
		t.Errorf("in.gateway = false, want true")
	}
	if in.ingressAdvertisedEndpoint != "https://gw-1:9444" {
		t.Errorf("in.ingressAdvertisedEndpoint = %q, want %q", in.ingressAdvertisedEndpoint, "https://gw-1:9444")
	}
	if in.ingressListen != "0.0.0.0:9444" {
		t.Errorf("in.ingressListen = %q, want default %q", in.ingressListen, "0.0.0.0:9444")
	}
}
