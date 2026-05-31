// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package etcd embeds an etcd v3 cluster member in the Otherix control-plane
// process. One code path runs three operator-selected topologies:
// single-node (default), 3-node HA, and a single->HA transition via learner
// promotion. This package owns the embedded runtime lifecycle and the
// zap->slog logging bridge; higher layers build a KV client over it
// (Phase 3 slice 2) and the etcd-backed store over that (slice 3+).
package etcd

import (
	"fmt"
	"net/url"
)

// Mode selects the embedded member's cluster bootstrap behaviour.
//
//   - ModeSingle: self-only InitialCluster, ClusterState=new (homelab / dev /
//     standalone, quorum of 1).
//   - ModeBootstrap: full InitialCluster list, ClusterState=new (every member
//     of a fresh HA cluster starts together to form quorum).
//   - ModeJoin: full InitialCluster list including self, ClusterState=existing
//     (member added to an already-running cluster first; see cluster expand).
type Mode string

// The supported cluster bootstrap modes; see Mode for the per-mode behaviour.
const (
	ModeSingle    Mode = "single"
	ModeBootstrap Mode = "bootstrap"
	ModeJoin      Mode = "join"
)

// Config holds the fields needed to start an embedded etcd member. The koanf
// config layer maps operator settings into this struct (wired when the api
// binary gains backend selection); the runtime depends only on this shape.
type Config struct {
	Mode      Mode
	Name      string
	DataDir   string
	PeerURL   string
	ClientURL string
	// ClusterToken is the etcd InitialClusterToken; distinct tokens prevent
	// accidental cross-cluster joins on shared networks.
	ClusterToken string
	// InitialCluster is the full member list ("n1=peer1,n2=peer2,..."). Required
	// for ModeBootstrap and ModeJoin; for ModeSingle it is derived from
	// Name+PeerURL when empty.
	InitialCluster string

	// Peer mTLS material. When PeerCAFile is set, inter-member (Raft) traffic
	// uses mutual TLS: every peer presents a cert chaining to the cluster CA and
	// rejects peers that do not - the protection for control-plane replicas
	// talking over a public network. Peer URLs must use https when these are
	// set. In production these are issued by the cluster CA (ADR 0026), reusing
	// the same trust anchor as agent mTLS - no new PKI.
	PeerCertFile string
	PeerKeyFile  string
	PeerCAFile   string
}

// peerTLSEnabled reports whether peer mTLS material is fully configured.
func (c *Config) peerTLSEnabled() bool {
	return c.PeerCAFile != "" && c.PeerCertFile != "" && c.PeerKeyFile != ""
}

// initialClusterString returns the InitialCluster value to hand etcd: the
// explicit list when set, else the single-node self entry.
func (c *Config) initialClusterString() string {
	if c.InitialCluster != "" {
		return c.InitialCluster
	}
	return fmt.Sprintf("%s=%s", c.Name, c.PeerURL)
}

// Validate checks the config invariants. It returns the first failure
// encountered so the operator sees one clear error.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeSingle, ModeBootstrap, ModeJoin:
	default:
		return fmt.Errorf("invalid mode %q (want single|bootstrap|join)", c.Mode)
	}
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data-dir is required")
	}
	if u, err := url.Parse(c.PeerURL); err != nil || c.PeerURL == "" || u.Host == "" {
		return fmt.Errorf("peer-url is required and must be a valid URL")
	}
	if u, err := url.Parse(c.ClientURL); err != nil || c.ClientURL == "" || u.Host == "" {
		return fmt.Errorf("client-url is required and must be a valid URL")
	}
	if c.ClusterToken == "" {
		return fmt.Errorf("cluster-token is required")
	}
	if (c.Mode == ModeBootstrap || c.Mode == ModeJoin) && c.InitialCluster == "" {
		return fmt.Errorf("initial-cluster is required for mode %q", c.Mode)
	}
	return nil
}
