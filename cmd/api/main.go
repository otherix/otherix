// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/api/agentclient"
	clustermembers "github.com/otherix/otherix/internal/api/handlers/clustermembers"
	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	networkshandlers "github.com/otherix/otherix/internal/api/handlers/networks"
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
	st := etcdstore.New(cli)

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

// ensureClusterCAForJoin fetches the cluster CA from an existing replica and
// persists it to disk when a join node has no CA yet, returning the etcd
// initial-cluster string computed from the membership the join call reports. On
// a restart (CA already on disk) it is a no-op and returns the configured
// initial-cluster, so the fetch happens exactly once per joining replica. The CA
// cert + key land at the cluster_ca paths, ready for the on-disk provisioning
// step that follows.
func ensureClusterCAForJoin(ctx context.Context, cfg *config.APIConfig, log *slog.Logger) (string, error) {
	if fileExists(cfg.ClusterCA.CertFile) && fileExists(cfg.ClusterCA.KeyFile) {
		log.Info("cluster CA already on disk; skipping join fetch")
		return cfg.Etcd.InitialCluster, nil
	}

	jc := cfg.ClusterJoin
	token, err := resolveClusterJoinToken(jc)
	if err != nil {
		return "", err
	}
	if jc.CPURL == "" || jc.CAFingerprint == "" {
		return "", errors.New("cluster_join.cp_url and cluster_join.ca_fingerprint are required to join with no CA on disk")
	}
	timeout := jc.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	res, err := api.FetchClusterCA(ctx, api.ClusterJoinFetchParams{
		CPURL:         jc.CPURL,
		Token:         token,
		CAFingerprint: jc.CAFingerprint,
		PeerURL:       cfg.Etcd.PeerURL,
		Timeout:       timeout,
	}, log)
	if err != nil {
		return "", err
	}
	if err := auth.WriteCertCacheAtomic(cfg.ClusterCA.CertFile, cfg.ClusterCA.KeyFile, res.CA.CertPEM, res.CA.KeyPEM); err != nil {
		return "", fmt.Errorf("persist fetched cluster CA: %v", err)
	}
	log.Info("persisted cluster CA fetched on join",
		"fingerprint_sha256", hex.EncodeToString(res.CA.Fingerprint),
		"cert_file", cfg.ClusterCA.CertFile)

	return buildInitialCluster(res.Members, cfg.Etcd.Name, cfg.Etcd.PeerURL), nil
}

// buildInitialCluster renders the etcd initial-cluster "name=peer,..." from the
// membership returned by the cluster-join call. The just-registered learner (self)
// is echoed back in that membership with an empty etcd name (a learner has no name
// until it starts), so the trailing add() keys self by the configured name, and
// the peer-URL-keyed order map lists self exactly once instead of twice.
func buildInitialCluster(members []api.ClusterMemberRef, selfName, selfPeerURL string) string {
	names := make(map[string]string)
	order := make([]string, 0, len(members)+1)
	add := func(peer, name string) {
		if peer == "" {
			return
		}
		// Key by the canonical peer URL so a non-canonical configured peer_url
		// (e.g. a trailing slash) cannot key differently from etcd's echoed form
		// and list self twice, which would render an invalid initial-cluster.
		key := canonPeerURL(peer)
		if _, ok := names[key]; !ok {
			order = append(order, key)
		}
		names[key] = name
	}
	for _, m := range members {
		add(m.PeerURL, m.Name)
	}
	add(selfPeerURL, selfName)
	parts := make([]string, 0, len(order))
	for _, peer := range order {
		parts = append(parts, names[peer]+"="+peer)
	}
	return strings.Join(parts, ",")
}

