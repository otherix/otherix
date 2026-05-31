// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/zap/zapcore"
)

// readyDeadline bounds how long Start waits for a member to signal ready
// before giving up. A member that never reaches ready (e.g. a peer cert
// rejected during join) would otherwise block startup indefinitely.
const readyDeadline = 60 * time.Second

// Runtime owns an embedded etcd member's lifecycle. Construct it with Start
// and release it with Stop; Etcd exposes the underlying member so higher
// layers can build a KV client over it.
type Runtime struct {
	e   *embed.Etcd
	log *slog.Logger
}

// Start builds an embed.Config from cfg, starts the member, and returns once
// the server has signalled ready (or the startup deadline / ctx elapses). On
// any failure the partially-started member is closed before returning.
func Start(ctx context.Context, cfg *Config, log *slog.Logger) (*Runtime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("etcd config: %w", err)
	}
	ec, err := buildEmbedConfig(cfg, log)
	if err != nil {
		return nil, err
	}
	log.Info("starting embedded etcd",
		"name", cfg.Name,
		"mode", cfg.Mode,
		"data_dir", cfg.DataDir,
		"peer_url", cfg.PeerURL,
		"client_url", cfg.ClientURL,
		"cluster_state", ec.ClusterState,
	)

	e, err := embed.StartEtcd(ec)
	if err != nil {
		return nil, fmt.Errorf("embed start: %v", err)
	}
	if err := waitReady(ctx, e, cfg.Name, log); err != nil {
		e.Close()
		return nil, err
	}
	return &Runtime{e: e, log: log}, nil
}

// Etcd returns the underlying embedded member. Higher layers build a KV client
// over it (in-process via the member's server, or over the client URL).
func (r *Runtime) Etcd() *embed.Etcd {
	return r.e
}

// Stop shuts the member down, abandoning the close after timeout.
//
// Finding (spike): embed.Etcd.Close() can block indefinitely on a member that
// never reached ready (e.g. a node that failed to join because its peer cert
// was rejected) - the graceful path waits on internals a never-started server
// never signals. Bounding the close keeps one wedged member from hanging CP
// shutdown.
func (r *Runtime) Stop(timeout time.Duration) {
	if r == nil || r.e == nil {
		return
	}
	r.log.Info("stopping embedded etcd")
	done := make(chan struct{})
	go func() {
		r.e.Close()
		close(done)
	}()
	select {
	case <-done:
		r.log.Info("embedded etcd stopped")
	case <-time.After(timeout):
		r.log.Warn("embedded etcd close timed out; abandoning", "timeout", timeout)
	}
}

// buildEmbedConfig translates a Config into an embed.Config, including the
// zap-to-slog logger bridge so etcd's internal logs flow through Otherix's
// slog stream rather than a separate zap sink.
func buildEmbedConfig(cfg *Config, log *slog.Logger) (*embed.Config, error) {
	peerURL, err := url.Parse(cfg.PeerURL)
	if err != nil {
		return nil, fmt.Errorf("parse peer-url: %v", err)
	}
	clientURL, err := url.Parse(cfg.ClientURL)
	if err != nil {
		return nil, fmt.Errorf("parse client-url: %v", err)
	}

	clusterState := embed.ClusterStateFlagNew
	if cfg.Mode == ModeJoin {
		clusterState = embed.ClusterStateFlagExisting
	}

	ec := embed.NewConfig()
	ec.Name = cfg.Name
	ec.Dir = cfg.DataDir
	ec.ListenPeerUrls = []url.URL{*peerURL}
	ec.AdvertisePeerUrls = []url.URL{*peerURL}
	ec.ListenClientUrls = []url.URL{*clientURL}
	ec.AdvertiseClientUrls = []url.URL{*clientURL}
	ec.InitialCluster = cfg.initialClusterString()
	ec.InitialClusterToken = cfg.ClusterToken
	ec.ClusterState = clusterState

	// Bound MVCC history growth. etcd defaults to compaction off (retention
	// "0"); a non-zero retention keeps a long-running member from accumulating
	// unbounded key revisions. Default periodic / 1h when unset.
	ec.AutoCompactionMode = cfg.CompactionMode
	if ec.AutoCompactionMode == "" {
		ec.AutoCompactionMode = "periodic"
	}
	ec.AutoCompactionRetention = cfg.CompactionRetention
	if ec.AutoCompactionRetention == "" {
		ec.AutoCompactionRetention = "1h"
	}

	ec.LogLevel = "warn"
	// Route etcd's zap logs into Otherix's slog stream. LogOutputs is left
	// unset because the bridge owns the destination.
	ec.ZapLoggerBuilder = zapBuilderFromSlog(log, zapcore.WarnLevel)

	// Peer (Raft) mTLS. ClientCertAuth makes etcd require and verify a client
	// cert on every inbound peer connection against the cluster CA, so a member
	// without a cluster-issued cert cannot join the Raft transport - the
	// protection for replicas talking over public networks.
	if cfg.peerTLSEnabled() {
		ec.PeerTLSInfo.CertFile = cfg.PeerCertFile
		ec.PeerTLSInfo.KeyFile = cfg.PeerKeyFile
		ec.PeerTLSInfo.TrustedCAFile = cfg.PeerCAFile
		ec.PeerTLSInfo.ClientCertAuth = true
	}

	return ec, nil
}

// waitReady blocks until the member signals ready, errors, or the deadline or
// context elapses.
func waitReady(ctx context.Context, e *embed.Etcd, name string, log *slog.Logger) error {
	select {
	case <-e.Server.ReadyNotify():
		log.Info("embedded etcd ready", "name", name)
		return nil
	case err := <-e.Err():
		return fmt.Errorf("embed startup failed: %v", err)
	case <-time.After(readyDeadline):
		return fmt.Errorf("embed startup timeout after %s", readyDeadline)
	case <-ctx.Done():
		return ctx.Err()
	}
}
