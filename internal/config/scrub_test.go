// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"strings"
	"testing"
	"time"
)

func TestArtifactsScrubDefaults(t *testing.T) {
	c := defaultAgentConfig()
	if c.Artifacts.Scrub.Interval != time.Hour {
		t.Errorf("Scrub.Interval = %v, want 1h", c.Artifacts.Scrub.Interval)
	}
	if c.Artifacts.Scrub.MinReverifyInterval != 168*time.Hour {
		t.Errorf("Scrub.MinReverifyInterval = %v, want 168h", c.Artifacts.Scrub.MinReverifyInterval)
	}
	if c.Artifacts.Scrub.MaxBytesPerPass != 10<<30 {
		t.Errorf("Scrub.MaxBytesPerPass = %d, want %d", c.Artifacts.Scrub.MaxBytesPerPass, int64(10<<30))
	}
}

func TestArtifactsScrubValidateNegative(t *testing.T) {
	c := ArtifactsConfig{
		Root:           "/var/lib/otherix/artifacts",
		PortRangeStart: 49252,
		PortRangeEnd:   49351,
		Scrub:          ScrubConfig{MaxBytesPerPass: -1},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "scrub.max_bytes_per_pass") {
		t.Errorf("Validate() = %v, want error mentioning scrub.max_bytes_per_pass", err)
	}
}