// canonPeerURL returns the canonical string form of a peer URL so two spellings
// of the same endpoint (e.g. with and without a trailing slash) compare equal.
// A peer URL that does not parse is returned unchanged rather than dropped.
// url.Parse normalizes scheme/host but preserves a trailing slash, so trim a
// lone trailing slash on an otherwise-empty path - the form etcd echoes for a
// bare peer endpoint - to keep "host:2380/" and "host:2380" from keying apart.
func canonPeerURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	if u.Path == "/" && u.RawQuery == "" && u.Fragment == "" {
		u.Path = ""
	}
	return u.String()
}

// resolveClusterJoinToken reads the cluster join token from the configured
// path (preferred) or the inline value. Exactly one must be set.
func resolveClusterJoinToken(jc config.ClusterJoinConfig) (string, error) {
	switch {
	case jc.TokenPath != "" && jc.Token != "":
		return "", errors.New("set only one of cluster_join.token or cluster_join.token_path")
	case jc.TokenPath != "":
		b, err := os.ReadFile(jc.TokenPath)
		if err != nil {
			return "", fmt.Errorf("read cluster_join.token_path: %v", err)
		}
		return strings.TrimSpace(string(b)), nil
	case jc.Token != "":
		return jc.Token, nil
	default:
		return "", errors.New("cluster_join.token or cluster_join.token_path is required to join with no CA on disk")
	}
}

// fileExists reports whether path exists as a regular readable entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runServe handles the post-bootstrap serving lifecycle - extracted from run()
// to keep cyclomatic complexity under gocyclo's ceiling. Threading: bootstrap
// hooks → CP cert load → agent client → worker runtime → HTTP server. Each
// step's failure path returns after wrapping the error.
func runServe(ctx context.Context, cfg *config.APIConfig, st *etcdstore.Store, authSvc *auth.Service, caMaterial auth.ClusterCAResult, log *slog.Logger) error {
	if err := runBootstrapHooks(ctx, st, caMaterial, cfg.Network, log); err != nil {
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

	// The loopback membership client drives the cluster-join learner registration,
	// the admin member routes, and the always-on promote loop. It targets the local
	// member's client URL, so every replica forwards reconfiguration to the etcd
	// leader.
	membership := etcd.NewMembershipClient(cfg.Etcd.ClientURL, log)

	// The promote loop runs on every replica independent of cfg.Workers.Enabled:
	// membership convergence is a cluster concern, not a job-dispatch one. On a
	// single-node cluster each tick is a cheap no-op.
	var promoteWG sync.WaitGroup
	promoteWG.Add(1)
	go func() {
		defer promoteWG.Done()
		clustermembers.RunPromoteLoop(ctx, membership, 15*time.Second, log)
	}()

	// The etcd store self-enqueues (EnqueueTask writes the job inline) and
	// tasks.cancel cancels through the store, so there is no queue client to pass.
	server, err := api.NewServer(
		*cfg, st, agentClient,
		vmshandlers.LifecycleDeps{AgentClient: agentClient},
		vmshandlers.ConsoleDeps{AgentClient: agentClient, AccessMode: cfg.Console.AccessMode},
		authSvc, material, membership, log)
	if err != nil {
		return fmt.Errorf("server init: %v", err)
	}
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("server: %v", err)
	}

	stopWorkers()
	promoteWG.Wait()

	log.Info("shutting down")
	return nil
}

