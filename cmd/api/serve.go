// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/logger"
	"github.com/otherix/otherix/internal/version"
)

// newServeCommand builds the `otherix-api serve` subcommand. It is the
// binary's default (the bare invocation routes here via root.RunE) so the
// systemd unit keeps working without a subcommand.
func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control-plane API server (default).",
		Args:  cobra.NoArgs,
		RunE:  runServeCmd,
	}
	cmd.Flags().String("config", defaultConfigPath, "path to config file")
	cmd.Flags().Bool("version", false, "print version and exit")
	cmd.Flags().String("hash-password", "", "if set, print an argon2id hash of the given plaintext and exit (for bootstrap)")
	return cmd
}

// runServeCmd is the serve subcommand body, extracted so deferred cleanup runs
// before main exits: cobra returns this error to Execute, main prints and
// exits. The lifecycle is relocated verbatim from the former run().
func runServeCmd(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	showVersion, _ := cmd.Flags().GetBool("version")
	hashPassword, _ := cmd.Flags().GetString("hash-password")

	if showVersion {
		v := version.Current()
		fmt.Printf("otherix-%s %s (commit %s, built %s)\n", componentName, v.Version, v.Commit, v.Date)
		return nil
	}

	if hashPassword != "" {
		hash, err := auth.HashPassword(hashPassword)
		if err != nil {
			return fmt.Errorf("hash password: %v", err)
		}
		fmt.Println(hash)
		return nil
	}

	cfg, err := config.LoadAPI(configPath)
	if err != nil {
		return fmt.Errorf("config: %v", err)
	}

	log := logger.WithComponent(logger.New(cfg.Logger), componentName)
	logger.SetDefault(log)

	v := version.Current()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("starting",
		"binary", "otherix-"+componentName,
		"version", v.Version,
		"commit", v.Commit,
		"listen", cfg.Server.Listen,
	)

	for _, msg := range cfg.Placement.Warnings() {
		log.Warn("placement config", "warning", msg)
	}

	// Resolve an auto/empty peer URL to the host's routable IPv4 before any
	// consumer reads it (peer cert SANs, initial-cluster, the etcd member).
	// This keeps a single-node default HA-ready without operator input.
	if err := resolvePeerURL(cfg, log); err != nil {
		return err
	}

	// A join node with no cluster CA on disk fetches it from an existing
	// replica before anything else: the peer plane needs the CA pre-start and
	// a joiner must adopt the cluster's shared CA, not mint its own.
	if cfg.Etcd.Mode == "join" {
		ic, err := ensureClusterCAForJoin(ctx, cfg, log)
		if err != nil {
			return fmt.Errorf("fetch cluster CA for join: %v", err)
		}
		cfg.Etcd.InitialCluster = ic
	}

	// Provision the cluster CA on disk before etcd starts: the peer (Raft)
	// mTLS plane needs a CA-signed cert pre-start. A bootstrap / single node
	// generates it on first boot and reloads it on restart; a join node loads
	// the CA fetched above (allowGenerate is false for join, so a missing CA
	// fails fast rather than forking a new trust root).
	caMaterial, caGenerated, err := auth.LoadOrGenerateClusterCAOnDisk(
		cfg.ClusterCA.CertFile, cfg.ClusterCA.KeyFile, cfg.Etcd.Mode != "join", time.Now())
	if err != nil {
		return fmt.Errorf("provision cluster CA on disk: %v", err)
	}
	log.Info("cluster CA on disk",
		"generated", caGenerated,
		"fingerprint_sha256", hex.EncodeToString(caMaterial.Fingerprint),
		"cert_file", cfg.ClusterCA.CertFile)

	// Provision the etcd peer (Raft) mTLS material from the on-disk CA before
	// the member starts. Peer mTLS is always on (uniform across single-node and
	// HA); with no operator override the peer leaf is auto-generated each boot.
	peerMat, err := api.ProvisionPeerCert(caMaterial, api.PeerCertParams{
		PeerURL:          cfg.Etcd.PeerURL,
		OperatorCertFile: cfg.Etcd.PeerCertFile,
		OperatorKeyFile:  cfg.Etcd.PeerKeyFile,
		OperatorCAFile:   cfg.Etcd.PeerCAFile,
		GenCertPath:      filepath.Join(cfg.Etcd.PeerAutoDir, "peer.crt"),
		GenKeyPath:       filepath.Join(cfg.Etcd.PeerAutoDir, "peer.key"),
		GenCAPath:        filepath.Join(cfg.Etcd.PeerAutoDir, "peer-ca.crt"),
	}, time.Now(), log)
	if err != nil {
		return fmt.Errorf("provision peer cert: %v", err)
	}

	// Start the embedded etcd member, then build the KV client and the
	// etcd-backed store over it. Deferred cleanup runs LIFO: the client closes
	// before the member stops, both bounded by ShutdownGrace.
	etcdCfg := etcdConfigFromAPI(cfg.Etcd)
	etcdCfg.PeerCertFile = peerMat.CertFile
	etcdCfg.PeerKeyFile = peerMat.KeyFile
	etcdCfg.PeerCAFile = peerMat.CAFile
	rt, err := etcd.Start(ctx, etcdCfg, log)
	if err != nil {
		return fmt.Errorf("etcd start: %v", err)
	}
	defer rt.Stop(cfg.Server.ShutdownGrace)

	cli := etcd.NewClient(rt)
	defer func() {
		// Close races the shutdown context cancellation; a context.Canceled
		// here is the benign result of a clean shutdown, not a failure.
		if cerr := cli.Close(); cerr != nil && !errors.Is(cerr, context.Canceled) {
			log.Error("etcd client close", "error", cerr)
		}
	}()
	st := etcdstore.New(cli, etcdstore.WithLogger(log), etcdstore.WithRefreshTokenTTL(cfg.Auth.JWTRefreshTTL), etcdstore.WithDownPathStaleness(cfg.Workers.Heartbeat.DownPathStaleness))

	authSvc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte(cfg.Auth.JWTSecret),
		JWTAccessTTL: cfg.Auth.JWTAccessTTL,
		RefreshTTL:   cfg.Auth.JWTRefreshTTL,
	}, st)
	if err != nil {
		return fmt.Errorf("auth init: %v", err)
	}

	return runServe(ctx, cfg, st, authSvc, caMaterial, log)
}
