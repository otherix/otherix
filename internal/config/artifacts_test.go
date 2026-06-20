// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"testing"
	"time"
)

func TestArtifactsConfigValidate(t *testing.T) {
	good := ArtifactsConfig{Root: "/var/lib/otherix/artifacts", PortRangeStart: 49252, PortRangeEnd: 49351}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate(good) = %v, want nil", err)
	}

	cases := []struct {
		name string
		cfg  ArtifactsConfig
	}{
		{"empty root", ArtifactsConfig{Root: "", PortRangeStart: 49252, PortRangeEnd: 49351}},
		{"start too low", ArtifactsConfig{Root: "/var/lib/otherix/x", PortRangeStart: 1023, PortRangeEnd: 49351}},
		{"end too high", ArtifactsConfig{Root: "/var/lib/otherix/x", PortRangeStart: 49252, PortRangeEnd: 70000}},
		{"end before start", ArtifactsConfig{Root: "/var/lib/otherix/x", PortRangeStart: 49351, PortRangeEnd: 49252}},
		{"root outside allowlist", ArtifactsConfig{Root: "/etc/otherix/artifacts", PortRangeStart: 49252, PortRangeEnd: 49351}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want error", tc.cfg)
			}
		})
	}
}

func TestDefaultAgentConfigArtifacts(t *testing.T) {
	c := defaultAgentConfig()
	if c.Artifacts.Root != "/var/lib/otherix/artifacts" {
		t.Errorf("default Artifacts.Root = %q, want /var/lib/otherix/artifacts", c.Artifacts.Root)
	}
	if c.Artifacts.PortRangeStart != 49252 || c.Artifacts.PortRangeEnd != 49351 {
		t.Errorf("default Artifacts port range = [%d,%d], want [49252,49351]", c.Artifacts.PortRangeStart, c.Artifacts.PortRangeEnd)
	}
	// The artifact port range must NOT overlap the migration range (49152-49251).
	if c.Artifacts.PortRangeStart <= c.Migration.PortRangeEnd {
		t.Errorf("artifact range start %d overlaps migration end %d", c.Artifacts.PortRangeStart, c.Migration.PortRangeEnd)
	}
	// defaultAgentConfig has no control_plane.url or migration.host (both are
	// supplied via file/env at Load time; load_test.go asserts the bare default
	// fails Validate for exactly that reason). Set them so this test exercises
	// its real intent: the default Artifacts block does not break
	// AgentConfig.Validate.
	c.ControlPlane.URL = "https://cp.example.local:8080"
	c.Migration.Host = "10.0.0.1"
	if err := c.Validate(); err != nil {
		t.Errorf("default AgentConfig.Validate() = %v, want nil", err)
	}
}

func TestImageCacheConfigDefaultsAndValidation(t *testing.T) {
	ic := defaultAgentConfig().Artifacts.ImageCache
	if ic.MaxBytes != 50<<30 {
		t.Errorf("default MaxBytes = %d, want %d", ic.MaxBytes, int64(50<<30))
	}
	if ic.MinFreeBytes != 10<<30 {
		t.Errorf("default MinFreeBytes = %d, want %d", ic.MinFreeBytes, int64(10<<30))
	}
	if ic.EvictionInterval != 5*time.Minute {
		t.Errorf("default EvictionInterval = %v, want 5m", ic.EvictionInterval)
	}

	bad := ArtifactsConfig{
		Root:           "/var/lib/otherix/artifacts",
		PortRangeStart: 49252,
		PortRangeEnd:   49351,
		ImageCache:     ImageCacheConfig{MaxBytes: -1},
	}
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate() = nil for negative MaxBytes, want error")
	}
}
