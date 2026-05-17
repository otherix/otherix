// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/api/agentclient"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/logger"
	"github.com/otherix/otherix/internal/store"
	"github.com/otherix/otherix/internal/store/migrate"
	"github.com/otherix/otherix/internal/version"
)

const componentName = "api"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is the body of main extracted so that deferred cleanup runs before exit.
func run() error {
	configPath := flag.String("config", "/etc/otherix/api.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	migrateAction := flag.String("migrate-action", "", "if set, run migration and exit (up|down|status)")
	hashPassword := flag.String("hash-password", "", "if set, print an argon2id hash of the given plaintext and exit (for bootstrap)")
	flag.Parse()

	if *showVersion {
		v := version.Current()
		fmt.Printf("otherix-%s %s (commit %s, built %s)\n", componentName, v.Version, v.Commit, v.Date)
		return nil
	}

	if *hashPassword != "" {
		hash, err := auth.HashPassword(*hashPassword)
		if err != nil {
			return fmt.Errorf("hash password: %v", err)
		}
		fmt.Println(hash)
		return nil
	}

	cfg, err := config.LoadAPI(*configPath)
	if err != nil {
		return fmt.Errorf("config: %v", err)
	}

	log := logger.WithComponent(logger.New(cfg.Logger), componentName)
	logger.SetDefault(log)

	v := version.Current()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *migrateAction != "" {
		if err := runMigration(ctx, cfg.Database.DSN, migrate.Action(*migrateAction), log); err != nil {
			log.Error("migration failed", "action", *migrateAction, "error", err)
			return errors.New("migration failed")
		}
		log.Info("migration completed", "action", *migrateAction)
		return nil
	}

	log.Info("starting",
		"binary", "otherix-"+componentName,
		"version", v.Version,
		"commit", v.Commit,
		"listen", cfg.Server.Listen,
	)

	for _, msg := range cfg.Placement.Warnings() {
		log.Warn("placement config", "warning", msg)
	}

	s, err := store.NewStore(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("store init: %v", err)
	}
	defer s.Close()

	authSvc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte(cfg.Auth.JWTSecret),
		JWTAccessTTL: cfg.Auth.JWTAccessTTL,
		RefreshTTL:   cfg.Auth.JWTRefreshTTL,
	}, s)
	if err != nil {
		return fmt.Errorf("auth init: %v", err)
	}

	return runServe(ctx, cfg, s, authSvc, log)
}

// runServe handles the post-bootstrap serving lifecycle — extracted
// from run() к keep cyclomatic complexity under gocyclo's ceiling.
// Threading is straightforward: bootstrap hooks → CP cert load → agent
// client → river → HTTP server. Каждый step's failure path returns
// after wrapping the error.
func runServe(ctx context.Context, cfg *config.APIConfig, s *store.Store, authSvc *auth.Service, log *slog.Logger) error {
	if err := runBootstrapHooks(ctx, s, log); err != nil {
		return err
	}

	material, err := api.LoadOrGenerateCPCert(ctx, s, *cfg, log)
	if err != nil {
		return fmt.Errorf("cp cert: %v", err)
	}

	agentClient, err := buildAgentClient(cfg, material, log)
	if err != nil {
		return fmt.Errorf("agent client: %v", err)
	}

	riverClient, stopRiver, err := startRiver(ctx, cfg, s, agentClient, log)
	if err != nil {
		return err
	}

	server, err := api.NewServer(
		*cfg, s, riverClient, agentClient,
		vmshandlers.LifecycleDeps{AgentClient: agentClient},
		vmshandlers.ConsoleDeps{AgentClient: agentClient, AccessMode: cfg.Console.AccessMode},
		authSvc, material, log)
	if err != nil {
		return fmt.Errorf("server init: %v", err)
	}
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("server: %v", err)
	}

	stopRiver()

	log.Info("shutting down")
	return nil
}

// runBootstrapHooks runs the post-migration / pre-serve bootstrap
// hooks в the canonical order: BootstrapAdmin first (seeds the first
// admin user), then BootstrapClusterCA (provisions the cluster CA so
// the /v1/ca endpoint и the Step 2 CSR signer have an active row).
// Both hooks are idempotent — repeat boots observe existing rows и
// no-op.
func runBootstrapHooks(ctx context.Context, s *store.Store, log *slog.Logger) error {
	if err := api.BootstrapAdmin(ctx, s, log); err != nil {
		return fmt.Errorf("bootstrap admin: %v", err)
	}
	if err := api.BootstrapClusterCA(ctx, s, log); err != nil {
		return fmt.Errorf("bootstrap cluster CA: %v", err)
	}
	return nil
}

