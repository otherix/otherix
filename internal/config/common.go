// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package config defines the configuration types for every Otherix binary
// (api, agent) and loads them from YAML files plus environment variables
// (prefix OTHERIX_) using koanf.
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ServerConfig describes how a binary's HTTP(S) server should listen.
type ServerConfig struct {
	Listen        string          `koanf:"listen"`
	ReadTimeout   time.Duration   `koanf:"read_timeout"`
	WriteTimeout  time.Duration   `koanf:"write_timeout"`
	ShutdownGrace time.Duration   `koanf:"shutdown_grace"`
	TLS           ServerTLSConfig `koanf:"tls"`
}

// ServerTLSConfig controls server-side TLS termination on the user-facing
// listener. Enabled defaults to true (set in defaultAPIConfig); an
// explicit `server.tls.enabled: false` opts the listener back to
// plaintext for loopback/dev use. The certificate is the per-replica
// CP cert produced by LoadOrGenerateCPCert (cp_cert block: operator
// override or auto-generated from the cluster CA) - there is no
// separate cert material here.
type ServerTLSConfig struct {
	Enabled bool `koanf:"enabled"`
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

// ArtifactsConfig describes the per-node content-addressed artifact store and
// the peer-facing blob-transport listener used for on-demand cross-node blob
// pulls. Root holds the immutable blobs + manifests; the port range
// supports parallel peer serves on a single node and is intentionally distinct
// from MigrationConfig's range so blob serves never contend for migration
// ingress ports. The listener reuses the node leaf cert (same trust as
// CP<->agent mTLS) - no cert material is configured here.
type ArtifactsConfig struct {
	Root           string           `koanf:"root"`
	PortRangeStart int              `koanf:"port_range_start"`
	PortRangeEnd   int              `koanf:"port_range_end"`
	ImageCache     ImageCacheConfig `koanf:"image_cache"`
	Scrub          ScrubConfig      `koanf:"scrub"`
}

// ImageCacheConfig bounds the node-level pinned-image cache store. A periodic
// sweeper evicts the coldest cached images (LRU by file mtime) to keep the store
// under MaxBytes and to keep at least MinFreeBytes free on its partition. A zero
// ceiling disables that bound; both zero disables eviction.
type ImageCacheConfig struct {
	MaxBytes         int64         `koanf:"max_bytes"`         // total cache size ceiling; 0 disables
	MinFreeBytes     int64         `koanf:"min_free_bytes"`    // partition free-space floor; 0 disables
	EvictionInterval time.Duration `koanf:"eviction_interval"` // periodic pass cadence
}

// ScrubConfig drives the periodic blob scrubber: it re-hashes stored blobs to
// detect silent corruption a Put-time verify cannot catch (bit-rot, a bad
// sector), deletes a confirmed-corrupt copy, and lets the heartbeat inventory +
// durability reconcile re-replicate a healthy copy. Each blob is re-verified at
// most once per MinReverifyInterval, with at most MaxBytesPerPass re-hashed per
// pass to bound read I/O. A zero on any of the three disables the scrubber.
type ScrubConfig struct {
	Interval            time.Duration `koanf:"interval"`              // pass cadence; 0 disables
	MinReverifyInterval time.Duration `koanf:"min_reverify_interval"` // min age before a blob is re-verified; 0 disables
	MaxBytesPerPass     int64         `koanf:"max_bytes_per_pass"`    // per-pass re-hash byte ceiling; 0 disables
}

// artifactsRootAllowedPrefix is the fixed allowlist prefix Root must sit under,
// mirroring storage_pools.allowed_path_prefixes (default /var/lib/otherix/pools/).
// A Root outside it is rejected so a misconfigured agent never reads/writes
// blobs from an arbitrary filesystem location.
const artifactsRootAllowedPrefix = "/var/lib/otherix/"

// Validate checks Root is non-empty and under the fixed allowlist prefix, and
// that both ports are in [1024, 65535] with PortRangeEnd >= PortRangeStart.
func (c ArtifactsConfig) Validate() error {
	if c.Root == "" {
		return errors.New("artifacts.root is required")
	}
	if !strings.HasPrefix(filepath.Clean(c.Root)+"/", artifactsRootAllowedPrefix) {
		return fmt.Errorf("artifacts.root %q must be under %s", c.Root, artifactsRootAllowedPrefix)
	}
	if c.PortRangeStart < 1024 || c.PortRangeStart > 65535 {
		return fmt.Errorf("artifacts.port_range_start must be in [1024, 65535], got %d", c.PortRangeStart)
	}
	if c.PortRangeEnd < 1024 || c.PortRangeEnd > 65535 {
		return fmt.Errorf("artifacts.port_range_end must be in [1024, 65535], got %d", c.PortRangeEnd)
	}
	if c.PortRangeEnd < c.PortRangeStart {
		return errors.New("artifacts.port_range_end must be >= artifacts.port_range_start")
	}
	if c.ImageCache.MaxBytes < 0 {
		return errors.New("artifacts.image_cache.max_bytes must be >= 0")
	}
	if c.ImageCache.MinFreeBytes < 0 {
		return errors.New("artifacts.image_cache.min_free_bytes must be >= 0")
	}
	if c.Scrub.MaxBytesPerPass < 0 {
		return errors.New("artifacts.scrub.max_bytes_per_pass must be >= 0")
	}
	if c.Scrub.Interval < 0 {
		return errors.New("artifacts.scrub.interval must be >= 0")
	}
	if c.Scrub.MinReverifyInterval < 0 {
		return errors.New("artifacts.scrub.min_reverify_interval must be >= 0")
	}
	return nil
}
