// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"strings"
	"testing"
)

// validBootstrap returns a BootstrapConfig that passes Validate. Tests
// mutate a single field at a time so failure messages point clearly
// at the rule being exercised.
func validBootstrap() BootstrapConfig {
	return BootstrapConfig{
		Token:                   "otx_join_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CAFingerprint:           "sha256:" + strings.Repeat("a", 64),
		CPURL:                   "https://cp.example.com:8443",
		NodeName:                "node-001",
		Architecture:            "arm64",
		AdvertisedEndpoint:      "https://10.0.0.1:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
	}
}

func TestBootstrapConfigValidate_ValidLiteralToken(t *testing.T) {
	cfg := validBootstrap()
	if err := cfg.Validate(); err != nil {
		t.Errorf("validBootstrap().Validate() = %v, want nil", err)
	}
}

func TestBootstrapConfigValidate_ValidTokenPath(t *testing.T) {
	cfg := validBootstrap()
	cfg.Token = ""
	cfg.TokenPath = "/etc/otherix/join-token"
	if err := cfg.Validate(); err != nil {
		t.Errorf("token_path config .Validate() = %v, want nil", err)
	}
}

func TestBootstrapConfigValidate_BareHexFingerprint(t *testing.T) {
	cfg := validBootstrap()
	cfg.CAFingerprint = strings.Repeat("a", 64)
	if err := cfg.Validate(); err != nil {
		t.Errorf("bare hex fingerprint .Validate() = %v, want nil", err)
	}
}

func TestBootstrapConfigValidate_UppercaseFingerprint(t *testing.T) {
	cfg := validBootstrap()
	cfg.CAFingerprint = strings.Repeat("A", 64)
	if err := cfg.Validate(); err != nil {
		t.Errorf("uppercase fingerprint .Validate() = %v, want nil", err)
	}
}

func TestBootstrapConfigValidate_TableOfFailures(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*BootstrapConfig)
		want string
	}{
		{
			name: "both token and token_path",
			mut:  func(c *BootstrapConfig) { c.TokenPath = "/path" },
			want: "mutually exclusive",
		},
		{
			name: "neither token nor token_path",
			mut:  func(c *BootstrapConfig) { c.Token = "" },
			want: "one of token or token_path",
		},
		{
			name: "missing ca_fingerprint",
			mut:  func(c *BootstrapConfig) { c.CAFingerprint = "" },
			want: "ca_fingerprint is required",
		},
		{
			name: "fingerprint too short",
			mut:  func(c *BootstrapConfig) { c.CAFingerprint = "abc" },
			want: "ca_fingerprint must be 64 hex chars",
		},
		{
			name: "fingerprint non-hex",
			mut:  func(c *BootstrapConfig) { c.CAFingerprint = strings.Repeat("g", 64) },
			want: "ca_fingerprint must be 64 hex chars",
		},
		{
			name: "missing cp_url",
			mut:  func(c *BootstrapConfig) { c.CPURL = "" },
			want: "cp_url is required",
		},
		{
			name: "http scheme rejected",
			mut:  func(c *BootstrapConfig) { c.CPURL = "http://cp.example.com:8443" },
			want: "cp_url must use https",
		},
		{
			name: "missing node_name",
			mut:  func(c *BootstrapConfig) { c.NodeName = "" },
			want: "node_name is required",
		},
		{
			name: "node_name too long",
			mut:  func(c *BootstrapConfig) { c.NodeName = strings.Repeat("a", 254) },
			want: "node_name must be at most 253",
		},
		{
			name: "invalid architecture",
			mut:  func(c *BootstrapConfig) { c.Architecture = "i386" },
			want: "architecture must be amd64 or arm64",
		},
		{
			name: "missing advertised_endpoint",
			mut:  func(c *BootstrapConfig) { c.AdvertisedEndpoint = "" },
			want: "advertised_endpoint is required",
		},
		{
			name: "advertised_endpoint too long",
			mut:  func(c *BootstrapConfig) { c.AdvertisedEndpoint = strings.Repeat("x", 2049) },
			want: "advertised_endpoint must be at most 2048",
		},
		{
			name: "missing migration_host",
			mut:  func(c *BootstrapConfig) { c.MigrationHost = "" },
			want: "migration_host is required",
		},
		{
			name: "migration_host too long",
			mut:  func(c *BootstrapConfig) { c.MigrationHost = strings.Repeat("h", 254) },
			want: "migration_host must be at most 253",
		},
		{
			name: "migration port range start below 1024",
			mut:  func(c *BootstrapConfig) { c.MigrationPortRangeStart = 1000 },
			want: "migration_port_range must lie in [1024, 65535]",
		},
		{
			name: "migration port range end above 65535",
			mut:  func(c *BootstrapConfig) { c.MigrationPortRangeEnd = 70000 },
			want: "migration_port_range must lie in [1024, 65535]",
		},
		{
			name: "migration port range end before start",
			mut:  func(c *BootstrapConfig) { c.MigrationPortRangeStart = 50000; c.MigrationPortRangeEnd = 40000 },
			want: "end >= start",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBootstrap()
			tt.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

// AgentConfig no longer carries a Bootstrap field — bootstrap is a
// separate subcommand with standalone flag validation. The runtime-config
// Validate path therefore checks only the agent runtime invariants
// (server, control_plane, migration).
func TestAgentConfigValidate_RuntimeMinimumOK(t *testing.T) {
	cfg := AgentConfig{
		Server:       ServerConfig{Listen: "0.0.0.0:9443"},
		ControlPlane: ControlPlaneConfig{URL: "https://cp:8443"},
		Migration:    MigrationConfig{Host: "10.0.0.1", PortRangeStart: 49152, PortRangeEnd: 49251},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("AgentConfig.Validate() = %v, want nil", err)
	}
}
