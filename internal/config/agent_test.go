// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import "testing"

// baseGatewayAgentConfig returns a valid agent config (ControlPlane.URL set so
// the non-gateway rules pass) with a distinct default server listen, ready for
// the per-case Gateway mutation.
func baseGatewayAgentConfig() AgentConfig {
	c := defaultAgentConfig()
	c.ControlPlane.URL = "https://cp.example:9443"
	c.Migration.Host = "10.0.0.1"
	return c
}

// TestAgentConfigGatewayValidate pins the gateway validation rules: when
// Gateway.Enabled the ingress Listen must be non-empty and must differ from the
// control Server.Listen (two distinct ports in one process). A plain hypervisor
// (Enabled=false, no Listen) imposes no gateway constraints, and
// Gateway.AdvertisedEndpoint is validated at bootstrap, not at serve, so an empty
// endpoint here is accepted.
func TestAgentConfigGatewayValidate(t *testing.T) {
	t.Run("plain hypervisor with no ingress listen passes", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.Gateway = GatewayConfig{Enabled: false, Listen: ""}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(plain hypervisor) = %v, want nil", err)
		}
	})

	t.Run("enabled with distinct listen passes", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.Gateway = GatewayConfig{Enabled: true, Listen: "0.0.0.0:9444"}
		if c.Gateway.Listen == c.Server.Listen {
			t.Fatalf("test setup: gateway listen must differ from server listen")
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(enabled, distinct listen) = %v, want nil", err)
		}
	})

	t.Run("enabled with empty listen fails", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.Gateway = GatewayConfig{Enabled: true, Listen: ""}
		if err := c.Validate(); err == nil {
			t.Error("Validate(enabled, empty listen) = nil, want error")
		}
	})

	t.Run("enabled with listen equal to server listen fails", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.Gateway = GatewayConfig{Enabled: true, Listen: c.Server.Listen}
		if err := c.Validate(); err == nil {
			t.Error("Validate(enabled, listen == server listen) = nil, want error")
		}
	})
}

// TestGatewayConfigValidatesIngressListenWithoutEnabled pins the co-location
// capability: an ingress Listen is a valid hypervisor add-on independent of
// Gateway.Enabled (which only dispatches the standalone-no-KVM run path). A
// Listen set with Enabled=false must validate, and the distinct-port rule still
// applies whenever Listen is set.
func TestGatewayConfigValidatesIngressListenWithoutEnabled(t *testing.T) {
	c := baseGatewayAgentConfig()
	c.Gateway.Enabled = false
	c.Gateway.Listen = "0.0.0.0:9444"
	c.Gateway.AdvertisedEndpoint = "https://node-2:9444"
	if c.Gateway.Listen == c.Server.Listen {
		t.Fatalf("test setup: gateway listen must differ from server listen")
	}
	if err := c.Gateway.validate(c.Server.Listen); err != nil {
		t.Errorf("validate() with co-located ingress config error = %v, want nil", err)
	}

	c.Gateway.Listen = c.Server.Listen
	if err := c.Gateway.validate(c.Server.Listen); err == nil {
		t.Errorf("validate() with ingress listen == control listen returned nil, want a distinct-port error")
	}
}

// TestAgentConfigWireGuardAdvertisedEndpointOptional pins that the WireGuard
// advertised endpoint is optional: a node behind NAT reports no reachable WG
// endpoint (empty), and a node with a public endpoint reports one. Both must
// pass Validate - the endpoint is a reachability hint the control plane keys on,
// not a runtime-config requirement. A future Validate rule that rejected the
// empty form would strand every NAT'd node at config load.
func TestAgentConfigWireGuardAdvertisedEndpointOptional(t *testing.T) {
	t.Run("empty endpoint (behind NAT) passes", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.WireGuard.AdvertisedEndpoint = ""
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(empty wireguard advertised endpoint) = %v, want nil", err)
		}
	})

	t.Run("public endpoint passes", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.WireGuard.AdvertisedEndpoint = "203.0.113.7:51820"
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(public wireguard advertised endpoint) = %v, want nil", err)
		}
	})
}