// startRiver constructs the in-process river client and starts it
// when cfg.Workers.Enabled. Returns the client (always non-nil so
// `tasks.cancel` can issue JobCancelTx against river_job even with
// workers disabled), and a stop closure the caller invokes after the
// HTTP servers have drained. Disabled-mode skips Start; the stop
// closure is a no-op.
//
// Stop runs against a fresh background context bounded by
// ShutdownGrace because the parent ctx is already cancelled by the
// time the caller invokes the closure.
func startRiver(ctx context.Context, cfg *config.APIConfig, s *store.Store, agentClient *agentclient.Client, log *slog.Logger) (*river.Client[pgx.Tx], func(), error) {
	var (
		scanExecutor        storagepoolshandlers.ScanExecutor
		importExecutor      storagepoolshandlers.ImportExecutor
		vmCreateExecutor    vmshandlers.CreateExecutor
		vmDeleteExecutor    vmshandlers.DeleteExecutor
		vmLifecycleExecutor vmshandlers.LifecycleExecutor
	)
	if cfg.Workers.Enabled {
		// Workers running require the agent client: scan / import / vm
		// executors all talk to agents over mTLS. Booting Enabled=true
		// without the client would either silently wedge tasks or
		// (on a stub path) ship fake bytes — neither is acceptable
		// in production.
		if agentClient == nil {
			return nil, nil, errors.New("workers.enabled requires agent_client.enabled — provision mTLS material")
		}
		scanExecutor = storagepoolshandlers.NewAgentScanExecutor(agentClient)
		importExecutor = storagepoolshandlers.NewAgentImportExecutor(agentClient)
		vmCreateExecutor = vmshandlers.NewAgentVMCreateExecutor(agentClient)
		vmDeleteExecutor = vmshandlers.NewAgentVMDeleteExecutor(agentClient)
		vmLifecycleExecutor = vmshandlers.NewAgentVMLifecycleExecutor(agentClient)
	}
	// BuildRiverClient validates MaxWorkers > 0. When Workers.Enabled
	// is false we still build the client so tasks.cancel can issue
	// JobCancelTx, but the queue is never Started — MaxWorkers is
	// irrelevant. Synthesise a minimum value to pass the check while
	// preserving the strict validation for the explicit Enabled=true +
	// MaxWorkers=0 misconfig case.
	workersCfg := cfg.Workers
	if !workersCfg.Enabled && workersCfg.MaxWorkers <= 0 {
		workersCfg.MaxWorkers = 1
	}
	c, err := api.BuildRiverClient(api.RiverDeps{
		Pool:                s.Pool(),
		Cfg:                 workersCfg,
		Logger:              log,
		Store:               s,
		ScanExecutor:        scanExecutor,
		ImportExecutor:      importExecutor,
		VMCreateExecutor:    vmCreateExecutor,
		VMDeleteExecutor:    vmDeleteExecutor,
		VMLifecycleExecutor: vmLifecycleExecutor,
		AgentClient:         agentClient,
		PressureDisk:        cfg.Placement.Pressure.Disk,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("river client: %v", err)
	}
	if !cfg.Workers.Enabled {
		return c, func() {}, nil
	}
	if err := c.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("river start: %v", err)
	}
	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
		defer cancel()
		if err := c.Stop(stopCtx); err != nil {
			log.Error("river stop", "error", err)
		}
	}
	return c, stop, nil
}

// buildAgentClient constructs the *agentclient.Client used by both
// the scan executor and the storage_image.delete handler.
// Returns (nil, nil) when AgentClient.Enabled is
// false — the api binary still boots so HTTP-only smoke testing
// stays available; the consumer paths each emit their own
// degradation envelope (scan tasks pile up in pending; the delete
// handler returns 502 agent_unreachable on the count==0 path).
//
// mTLS material (replica's leaf cert + cluster CA trust anchor)
// flows in via material — produced upstream по LoadOrGenerateCPCert.
//
// Construction errors at this stage (config validation, empty
// material when AgentClient.Enabled=true) are boot-time fatal: the
// api binary must not start with а half-configured agent client.
func buildAgentClient(cfg *config.APIConfig, material api.TLSMaterial, log *slog.Logger) (*agentclient.Client, error) {
	if !cfg.AgentClient.Enabled {
		log.Info("agent client disabled; storage_image.delete and scan workers will surface degraded responses")
		return nil, nil
	}
	if material.Skipped() || len(material.Cert.Certificate) == 0 || material.ClusterCA == nil {
		return nil, errors.New("agent_client.enabled=true requires valid CP cert material (LoadOrGenerateCPCert produced а skipped/empty result — check agent_server / agent_client config consistency)")
	}
	client, err := agentclient.New(cfg.AgentClient, material.Cert, material.ClusterCA)
	if err != nil {
		return nil, err
	}
	log.Info("agent client wired",
		slog.Duration("poll_interval", cfg.AgentClient.PollInterval),
		slog.Duration("poll_max_interval", cfg.AgentClient.PollMaxInterval),
		slog.Duration("timeout", cfg.AgentClient.Timeout))
	return client, nil
}

// runMigration opens a short-lived pool just for migrations. The api-server's
// regular Store is intentionally not used here — its constructor pings and
// configures pool sizing, neither of which a CLI one-shot needs.
func runMigration(ctx context.Context, dsn string, action migrate.Action, log *slog.Logger) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to %q: %v", redactDSN(dsn), err)
	}
	defer pool.Close()
	return migrate.Run(ctx, pool, action, log)
}

// redactDSN strips userinfo from a DSN for safe logging. Best-effort: if the
// DSN does not parse as URL form (e.g. libpq keyword/value style), returns
// "<dsn>" rather than risk leaking the raw string.
func redactDSN(dsn string) string {
	const placeholder = "<dsn>"
	if dsn == "" {
		return placeholder
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return placeholder
	}
	if u.User == nil {
		return dsn
	}
	u.User = url.User("***")
	return u.String()
}
