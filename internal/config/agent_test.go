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

// TestAgentConfigGatewayValidate pins the gateway-mode validation rules: when
// Gateway.Enabled the ingress Listen must be non-empty and must differ from the
// control Server.Listen (two distinct ports in one process). Gateway.Enabled
// false leaves the field ignored, and Gateway.AdvertisedEndpoint is validated at
// bootstrap, not at serve, so an empty endpoint here is accepted.
func TestAgentConfigGatewayValidate(t *testing.T) {
	t.Run("disabled ignores gateway fields", func(t *testing.T) {
		c := baseGatewayAgentConfig()
		c.Gateway = GatewayConfig{Enabled: false, Listen: c.Server.Listen}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(disabled) = %v, want nil", err)
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
