// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"fmt"
	"time"
)

// AgentClientConfig configures the CP→agent HTTP client (polling
// parameters only) used by the worker dispatcher to drive async operations
// on agents — currently the storage_pool.scan worker, with future
// task types (vm.*, storage_pool.scan) reusing the same client.
//
// mTLS material (the CP's replica-specific leaf cert signed by the
// cluster CA, plus the cluster CA itself) is NOT configured here.
// Cert lifecycle is owned by a DB-backed LoadOrGenerateCPCert boot
// hook on the api side: each replica auto-generates its own cert
// from the cluster CA in DB and passes the material to
// agentclient.New(cfg, cert, ca). CLI tools (ping-agent, integration
// tests) that don't have DB access use agentclient.LoadMaterialFromFiles
// instead.
//
// Enabled gates the constructor: the api binary skips client
// construction entirely when false (HTTP-only smoke loops). The
// scan executor and other workers that dispatch to agents
// require enabled=true.
type AgentClientConfig struct {
	Enabled         bool          `koanf:"enabled"`
	Timeout         time.Duration `koanf:"timeout"`
	PollInterval    time.Duration `koanf:"poll_interval"`
	PollMaxInterval time.Duration `koanf:"poll_max_interval"`
}

// Validate enforces the agent-client invariants. Disabled configs
// skip every check so dev / test environments still load. When
// enabled, polling parameters must be strictly positive and ordered
// (PollMaxInterval >= PollInterval).
func (c AgentClientConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("agent_client.timeout must be > 0, got %v", c.Timeout)
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("agent_client.poll_interval must be > 0, got %v", c.PollInterval)
	}
	if c.PollMaxInterval < c.PollInterval {
		return fmt.Errorf("agent_client.poll_max_interval must be >= poll_interval (got %v < %v)",
			c.PollMaxInterval, c.PollInterval)
	}
	return nil
}