// runBootstrapHooks runs the post-start / pre-serve bootstrap hooks in the
// canonical order: BootstrapAdmin first (seeds the first admin user), then
// BootstrapClusterCA (syncs the on-disk cluster CA into etcd so the /v1/ca
// endpoint and the Step 2 CSR signer have an active row), then
// SeedOverlaySupernet (writes the cluster overlay supernet first-writer-wins so
// agent WG overlay allocation has a supernet to carve /24s from), then
// SeedVNIRange (writes the VXLAN VNI allocation bounds first-writer-wins), then
// SeedUnderlayMTU (writes the physical underlay MTU first-writer-wins; the
// overlay inner MTU and otwg0 MTU derive from it). All hooks are idempotent -
// repeat boots observe existing rows and no-op.
func runBootstrapHooks(ctx context.Context, st *etcdstore.Store, caMaterial auth.ClusterCAResult, netCfg config.NetworkConfig, log *slog.Logger) error {
	if err := api.BootstrapAdmin(ctx, st, log); err != nil {
		return fmt.Errorf("bootstrap admin: %v", err)
	}
	if err := api.BootstrapClusterCA(ctx, st, caMaterial, log); err != nil {
		return fmt.Errorf("bootstrap cluster CA: %v", err)
	}
	if err := st.SeedOverlaySupernet(ctx, netCfg.OverlaySupernet); err != nil {
		return fmt.Errorf("seed overlay supernet: %v", err)
	}
	if err := st.SeedVNIRange(ctx, netCfg.VNIRange.Min, netCfg.VNIRange.Max); err != nil {
		return fmt.Errorf("seed vni range: %v", err)
	}
	if err := st.SeedUnderlayMTU(ctx, netCfg.UnderlayMTU); err != nil {
		return fmt.Errorf("seed underlay mtu: %v", err)
	}
	// A cluster seeded under an older binary (the floor was 1280 before it was
	// raised to 1390) can carry an underlay MTU in [1280,1389] that derives a
	// sub-1280 overlay MTU - below the RFC 8200 IPv6 minimum. UnderlayMTU returns
	// such a seed verbatim (it is immutable, and silently clamping it up would push
	// the overlay MTU too high and fragment), so surface it loudly at boot instead.
	underlay, err := st.UnderlayMTU(ctx)
	if err != nil {
		return fmt.Errorf("read underlay mtu: %v", err)
	}
	warnIfUnderlayMTUBelowFloor(underlay, log)
	return nil
}

// warnIfUnderlayMTUBelowFloor logs a loud WARN when the seeded underlay MTU is
// strictly below the etcdstore.MinUnderlayMTU floor (1390), naming the seed, the
// derived overlay MTU, the floor, and the renumber procedure. It returns whether
// it warned. It does NOT mutate the seed - the seed is immutable and clamping it
// up would push the derived overlay MTU too high and fragment; the fix is the
// operator-driven renumber documented in docs/architecture.md ("Operations:
// overlay MTU and VNI range"). The default 1500 and the floor itself are silent.
func warnIfUnderlayMTUBelowFloor(underlay int32, log *slog.Logger) bool {
	if !etcdstore.UnderlayMTUBelowFloor(underlay) {
		return false
	}
	log.Warn("seeded underlay_mtu is below the 1390 floor; the derived overlay_mtu falls under the 1280-byte ipv6 minimum link mtu (rfc 8200). the seed is immutable and is not corrected on read; renumber per docs/architecture.md \"Operations: overlay MTU and VNI range\"",
		slog.Int("underlay_mtu", int(underlay)),
		slog.Int("overlay_mtu", int(etcdstore.DerivedOverlayMTU(underlay))),
		slog.Int("floor", int(etcdstore.MinUnderlayMTU)))
	return true
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
		vmshandlers.DeleteHandler(st, vmshandlers.NewAgentVMDeleteExecutor(agentClient), log, cfg.Workers.Heartbeat.GoneGrace))

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
			GoneGrace:      cfg.Workers.Heartbeat.GoneGrace,
			Interval:       cfg.Workers.Heartbeat.Interval,
		}, log))

	s.Register("auth.refresh_token_cleanup", time.Hour, false,
		auth.RefreshTokenCleanupFunc(st, log))

	s.Register("idempotency.cleanup", time.Hour, false,
		middleware.IdempotencyCleanupFunc(st, log))

	s.Register("networks.cleanup", time.Hour, false,
		networkshandlers.CleanupFunc(st, log))

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

		// Peer mTLS files are set by the caller from ProvisionPeerCert's
		// result (operator override or auto-generated), not copied here.

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
