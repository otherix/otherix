// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package config defines the configuration types for every Otherix binary
// (api, agent) and loads them from YAML files plus environment variables
// (prefix OTHERIX_) using koanf.
package config

import (
	"errors"
	"fmt"
	"time"
)

// ServerConfig describes how a binary's HTTP(S) server should listen.
type ServerConfig struct {
	Listen        string        `koanf:"listen"`
	ReadTimeout   time.Duration `koanf:"read_timeout"`
	WriteTimeout  time.Duration `koanf:"write_timeout"`
	ShutdownGrace time.Duration `koanf:"shutdown_grace"`
}

// Validate reports an error if Listen is empty.
func (c ServerConfig) Validate() error {
	if c.Listen == "" {
		return errors.New("listen is required")
	}
	return nil
}

// AgentServerConfig describes the optional second HTTPS listener on
// the api-server, dedicated to mTLS-authenticated agent traffic
// (currently `POST /v1/nodes/{id}/heartbeat`). It is independent of
// the user-facing ServerConfig so operators can bind agent traffic
// to a dedicated port / NIC and so user endpoints stay reachable
// without client-cert material.
//
// When Enabled is false the api-server skips the listener entirely
// — the standard dev workflow runs the user listener only.
//
// mTLS material is NOT configured here: each api replica
// auto-generates its own server cert from the cluster CA in DB via the
// LoadOrGenerateCPCert boot hook. Operator manual override moves to
// the `cp_cert:` block (`cert_file` + `key_file`). Cluster CA always
// loaded from DB. Legacy `cert_file` / `key_file` / `ca_file` fields
// under this block are removed outright (pre-prod squash window — no
// production deploys carry them).
type AgentServerConfig struct {
	Enabled bool   `koanf:"enabled"`
	Listen  string `koanf:"listen"`
}

// Validate reports an error when Enabled is true and the listen
// address is missing. Cert/key/CA material lives elsewhere now (see
// cp_cert block + LoadOrGenerateCPCert hook).
func (c AgentServerConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Listen == "" {
		return errors.New("agent_server.listen is required when enabled")
	}
	return nil
}

// MigrationConfig describes the network settings the agent uses for peer-to-peer
// VM migration. The port range supports parallel migrations on a single node
// (typical production case: drain-a-node migration of an entire cluster).
// Host is intentionally separate from ServerConfig.Listen so that operators
// can assign a dedicated migration network/VLAN.
type MigrationConfig struct {
	Host           string `koanf:"host"`
	PortRangeStart int    `koanf:"port_range_start"`
	PortRangeEnd   int    `koanf:"port_range_end"`
	// ConvergenceTimeout bounds how long the live-migration watchdog
	// waits without RAM-transfer progress before aborting and failing
	// safe back to the source. Zero means use the built-in default.
	ConvergenceTimeout time.Duration `koanf:"convergence_timeout"`
}

// Validate checks that Host is non-empty and both ports are in
// [1024, 65535] with PortRangeEnd >= PortRangeStart.
func (c MigrationConfig) Validate() error {
	if c.Host == "" {
		return errors.New("host is required")
	}
	if c.PortRangeStart < 1024 || c.PortRangeStart > 65535 {
		return fmt.Errorf("port_range_start must be in [1024, 65535], got %d", c.PortRangeStart)
	}
	if c.PortRangeEnd < 1024 || c.PortRangeEnd > 65535 {
		return fmt.Errorf("port_range_end must be in [1024, 65535], got %d", c.PortRangeEnd)
	}
	if c.PortRangeEnd < c.PortRangeStart {
		return errors.New("port_range_end must be >= port_range_start")
	}
	if c.ConvergenceTimeout < 0 {
		return fmt.Errorf("convergence_timeout must be >= 0, got %s", c.ConvergenceTimeout)
	}
	return nil
}
