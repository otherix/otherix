// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/config"
)

// TestLoadOrGenerateCPCert_SkipWhenNoConsumer verifies the skip lock:
// when neither agent_server nor agent_client is enabled, the
// orchestrator returns a Source="skipped" zero material and does NOT
// touch the database. This test passes a nil store to prove the no-
// DB code path executes only the skip branch.
func TestLoadOrGenerateCPCert_SkipWhenNoConsumer(t *testing.T) {
	t.Parallel()
	cfg := config.APIConfig{
		AgentServer: config.AgentServerConfig{Enabled: false},
		AgentClient: config.AgentClientConfig{Enabled: false},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	material, err := api.LoadOrGenerateCPCert(context.Background(), nil, cfg, log)
	if err != nil {
		t.Fatalf("LoadOrGenerateCPCert: %v", err)
	}
	if !material.Skipped() {
		t.Errorf("material.Skipped() = false, want true (no consumer)")
	}
	if material.Source != "skipped" {
		t.Errorf("Source = %q, want %q", material.Source, "skipped")
	}
	if len(material.Cert.Certificate) != 0 {
		t.Error("Cert should be zero-valued when skipped")
	}
	if material.ClusterCA != nil {
		t.Error("ClusterCA should be nil when skipped")
	}
}
