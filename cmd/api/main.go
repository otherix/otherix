// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/api/agentclient"
	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/logger"
	"github.com/otherix/otherix/internal/version"
	"github.com/otherix/otherix/internal/worker"
)

const componentName = "api"

// workerMaxAttempts is the per-kind retry budget the dispatcher applies to every
// async task kind. Mirrors the river MaxAttempts (25) set at enqueue time across
// vm.create / vm.delete / vm.* lifecycle / storage_pool.scan / storage_image.import.
const workerMaxAttempts = 25

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

	log.Info("starting",
		"binary", "otherix-"+componentName,
		"version", v.Version,
		"commit", v.Commit,
		"listen", cfg.Server.Listen,
	)

	for _, msg := range cfg.Placement.Warnings() {
		log.Warn("placement config", "warning", msg)
	}

	// Start the embedded etcd member, then build the KV client and the
	// etcd-backed store over it. Deferred cleanup runs LIFO: the client closes
	// before the member stops, both bounded by ShutdownGrace.
	rt, err := etcd.Start(ctx, etcdConfigFromAPI(cfg.Etcd), log)
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
	st := etcdstore.New(cli)

	authSvc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte(cfg.Auth.JWTSecret),
		JWTAccessTTL: cfg.Auth.JWTAccessTTL,
		RefreshTTL:   cfg.Auth.JWTRefreshTTL,
	}, st)
	if err != nil {
		return fmt.Errorf("auth init: %v", err)
	}

	return runServe(ctx, cfg, st, authSvc, log)
}

// runServe handles the post-bootstrap serving lifecycle - extracted from run()
// to keep cyclomatic complexity under gocyclo's ceiling. Threading: bootstrap
// hooks → CP cert load → agent client → worker runtime → HTTP server. Each
// step's failure path returns after wrapping the error.
func runServe(ctx context.Context, cfg *config.APIConfig, st *etcdstore.Store, authSvc *auth.Service, log *slog.Logger) error {
	if err := runBootstrapHooks(ctx, st, log); err != nil {
		return err
	}

	material, err := api.LoadOrGenerateCPCert(ctx, st, *cfg, log)
	if err != nil {
		return fmt.Errorf("cp cert: %v", err)
	}

	agentClient, err := buildAgentClient(cfg, material, log)
	if err != nil {
		return fmt.Errorf("agent client: %v", err)
	}

	stopWorkers, err := startWorkers(ctx, cfg, st, agentClient, log)
	if err != nil {
		return err
	}

	// The etcd store self-enqueues (EnqueueTask writes the job inline) and
	// tasks.cancel cancels through the store, so there is no queue client to pass.
	server, err := api.NewServer(
		*cfg, st, agentClient,
		vmshandlers.LifecycleDeps{AgentClient: agentClient},
		vmshandlers.ConsoleDeps{AgentClient: agentClient, AccessMode: cfg.Console.AccessMode},
		authSvc, material, log)
	if err != nil {
		return fmt.Errorf("server init: %v", err)
	}
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("server: %v", err)
	}

	stopWorkers()

	log.Info("shutting down")
	return nil
}

// runBootstrapHooks runs the post-start / pre-serve bootstrap hooks in the
// canonical order: BootstrapAdmin first (seeds the first admin user), then
// BootstrapClusterCA (provisions the cluster CA so the /v1/ca endpoint and the
// Step 2 CSR signer have an active row). Both hooks are idempotent - repeat
// boots observe existing rows and no-op.
func runBootstrapHooks(ctx context.Context, st *etcdstore.Store, log *slog.Logger) error {
	if err := api.BootstrapAdmin(ctx, st, log); err != nil {
		return fmt.Errorf("bootstrap admin: %v", err)
	}
	if err := api.BootstrapClusterCA(ctx, st, log); err != nil {
		return fmt.Errorf("bootstrap cluster CA: %v", err)
	}
	return nil
}

// startWorkers launches the etcd job dispatcher and the periodic scheduler when
// cfg.Workers.Enabled. Both run for the lifetime of ctx; the returned closure
// blocks until they have drained in-flight work after ctx is cancelled - the
// caller invokes it once the HTTP servers have stopped. Disabled-mode runs
// neither (async tasks stay pending, periodic maintenance does not run) and the
// closure is a no-op.
func startWorkers(ctx context.Context, cfg *config.APIConfig, st *etcdstore.Store, agentClient *agentclient.Client, log *slog.Logger) (func(), error) {
	if !cfg.Workers.Enabled {
		log.Info("workers disabled; async tasks will remain pending and periodic maintenance will not run")
		return func() {}, nil
	}
	// Workers require the agent client: the create / delete / lifecycle / scan /
	// import handlers all talk to agents over mTLS. Booting Enabled=true without
	// the client would wedge every task in pending.
	if agentClient == nil {
		return nil, errors.New("workers.enabled requires agent_client.enabled - provision mTLS material")
	}

	dispatcher := buildDispatcher(st, agentClient, cfg, log)
	scheduler := buildScheduler(st, cfg, log)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = dispatcher.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		_ = scheduler.Run(ctx)
	}()

	return wg.Wait, nil
}

