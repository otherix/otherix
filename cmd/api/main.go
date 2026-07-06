// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/blobbroker"
	clustermembers "github.com/otherix/otherix/internal/api/handlers/clustermembers"
	gatewayshandlers "github.com/otherix/otherix/internal/api/handlers/gateways"
	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	migrationshandlers "github.com/otherix/otherix/internal/api/handlers/migrations"
	networkshandlers "github.com/otherix/otherix/internal/api/handlers/networks"
	nodeshandlers "github.com/otherix/otherix/internal/api/handlers/nodes"
	replicationhandlers "github.com/otherix/otherix/internal/api/handlers/replication"
	snapshotshandlers "github.com/otherix/otherix/internal/api/handlers/snapshots"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/netdetect"
	schedpkg "github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/worker"
)

const componentName = "api"

// workerMaxAttempts is the per-kind retry budget the dispatcher applies to every
// async task kind. Mirrors the job-queue MaxAttempts (25) set at enqueue time across
// vm.create / vm.delete / vm.* lifecycle / storage_pool.scan.
const workerMaxAttempts = 25

// nodeDrainMaxAttempts caps redeliveries of the node.drain job. A drain is a
// long-lived saga that resumes from durable state on every redelivery, so it
// gets a larger budget than the per-action kinds. Mirrors the value the drain
// HTTP handler stamps at enqueue.
const nodeDrainMaxAttempts = 50

// The migrations cancel handler reaches its agent seam through a runtime
// type assertion on the shared lifecycle agent client (router.go). That assertion
// is satisfied only because *agentclient.Client carries CancelMigration; this
// compile-time guard turns a future drift of that method into a build break
// rather than a silent prod regression to "skip propagation".
var _ migrationshandlers.MigrationCancelClient = (*agentclient.Client)(nil)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// resolvePeerURL resolves an auto/empty etcd peer URL to the host's routable
// IPv4 in place on cfg, before any consumer (peer cert SANs, initial-cluster,
// the etcd member) reads it. An operator-supplied URL is left untouched; a
// changed value is logged at INFO.
func resolvePeerURL(cfg *config.APIConfig, log *slog.Logger) error {
	resolved, err := netdetect.ResolvePeerURL(cfg.Etcd.PeerURL, netdetect.RoutableIPv4)
	if err != nil {
		return fmt.Errorf("resolve peer url: %v", err)
	}
	if resolved == cfg.Etcd.PeerURL {
		return nil
	}
	// A loopback fallback fired only on auto/empty raw (an operator who set
	// https://127.0.0.1:2380 explicitly takes the equal-value early return
	// above and is not warned - that is their choice). Surface it loudly: such
	// a node is not HA-ready until peer_url names a routable address.
	if resolved == "https://127.0.0.1:2380" {
		log.Warn("no routable IPv4 found; etcd peer URL falls back to loopback - this node is not HA-ready until peer_url is set to a routable address",
			"from", cfg.Etcd.PeerURL, "to", resolved)
	} else {
		log.Info("resolved etcd peer url", "from", cfg.Etcd.PeerURL, "to", resolved)
	}
	cfg.Etcd.PeerURL = resolved
	return nil
}

