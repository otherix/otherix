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