// buildDispatcher registers the six async task-kind handlers on a dispatcher
// polling the etcd job queue. Each handler reuses the production agent executor
// and the Run-form worker in its owning handler package.
func buildDispatcher(st *etcdstore.Store, agentClient *agentclient.Client, cfg *config.APIConfig, log *slog.Logger) *worker.Dispatcher {
	d := worker.NewDispatcher(st, log, 0 /* default poll interval */, cfg.Workers.MaxWorkers)

	d.Register("vm.create", workerMaxAttempts,
		vmshandlers.CreateHandler(st, vmshandlers.NewAgentVMCreateExecutor(agentClient), log))
	d.Register("vm.delete", workerMaxAttempts,
		vmshandlers.DeleteHandler(st, vmshandlers.NewAgentVMDeleteExecutor(agentClient), log))

	lifecycleExec := vmshandlers.NewAgentVMLifecycleExecutor(agentClient)
	for _, lk := range vmshandlers.LifecycleKinds() {
		d.Register(lk.Kind, workerMaxAttempts,
			vmshandlers.LifecycleHandler(st, lifecycleExec, log, lk.Op, lk.DesiredPhase, lk.RuntimePhase, lk.FailureCode))
	}

	d.Register("storage_pool.scan", workerMaxAttempts,
		storagepoolshandlers.ScanHandler(st, storagepoolshandlers.NewAgentScanExecutor(agentClient), cfg.Placement.Pressure.Disk, log))
	d.Register("storage_image.import", workerMaxAttempts,
		storagepoolshandlers.ImportHandler(st, storagepoolshandlers.NewAgentImportExecutor(agentClient), log))

	return d
}

// buildScheduler registers the periodic maintenance functions on a scheduler.
// Cadences mirror the river periodic registrations: hourly retention sweeps, the
// heartbeat reconcile on the configured interval (run-on-start so a restart
// promotes nodes that kept heartbeating), and the scan trigger when enabled.
func buildScheduler(st *etcdstore.Store, cfg *config.APIConfig, log *slog.Logger) *worker.Scheduler {
	s := worker.NewScheduler(log)

	s.Register("tasks.cleanup", time.Hour, false,
		taskshandlers.CleanupFunc(st, taskshandlers.RetentionConfig{
			Completed: cfg.Workers.Tasks.Retention.Completed,
			Failed:    cfg.Workers.Tasks.Retention.Failed,
		}, log))

	s.Register("heartbeat.reconcile", positiveOr(cfg.Workers.Heartbeat.Interval, 30*time.Second), true,
		heartbeathandlers.ReconcileFunc(st, heartbeathandlers.ReconcileConfig{
			StaleThreshold: cfg.Workers.Heartbeat.StaleThreshold,
			Interval:       cfg.Workers.Heartbeat.Interval,
		}, log))

	s.Register("auth.refresh_token_cleanup", time.Hour, false,
		auth.RefreshTokenCleanupFunc(st, log))

	s.Register("idempotency.cleanup", time.Hour, false,
		middleware.IdempotencyCleanupFunc(st, log))

	if cfg.Workers.StoragePoolScan.Enabled {
		s.Register("storage_pool.scan_trigger", positiveOr(cfg.Workers.StoragePoolScan.Interval, 15*time.Minute), false,
			storagepoolshandlers.ScanTriggerFunc(st, log))
	}

	if cfg.Workers.Backup.Enabled && cfg.Workers.Backup.Dir != "" {
		s.Register("etcd.backup", positiveOr(cfg.Workers.Backup.Interval, 6*time.Hour), false,
			etcd.BackupFunc(cfg.Etcd.ClientURL, cfg.Workers.Backup.Dir, cfg.Workers.Backup.Retention, log))
	}

	return s
}

// positiveOr returns d when it is positive, else fallback. Guards the scheduler
// tickers against a 0 interval (time.NewTicker panics on a non-positive
// duration) when an operator explicitly zeroes a cadence in config.
func positiveOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// etcdConfigFromAPI translates the koanf-bound config.EtcdConfig into the leaf
// internal/etcd.Config. internal/etcd is a leaf package so it does not import
// config; the api binary copies fields here. etcd.Config.Validate (invoked by
// etcd.Start) is the single source of truth for the invariants.
func etcdConfigFromAPI(c config.EtcdConfig) *etcd.Config {
	return &etcd.Config{
		Mode:           etcd.Mode(c.Mode),
		Name:           c.Name,
		DataDir:        c.DataDir,
		PeerURL:        c.PeerURL,
		ClientURL:      c.ClientURL,
		ClusterToken:   c.ClusterToken,
		InitialCluster: c.InitialCluster,
		PeerCertFile:   c.PeerCertFile,
		PeerKeyFile:    c.PeerKeyFile,
		PeerCAFile:     c.PeerCAFile,

		CompactionMode:      c.CompactionMode,
		CompactionRetention: c.CompactionRetention,
	}
}

// buildAgentClient constructs the *agentclient.Client used by both the scan /
// import / vm executors and the storage_image.delete handler. Returns (nil, nil)
// when AgentClient.Enabled is false - the api binary still boots so HTTP-only
// smoke testing stays available; the consumer paths each emit their own
// degradation envelope.
//
// mTLS material (replica's leaf cert + cluster CA trust anchor) flows in via
// material - produced upstream per LoadOrGenerateCPCert. Construction errors at
// this stage (config validation, empty material when AgentClient.Enabled=true)
// are boot-time fatal: the api binary must not start with a half-configured
// agent client.
func buildAgentClient(cfg *config.APIConfig, material api.TLSMaterial, log *slog.Logger) (*agentclient.Client, error) {
	if !cfg.AgentClient.Enabled {
		log.Info("agent client disabled; storage_image.delete and scan workers will surface degraded responses")
		return nil, nil
	}
	if material.Skipped() || len(material.Cert.Certificate) == 0 || material.ClusterCA == nil {
		return nil, errors.New("agent_client.enabled=true requires valid CP cert material (LoadOrGenerateCPCert produced a skipped/empty result - check agent_server / agent_client config consistency)")
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