// ensureClusterCAForJoin returns the etcd initial-cluster string a joining
// replica needs to start, fetching the cluster CA from an existing replica
// exactly once across the joiner's lifetime. The single-use join token forces
// the control flow to survive a restart WITHOUT re-redeeming it. Three branches,
// checked before any token resolution so a restart never hits the network:
//
//   - Member dir present: etcd is fully initialized and recovers membership from
//     the WAL, ignoring initial-cluster. Return the configured value verbatim
//     (empty is fine) - no fetch, no sidecar needed.
//   - CA on disk but no member dir (a partial join across a restart, token
//     already spent): resume from the saved initial-cluster sidecar. A
//     missing/empty sidecar is an unrecoverable partial join - return a clear
//     actionable error naming the cleanup, not the misleading etcd one.
//   - No CA (first join): resolve the token, fetch the CA, reject an etcd.name
//     collision before persisting anything, persist the CA, persist the computed
//     initial-cluster to the sidecar, and remove the consumed token file.
//
// The sidecar lives next to the CA (a daemon-writable dir) and is written
// atomically after the CA so it never exists without one.
func ensureClusterCAForJoin(ctx context.Context, cfg *config.APIConfig, log *slog.Logger) (string, error) {
	icSidecar := filepath.Join(filepath.Dir(cfg.ClusterCA.CertFile), "initial-cluster")

	// etcd creates <DataDir>/member on first successful start; once present it
	// recovers membership from the WAL and ignores initial-cluster. Mirror
	// internal/etcd memberDirExists so a restart after a healthy first start
	// never re-fetches and never requires the sidecar.
	if fileExists(filepath.Join(cfg.Etcd.DataDir, "member")) {
		log.Info("etcd member already initialized; recovering from WAL")
		return cfg.Etcd.InitialCluster, nil
	}

	if fileExists(cfg.ClusterCA.CertFile) && fileExists(cfg.ClusterCA.KeyFile) {
		// Partial join across a restart: the CA was persisted but the member was
		// never initialized, and the single-use token is already spent. Recover
		// the previously-computed initial-cluster from the sidecar.
		if ic := readInitialClusterSidecar(icSidecar); ic != "" {
			log.Info("resuming partial join from saved initial-cluster", "sidecar", icSidecar)
			return ic, nil
		}
		return "", fmt.Errorf("partial join detected: cluster CA is present but the etcd member was never initialized and no saved initial-cluster exists at %s; the join token is already consumed - remove the cluster CA (%s, %s) and the etcd data dir (%s), then re-run 'otherix-api join' with a fresh token",
			icSidecar, cfg.ClusterCA.CertFile, cfg.ClusterCA.KeyFile, cfg.Etcd.DataDir)
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

	// Reject an etcd.name collision before persisting anything. FetchClusterCA
	// has already consumed the token and registered the learner, but writing no
	// CA/sidecar keeps the on-disk state clean: the operator just re-runs join
	// with a unique --name and a fresh token, with nothing CA-side to clean up.
	if otherPeer, collides := nameCollision(res.Members, cfg.Etcd.Name, cfg.Etcd.PeerURL); collides {
		return "", fmt.Errorf("etcd member name %q is already used by another member in the cluster (peer %s); choose a unique --name and re-run 'otherix-api join' (the join token was consumed, so use a fresh one)",
			cfg.Etcd.Name, otherPeer)
	}

	if err := auth.WriteCertCacheAtomic(cfg.ClusterCA.CertFile, cfg.ClusterCA.KeyFile, res.CA.CertPEM, res.CA.KeyPEM); err != nil {
		return "", fmt.Errorf("persist fetched cluster CA: %v", err)
	}
	log.Info("persisted cluster CA fetched on join",
		"fingerprint_sha256", hex.EncodeToString(res.CA.Fingerprint),
		"cert_file", cfg.ClusterCA.CertFile)

	ic := buildInitialCluster(res.Members, cfg.Etcd.Name, cfg.Etcd.PeerURL)

	// Persist the computed initial-cluster so a restart before the member dir
	// exists can resume without the spent token. Best-effort: a write failure
	// still lets this first boot proceed; only a restart-before-member-init
	// would then hit the clear partial-join error, which is acceptable.
	if err := writeInitialClusterSidecar(icSidecar, ic); err != nil {
		log.Warn("could not persist initial-cluster sidecar; a restart before etcd initializes will require a fresh-token rejoin",
			"sidecar", icSidecar, "error", err)
	}

	// The token is now spent and never needed again (later boots use the sidecar
	// or the WAL). Remove the token file so a CA-key-redeeming secret is not left
	// at rest. Only a file-backed token is removed; an inline token has no file.
	if jc.TokenPath != "" {
		if err := os.Remove(jc.TokenPath); err != nil {
			log.Warn("could not remove consumed join token", "token_path", jc.TokenPath, "error", err)
		} else {
			log.Info("removed consumed join token", "token_path", jc.TokenPath)
		}
	}

	return ic, nil
}

// nameCollision reports whether selfName is already claimed by a different
// member in the fetched membership. A same-name entry whose peer URL equals
// selfPeerURL is this node being re-registered (fine); a same-name entry with a
// different peer URL is a real collision. Peer URLs compare canonically so
// trailing-slash spellings do not falsely diverge. Returns the colliding peer.
func nameCollision(members []api.ClusterMemberRef, selfName, selfPeerURL string) (string, bool) {
	self := canonPeerURL(selfPeerURL)
	for _, m := range members {
		if m.Name == selfName && canonPeerURL(m.PeerURL) != self {
			return m.PeerURL, true
		}
	}
	return "", false
}

// readInitialClusterSidecar returns the trimmed initial-cluster persisted next
// to the cluster CA, or "" when the sidecar is absent, unreadable, or empty. An
// empty result is treated as missing by the caller (a truncated write must not
// hand etcd a blank initial-cluster).
func readInitialClusterSidecar(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // daemon-derived path next to the configured cluster CA, not operator/user input
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeInitialClusterSidecar atomically writes the initial-cluster string via a
// temp file in the same directory plus a rename, mode 0600, so a concurrent
// reader never observes a partial write.
func writeInitialClusterSidecar(path, ic string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".initial-cluster-*")
	if err != nil {
		return fmt.Errorf("create temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(ic); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %v", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %v", err)
	}
	return nil
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
	if err := runBootstrapHooks(ctx, st, caMaterial, cfg.Network, cfg.StoragePools, log); err != nil {
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

	// The background loops (dispatcher, scheduler, promote loop) run under a
	// cancelable child of ctx so an early server-init/start failure - which does
	// NOT cancel ctx - can still drive them to Done and drain them before return.
	// server.Run stays on the parent ctx so SIGTERM alone drives graceful HTTP
	// shutdown.
	runCtx, cancelRun := context.WithCancel(ctx)

	stopWorkers, err := startWorkers(runCtx, cfg, st, agentClient, log)
	if err != nil {
		cancelRun()
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
		clustermembers.RunPromoteLoop(runCtx, membership, 15*time.Second, log)
	}()

	// Shutdown ordering contract (load-bearing for the graceful-shutdown
	// requeue): cancelRun() drives the background loops to Done. On the SIGTERM
	// path the parent ctx is already cancelled, but on an early server-init/start
	// failure it is NOT - so this cancel is what lets the loops observe Done and
	// keeps stopWorkers from blocking forever. stopWorkers() then blocks on the
	// dispatcher's in-flight wg.Wait, so every in-flight handler finishes and its
	// queue bookkeeping (which runs on a context.WithoutCancel that survives ctx
	// cancel) lands BEFORE runServe returns. Only after runServe returns do
	// serve.go's deferred etcd client Close and member Stop run, so the store stays
	// available for those requeue/complete writes. Draining on EVERY post-launch
	// return path - including the api.NewServer and server.Run error paths, which
	// do not cancel ctx - is what stops etcd from being torn down under running
	// loops. Do NOT tear etcd down before this returns.
	defer func() {
		cancelRun()
		stopWorkers()
		promoteWG.Wait()
	}()

	// The etcd store self-enqueues (EnqueueTask writes the job inline) and
	// tasks.cancel cancels through the store, so there is no queue client to pass.
	server, err := api.NewServer(
		*cfg, st,
		vmshandlers.LifecycleDeps{AgentClient: agentClient},
		vmshandlers.ConsoleDeps{AgentClient: agentClient, AccessMode: cfg.Console.AccessMode},
		authSvc, material, membership, log)
	if err != nil {
		return fmt.Errorf("server init: %v", err)
	}
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("server: %v", err)
	}

	log.Info("shutting down")
	return nil
}

// runBootstrapHooks runs the post-start / pre-serve bootstrap hooks in the
// canonical order: BootstrapAdmin first (seeds the first admin user), then
// BootstrapClusterCA (syncs the on-disk cluster CA into etcd so the /v1/ca
// endpoint and the Step 2 CSR signer have an active row), then
// BootstrapSSHUserCA (provisions the cluster SSH user-CA in etcd so every
// replica signs guest user-certs with the same CA), then
// BootstrapSessionCA (provisions the cluster ingress-session CA in etcd so every
// replica signs session credentials with the same key and gateways verify them
// against a single public half), then
// SeedOverlaySupernet (writes the cluster overlay supernet first-writer-wins so
// agent WG overlay allocation has a supernet to carve /24s from), then
// SeedVNIRange (writes the VXLAN VNI allocation bounds first-writer-wins), then
// SeedUnderlayMTU (writes the physical underlay MTU first-writer-wins; the
// overlay inner MTU and otwg0 MTU derive from it). All hooks are idempotent -
// repeat boots observe existing rows and no-op.
func runBootstrapHooks(ctx context.Context, st *etcdstore.Store, caMaterial auth.ClusterCAResult, netCfg config.NetworkConfig, poolCfg config.StoragePoolsConfig, log *slog.Logger) error {
	if err := api.BootstrapAdmin(ctx, st, log); err != nil {
		return fmt.Errorf("bootstrap admin: %v", err)
	}
	if err := api.BootstrapClusterCA(ctx, st, caMaterial, log); err != nil {
		return fmt.Errorf("bootstrap cluster CA: %v", err)
	}
	if err := api.BootstrapSSHUserCA(ctx, st, log); err != nil {
		return fmt.Errorf("bootstrap SSH user CA: %v", err)
	}
	if err := api.BootstrapSessionCA(ctx, st, log); err != nil {
		return fmt.Errorf("bootstrap session CA: %v", err)
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
	// SeedDefaultPoolName writes the cluster default pool name first-writer-wins.
	// An empty DefaultPoolName is the documented opt-out and is seeded as nothing
	// (the ensurer then no-ops). Idempotent across boots.
	if err := st.SeedDefaultPoolName(ctx, poolCfg.DefaultPoolName); err != nil {
		return fmt.Errorf("seed default pool name: %v", err)
	}
	// Log the EFFECTIVE default pool name, not the configured one: the seed is
	// first-writer-wins, so this replica's config value is a no-op against a
	// pre-existing operator-set or older-boot value. Logging poolCfg.DefaultPoolName
	// would misreport the cluster default after a no-op seed.
	if cs, err := st.ClusterSettings(ctx); err == nil && cs.DefaultPoolName != nil {
		log.InfoContext(ctx, "cluster default pool name in effect", slog.String("default_pool_name", *cs.DefaultPoolName))
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

// buildDispatcher registers the async task-kind handlers on a dispatcher
// polling the etcd job queue. Each handler reuses the production agent executor
// and the Run-form worker in its owning handler package.
func buildDispatcher(st *etcdstore.Store, agentClient *agentclient.Client, cfg *config.APIConfig, log *slog.Logger) *worker.Dispatcher {
	d := worker.NewDispatcher(st, log, 0 /* default poll interval */, cfg.Workers.MaxWorkers)

	// The snapshot-blob pull broker pulls a recreate's snapshot blobs from a live
	// peer to the target node before the agent materializes from them.
	// It reuses the etcd store (holder discovery + saga) and the agentclient
	// (serve/pull data path).
	blobBroker := blobbroker.New(st, blobbroker.NewClientExecutor(agentClient), log)
	d.Register("vm.create", workerMaxAttempts,
		vmshandlers.CreateHandler(st, vmshandlers.NewAgentVMCreateExecutor(agentClient), blobBroker, log, cfg.Workers.Heartbeat.RebalanceGrace))
	d.Register("vm.delete", workerMaxAttempts,
		vmshandlers.DeleteHandler(st, vmshandlers.NewAgentVMDeleteExecutor(agentClient), log, cfg.Workers.Heartbeat.RebalanceGrace))

	lifecycleExec := vmshandlers.NewAgentVMLifecycleExecutor(agentClient)
	for _, lk := range vmshandlers.LifecycleKinds() {
		d.Register(lk.Kind, workerMaxAttempts,
			vmshandlers.LifecycleHandler(st, lifecycleExec, log, lk.Op, lk.DesiredPhase, lk.RuntimePhase, lk.FailureCode, cfg.Workers.Heartbeat.RebalanceGrace))
	}

	d.Register("storage_pool.scan", workerMaxAttempts,
		storagepoolshandlers.ScanHandler(st, storagepoolshandlers.NewAgentScanExecutor(agentClient), cfg.Placement.Pressure.Disk, log))

	snapshotExec := snapshotshandlers.NewAgentSnapshotExecutor(agentClient)
	d.Register("vm.snapshot.create", workerMaxAttempts,
		snapshotshandlers.CreateHandler(st, snapshotExec, log))
	d.Register("vm.snapshot.delete", workerMaxAttempts,
		snapshotshandlers.DeleteHandler(st, snapshotExec, log))

	d.Register("artifact.replicate", workerMaxAttempts,
		replicationhandlers.ReplicateHandler(st, blobBroker, log))
	d.Register("artifact.reclaim", workerMaxAttempts,
		replicationhandlers.ReclaimHandler(st, reclaimAdapter{st: st, agentClient: agentClient}, log))

	// vm.migrate drives the live-migration saga (placement / two-phase handshake /
	// atomic cutover). The placer reuses SchedulePlacement over the store's
	// read-only querier (no placement lock - the worker re-pins only at cutover).
	migratePlacer := migrationshandlers.NewSchedulerPlacer(st.PlacementQuerier(), schedpkg.PlacementConfig{
		Algorithm: cfg.Placement.Algorithm,
		Resources: api.SchedulerResourcesFromConfig(cfg.Placement.Resources),
	})
	d.Register("vm.migrate", workerMaxAttempts,
		migrationshandlers.MigrateHandler(st, agentClient, migratePlacer, migrationshandlers.MigrateConfig{
			DefaultPoolName: cfg.StoragePools.DefaultPoolName,
		}, log))

	// node.drain evacuates a node's VMs via node-less vm.migrate jobs, gating each
	// on a dry-run target check (the same scheduler placer the migrate saga binds
	// with). DeadNodeGrace from the heartbeat reconciler lets the saga finalize a
	// node that stops heartbeating mid-drain (the reconciler skips draining nodes).
	drainPlacer := migrationshandlers.NewSchedulerPlacer(st.PlacementQuerier(), schedpkg.PlacementConfig{
		Algorithm: cfg.Placement.Algorithm,
		Resources: api.SchedulerResourcesFromConfig(cfg.Placement.Resources),
	})
	d.Register("node.drain", nodeDrainMaxAttempts,
		nodeshandlers.DrainHandler(st, drainPlacer, nodeshandlers.DrainConfig{
			DeadNodeGrace: cfg.Workers.Heartbeat.RebalanceGrace,
		}, log))

	return d
}

// reclaimAdapter resolves a target node's advertised endpoint and tells its
// agent to delete a blob by digest. It adapts the store + agent client to the
// replication.Reclaimer seam the artifact.reclaim worker drives, mirroring how
// blobbroker resolves a node endpoint via NodeByID.
type reclaimAdapter struct {
	st          *etcdstore.Store
	agentClient *agentclient.Client
}

// Reclaim resolves targetNodeID to its advertised endpoint and calls the agent
// blob-reclaim endpoint. A reclaim of an absent blob is a no-op success agent-side.
func (a reclaimAdapter) Reclaim(ctx context.Context, targetNodeID uuid.UUID, digest string) error {
	node, err := a.st.NodeByID(ctx, targetNodeID)
	if err != nil {
		return fmt.Errorf("resolve reclaim target node %s: %v", targetNodeID, err)
	}
	return a.agentClient.ReclaimBlob(ctx, node.AdvertisedEndpoint, agentapi.BlobReclaimRequest{Digest: digest})
}

// buildScheduler registers the periodic maintenance functions on a scheduler.
// Cadences mirror the worker periodic registrations: hourly retention sweeps, the
// heartbeat reconcile on the configured interval (run-on-start so a restart
// promotes nodes that kept heartbeating), and the scan trigger when enabled.
func buildScheduler(st *etcdstore.Store, cfg *config.APIConfig, log *slog.Logger) *worker.Scheduler {
	s := worker.NewScheduler(log)

	s.Register("tasks.cleanup", time.Hour, false,
		taskshandlers.CleanupFunc(st, taskshandlers.RetentionConfig{
			Completed: cfg.Workers.Tasks.Retention.Completed,
			Failed:    cfg.Workers.Tasks.Retention.Failed,
		}, log))

	s.Register("migrations.cleanup", time.Hour, false,
		migrationshandlers.CleanupFunc(st, migrationshandlers.RetentionConfig{
			Completed: cfg.Workers.Migrations.Retention.Completed,
			Failed:    cfg.Workers.Migrations.Retention.Failed,
			Cancelled: cfg.Workers.Migrations.Retention.Cancelled,
		}, log))

	s.Register("heartbeat.reconcile", positiveOr(cfg.Workers.Heartbeat.Interval, 30*time.Second), true,
		heartbeathandlers.ReconcileFunc(st, heartbeathandlers.ReconcileConfig{
			StaleThreshold: cfg.Workers.Heartbeat.StaleThreshold,
			GoneGrace:      cfg.Workers.Heartbeat.RebalanceGrace,
			Interval:       cfg.Workers.Heartbeat.Interval,
		}, log,
			storagepoolshandlers.EnsureDefaultPoolsFunc(st, cfg.StoragePools.AllowedPathPrefixes[0], log),
			replicationhandlers.PruneGoneNodeBlobsFunc(st, log)))

	s.Register("vms.schedule", 2*time.Second, true,
		vmshandlers.ScheduleFunc(st, vmshandlers.ScheduleConfig{Algorithm: cfg.Placement.Algorithm}, log,
			api.SchedulerResourcesFromConfig(cfg.Placement.Resources)))

	s.Register("placement.reconcile", positiveOr(cfg.Workers.PlacementReconcile.Interval, 30*time.Second), true,
		replicationhandlers.ReconcileFunc(st, cfg.Workers.Heartbeat.RebalanceGrace, log))

	s.Register("node.drain.reconcile", 2*time.Minute, false,
		nodeshandlers.DrainReconcileFunc(st, log))

	s.Register("gateway.reconcile", positiveOr(cfg.Workers.PlacementReconcile.Interval, 30*time.Second), true,
		gatewayshandlers.ReconcileFunc(st, gatewayshandlers.ReconcileConfig{}, log))

	s.Register("artifact.saga.retention",
		positiveOr(cfg.Workers.ArtifactSagaRetention.Interval, time.Hour), false,
		blobbroker.SagaRetentionFunc(st,
			positiveOr(cfg.Workers.ArtifactSagaRetention.Retention, 24*time.Hour),
			positiveOr(cfg.Workers.ArtifactSagaRetention.StrandedRetention, 24*time.Hour),
			log))

	s.Register("auth.refresh_token_cleanup", time.Hour, false,
		auth.RefreshTokenCleanupFunc(st, log))

	s.Register("idempotency.cleanup", time.Hour, false,
		middleware.IdempotencyCleanupFunc(st, log))

	s.Register("networks.cleanup", time.Hour, false,
		networkshandlers.CleanupFunc(st, log))

	s.Register("delete_intent.reap", etcdstore.DefaultDeleteIntentStaleAfter, false,
		etcdstore.DeleteIntentReaperFunc(st, etcdstore.DefaultDeleteIntentStaleAfter, log))

	s.Register("jobs.cleanup", time.Hour, false, etcdstore.JobsCleanupFunc(st, log))

	s.Register("jobs.reclaim", etcdstore.JobLeaseRenewInterval, false,
		etcdstore.ReclaimStaleJobsFunc(st, etcdstore.JobLease, log))

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

// buildAgentClient constructs the *agentclient.Client used by the scan and vm
// executors. Returns (nil, nil) when AgentClient.Enabled is false - the api
// binary still boots so HTTP-only smoke testing stays available; the consumer
// paths each emit their own degradation envelope.
//
// mTLS material (replica's leaf cert + cluster CA trust anchor) flows in via
// material - produced upstream per LoadOrGenerateCPCert. Construction errors at
// this stage (config validation, empty material when AgentClient.Enabled=true)
// are boot-time fatal: the api binary must not start with a half-configured
// agent client.
func buildAgentClient(cfg *config.APIConfig, material api.TLSMaterial, log *slog.Logger) (*agentclient.Client, error) {
	if !cfg.AgentClient.Enabled {
		log.Info("agent client disabled; scan workers will surface degraded responses")
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
