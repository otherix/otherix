// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package etcd embeds an etcd v3 cluster member in the Otherix control-plane
// process. One code path runs three operator-selected topologies:
// single-node (default), 3-node HA, and a single->HA transition via learner
// promotion. This package owns the embedded runtime lifecycle and the
// zap->slog logging bridge; higher layers build a KV client over it
// and the etcd-backed store over that.
package etcd

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

	// CompactionMode and CompactionRetention bound the MVCC history etcd keeps,
	// so a long-running member does not accumulate unbounded key revisions.
	// Mode is "periodic" (retention is a duration, e.g. "1h") or "revision"
	// (retention is a revision count, e.g. "5000"). Empty fields default to
	// periodic / "1h" in buildEmbedConfig; etcd's own default leaves compaction
	// off, which is why we set a non-zero retention.
	CompactionMode      string
	CompactionRetention string

	// Peer mTLS material. When PeerCAFile is set, inter-member (Raft) traffic
	// uses mutual TLS: every peer presents a cert chaining to the cluster CA and
	// rejects peers that do not - the protection for control-plane replicas
	// talking over a public network. Peer URLs must use https when these are
	// set. In production these are issued by the cluster CA, reusing
	// the same trust anchor as agent mTLS - no new PKI.
	PeerCertFile string
	PeerKeyFile  string
	PeerCAFile   string

	// UnsafeNoFsync disables every etcd fsync. It trades durability for speed
	// and MUST NEVER be set in production; it exists only so the test harness
	// can cut the fsync cost of the embedded member it spins up per suite.
	UnsafeNoFsync bool
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

// validatePeerURL checks the peer URL is a valid URL and, when https, that peer
// mTLS material is set (else etcd would serve an https peer listener with no TLS
// config). Peer mTLS is always on in production; this guards a caller that sets
// https without wiring the files.
func (c *Config) validatePeerURL() error {
	peerU, err := url.Parse(c.PeerURL)
	if err != nil || c.PeerURL == "" || peerU.Host == "" {
		return errors.New("peer-url is required and must be a valid URL")
	}
	if peerU.Scheme == "https" && !c.peerTLSEnabled() {
		return errors.New("peer-url is https but peer mTLS files (cert/key/ca) are not all set")
	}
	return nil
}

// validateClientURL checks the client URL is a valid URL bound to loopback. The
// etcd client KV API is plaintext and unauthenticated and exposes the cluster CA
// private key and every secret hash, so it must never bind to a routable address
// The control plane reads KV in-process; the membership/backup TCP
// clients dial the local loopback client URL; HA inter-member traffic uses the
// peer (Raft) transport - so there is no legitimate non-loopback client URL.
func (c *Config) validateClientURL() error {
	u, err := url.Parse(c.ClientURL)
	if err != nil || c.ClientURL == "" || u.Host == "" {
		return errors.New("client-url is required and must be a valid URL")
	}
	if !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("client-url host %q is not loopback: the etcd client KV API is "+
			"plaintext and unauthenticated (it exposes the cluster CA private key and all "+
			"secrets), so it must bind to loopback (127.0.0.1 / ::1 / localhost)", u.Hostname())
	}
	return nil
}

// isLoopbackHost reports whether host is a loopback address (127.0.0.0/8 or ::1)
// or the name "localhost" (case-insensitive). It does not resolve DNS: any other
// hostname is rejected even if it would resolve to a loopback address, pinning
// the guard on the literal value (fail-closed).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		return errors.New("name is required")
	}
	if c.DataDir == "" {
		return errors.New("data-dir is required")
	}
	if err := c.validatePeerURL(); err != nil {
		return err
	}
	if err := c.validateClientURL(); err != nil {
		return err
	}
	if c.ClusterToken == "" {
		return errors.New("cluster-token is required")
	}
	if (c.Mode == ModeBootstrap || c.Mode == ModeJoin) && c.InitialCluster == "" && !c.memberDirExists() {
		return fmt.Errorf("initial-cluster is required for mode %q on a fresh data dir", c.Mode)
	}
	return nil
}

// memberDirExists reports whether the member's data directory has already been
// initialised (its WAL exists). etcd creates <DataDir>/member on first start and,
// once present, recovers cluster membership from the WAL and ignores the
// initial-cluster flag - so the initial-cluster requirement only applies to a
// fresh member.
func (c *Config) memberDirExists() bool {
	if c.DataDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(c.DataDir, "member"))
	return err == nil
}
