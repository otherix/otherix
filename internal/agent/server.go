// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/dnsproxy"
	blobshandlers "github.com/otherix/otherix/internal/agent/handlers/blobs"
	heartbeatHandlers "github.com/otherix/otherix/internal/agent/handlers/heartbeat"
	storagepoolshandlers "github.com/otherix/otherix/internal/agent/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/agent/handlers/tasks"
	vmshandlers "github.com/otherix/otherix/internal/agent/handlers/vms"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/ingress"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/reconciler"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agent/wgkey"
	"github.com/otherix/otherix/internal/agent/zram"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/version"
)

// nodeCNPrefix is the literal prefix all agent cert CommonName values
// share — `node-<name>` per internal/auth/csr.go SignCSR template.
// parseNodeNameFromCert strips this prefix to surface the operator-
// supplied name.
const nodeCNPrefix = "node-"

// parseNodeNameFromCert loads the agent's own cert from certPath, parses
// the leaf, and returns the operator-supplied node name encoded in the
// Subject CN. The cert template (internal/auth/csr.go SignCSR) emits
// CN `node-<nodeName>`; this function strips the prefix.
//
// The agent identifies itself via the cert CN rather than a separate
// sidecar file. Cert tampering is out of scope - the file is owned
// by the agent uid and the CP-side mTLS verifier validates fingerprint
// + chain.
func parseNodeNameFromCert(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath) //nolint:gosec // path is operator-configured (agent.yaml tls.cert_path); the function exists to open this exact file
	if err != nil {
		return "", fmt.Errorf("read cert %s: %v", certPath, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("cert %s: not a CERTIFICATE PEM block", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert %s: %v", certPath, err)
	}
	cn := strings.TrimSpace(cert.Subject.CommonName)
	if cn == "" {
		return "", fmt.Errorf("cert %s: Subject CommonName is empty", certPath)
	}
	if !strings.HasPrefix(cn, nodeCNPrefix) {
		return "", fmt.Errorf("cert %s: Subject CN %q does not start with %q", certPath, cn, nodeCNPrefix)
	}
	name := strings.TrimPrefix(cn, nodeCNPrefix)
	if name == "" {
		return "", fmt.Errorf("cert %s: Subject CN %q has empty node name after prefix", certPath, cn)
	}
	return name, nil
}

// Run starts the agent HTTPS (mTLS) server and blocks until ctx is
// cancelled or the server fails. It mounts the full agent
// surface - /v1/vms, /v1/tasks - alongside the /health
// endpoint, all guarded by mTLS client cert verification.
//
// Name-keyed agent identity: the node name is parsed from the cert CN
// at startup. cfg.NodeID is no longer carried; the heartbeat URL
// uses the name (per the `/v1/nodes/{name}/heartbeat` endpoint).
func Run(ctx context.Context, cfg *config.AgentConfig, log *slog.Logger) error {
	tlsCfg, err := loadTLS(cfg.TLS)
	if err != nil {
		return fmt.Errorf("load tls: %w", err)
	}

	nodeName, err := parseNodeNameFromCert(cfg.TLS.CertPath)
	if err != nil {
		return fmt.Errorf("parse node name from cert: %w", err)
	}
	log.Info("agent: resolved node name from cert CN", "node_name", nodeName)

	// Host-level compressed-swap safety net. Set up once at boot; the observed
	// capability is reported on the next heartbeat.
	if active, err := zram.Ensure(zram.Params{
		Enabled:       cfg.Zram.Enabled,
		MaxRAMPercent: cfg.Zram.MaxRAMPercent,
		Algorithm:     cfg.Zram.Algorithm,
	}, log); err != nil {
		// Non-fatal: a failed safety-net setup must never crash the agent. The
		// observed capability then reports off (fail-closed for the overcommit gate).
		log.Warn("zram safety net setup failed", "err", err)
	} else if active != nil {
		log.Info("zram safety net active",
			"device", active.Device, "size_mib", active.SizeMib,
			"mem_limit_mib", active.MemLimitMib, "algorithm", active.Algorithm)
	}

	// Single fabric shared by the VM manager (tap create/attach) and the
	// network reconciler (bridge/NAT materialisation). Linux-only impl;
	// an unsupported stub on other platforms keeps the agent compiling.
	fabric := netfabric.New()

	// Per-node overlay L3 services bound at the gateway anycast address: the
	// DNS forwarder and the DHCPv4 responder. Both are registered per overlay
	// by the network reconciler and run their own loops below.
	dnsForwarder, dhcpResponder, err := newOverlayServices(log)
	if err != nil {
		return err
	}

	manager, err := vm.New(cfg, fabric, log)
	if err != nil {
		return fmt.Errorf("vm manager: %w", err)
	}

	// Blob transport (artifact store + serve/pull control handler). Runs
	// the one-time relocation sweep and fails closed if the production artifact
	// store is absent (artifacts.root is mandatory in production config).
	artStore, blobsHandler, blobServeMgr, err := setupBlobTransport(cfg, manager, tlsCfg, log)
	if err != nil {
		return fmt.Errorf("blob transport: %w", err)
	}

	// Boot hygiene, run synchronously BEFORE anything that can open a `.staging`
	// or image-scratch temp starts (the listener accepting /v1/blobs/pull and the
	// reconcilers driving a create/import). The boot staging sweep removes ALL
	// staging on both stores; running it concurrently with serving could delete an
	// in-flight Put's temp mid-write. SweepImageScratch and the temp-dir backstop
	// reclaim image downloads a crash left behind that no periodic sweep covers.
	sweeper := newArtifactSweeper(artStore, manager.ImageStore(), log)
	sweeper.BootSweep(ctx)
	manager.SweepImageScratch()
	manager.SweepOrphanImageMeta()
	sweepLeftoverImageTempDirs(log)

	// Snapshot staging hygiene, run synchronously BEFORE serving for the same
	// reason: a capture that opens a snapshots/.staging temp after serving begins
	// must never be swept mid-write. The boot pass clears all staging while
	// nothing is in flight; the periodic pass (in the drain set below) spares
	// fresh temps.
	snapStagingSweeper := newSnapshotStagingSweeper(manager.SweepSnapshotStaging, log)
	snapStagingSweeper.BootSweep(ctx)

	// Per-resource reconcilers (pool / network / vm / wireguard). Each plugs
	// into the heartbeat sender as both a ResponseHandler (consumes its
	// declared_* slice) and a reporter (publishes observed state).
	rec, err := buildReconcilers(cfg, manager, fabric, dhcpResponder, log)
	if err != nil {
		return err
	}
	poolReconciler, netReconciler := rec.pools, rec.networks
	vmReconciler, wgReconciler := rec.vms, rec.wireGuard
	healthReconciler := rec.health
	pubListenerReconciler := rec.publishedListeners

	// Console token store - in-memory, lifecycle bound to the agent
	// process; restart drops the tokens alongside the QEMU `-serial`
	// sockets they reference. The single-session lock that used to
	// live next to it now lives inside serialmux.Multiplexer
	// (ErrConsoleInUse).
	consoleTokens := console.NewTokenStore()
	consoleTokens.Start(ctx)

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	// Co-located ingress plane: a hypervisor node that also serves ingress binds
	// the bearer-only /v1/connect listener on cfg.Gateway.Listen alongside the mTLS
	// control listener, on the SHARED node identity and WireGuard key (no key-path
	// switch). Gated on the ingress listen address being configured and decoupled
	// from cfg.Gateway.Enabled (which dispatches the standalone-no-KVM run path).
	// The plane is inert until the CP declares a membership: the connect gate
	// refuses every request without a session credential the CP-distributed session
	// CA verifies. The single Networks reconciler satisfies OverlayResolver, so the
	// same reconciler drives the node's own overlay services and its ingress veths.
	ingressPlane, err := buildIngressPlaneIfConfigured(cfg, fabric, netReconciler, log)
	if err != nil {
		return err
	}
	// Build the heartbeat sender BEFORE the router so the same live Sender
	// the agent posts heartbeats with backs POST /v1/heartbeat/nudge. A nil
	// Sender means heartbeats are disabled (misconfiguration); the router
	// then mounts a no-op nudger so the endpoint still answers 204 without a
	// nil-pointer panic. startHeartbeat below launches the goroutine that
	// drives this same Sender's loop. When the ingress plane is active its
	// session-CA store is fanned into the heartbeat response handler chain.
	sender := buildSender(heartbeatCtx, cfg, nodeName, manager, artStore, poolReconciler, vmReconciler, netReconciler, wgReconciler, healthReconciler, pubListenerReconciler, ingressCAStoreOf(ingressPlane), log)

	router := buildRouter(cfg, nodeName, log, manager, consoleTokens, dhcpResponder, nudgerFor(sender), blobsHandler)

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      router,
		TLSConfig:    tlsCfg,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	heartbeatDone := startHeartbeat(heartbeatCtx, sender, log)

	reconcilerDone := runReconciler(heartbeatCtx, "pool reconciler", poolReconciler.Run, log)
	netReconcilerDone := runReconciler(heartbeatCtx, "network reconciler", netReconciler.Run, log)
	vmReconcilerDone := runReconciler(heartbeatCtx, "vm reconciler", vmReconciler.Run, log)
	wgReconcilerDone := runReconciler(heartbeatCtx, "wireguard reconciler", wgReconciler.Run, log)
	healthReconcilerDone := runReconciler(heartbeatCtx, "health reconciler", healthReconciler.Run, log)
	pubListenerReconcilerDone := runReconciler(heartbeatCtx, "published-listener reconciler", pubListenerReconciler.Run, log)
	dnsForwarderDone := runSupervised(heartbeatCtx, "dns forwarder", dnsForwarder.Run, dnsForwarderMinBackoff, dnsForwarderMaxBackoff, log)
	dhcpResponderDone := runReconciler(heartbeatCtx, "dhcp responder", dhcpResponder.Run, log)
	artifactSweeperDone := runReconciler(heartbeatCtx, "artifact sweeper", sweeper.Run, log)
	snapStagingSweeperDone := runReconciler(heartbeatCtx, "snapshot staging sweeper", snapStagingSweeper.Run, log)
	blobScrubberDone := runReconciler(heartbeatCtx, "blob scrubber", (&blobScrubber{
		artifactStore: artStore,
		imageStore:    manager.ImageStore(),
		imageTryLock:  manager.TryLockImageBlob,
		cfg:           cfg.Artifacts.Scrub,
		log:           log,
	}).Run, log)

	imageEvictDone := startImageCacheEviction(heartbeatCtx, cfg, manager, log)

	errc := startAgentListeners(cfg, srv, ingressPlane, log)

	// Drain set waited on at shutdown. The heartbeat sender respects
	// ctx and returns promptly; the reconcilers' Run loops only return
	// BETWEEN reconcile passes and call blocking netfabric (netlink /
	// nftables / wgctrl) ops that take no context and cannot be
	// interrupted. A wedged fabric op would otherwise make shutdown hang
	// until SIGKILL - see awaitDone for the bound.
	dones := []namedDone{
		{name: "heartbeat sender", done: heartbeatDone},
		{name: "pool reconciler", done: reconcilerDone},
		{name: "vm reconciler", done: vmReconcilerDone},
		{name: "network reconciler", done: netReconcilerDone},
		{name: "wireguard reconciler", done: wgReconcilerDone},
		{name: "health reconciler", done: healthReconcilerDone},
		{name: "published-listener reconciler", done: pubListenerReconcilerDone},
		{name: "dns forwarder", done: dnsForwarderDone},
		{name: "dhcp responder", done: dhcpResponderDone},
		{name: "artifact sweeper", done: artifactSweeperDone},
		{name: "snapshot staging sweeper", done: snapStagingSweeperDone},
		{name: "blob scrubber", done: blobScrubberDone},
	}
	if imageEvictDone != nil {
		dones = append(dones, namedDone{name: "image cache eviction", done: imageEvictDone})
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errc:
		stopHeartbeat()
		drainReconcilers(dones, cfg.Server.ShutdownGrace, log)
		if cerr := blobServeMgr.Close(); cerr != nil {
			log.Warn("blob serve manager close failed", "error", cerr.Error())
		}
		if ingressPlane == nil {
			return err
		}
		// Co-located: the sibling listener may still be serving (the failure could
		// be on either plane). Shut both down and drain the one remaining goroutine
		// return, but surface the original listener error rather than any shutdown
		// error. Fail toward inaction: a bind failure on one plane tears the whole
		// runtime down rather than leaving a half-up node.
		_ = stopAgentServers(cfg.Server.ShutdownGrace, srv, ingressPlane, errc, 1, log)
		return err
	}

	stopHeartbeat()
	drainReconcilers(dones, cfg.Server.ShutdownGrace, log)

	// Tear down any in-flight peer blob serve listeners alongside the main
	// server: each is a separate listener+goroutine not in the reconciler drain
	// set, so without this they would be abandoned at shutdown. Best-effort.
	if cerr := blobServeMgr.Close(); cerr != nil {
		log.Warn("blob serve manager close failed", "error", cerr.Error())
	}

	// Shut the control server (and the ingress listener when the plane is active)
	// down, then drain both listener goroutine returns so they have exited.
	drain := 1
	if ingressPlane != nil {
		drain = 2
	}
	return stopAgentServers(cfg.Server.ShutdownGrace, srv, ingressPlane, errc, drain, log)
}

// buildIngressPlaneIfConfigured builds the co-located ingress plane when an
// ingress listen address is configured (decoupled from cfg.Gateway.Enabled,
// which dispatches the standalone-no-KVM run path), or returns (nil, nil) for a
// plain hypervisor. overlays is the node's single Networks reconciler, so the
// same reconciler drives the node's own overlay services and its ingress veths.
func buildIngressPlaneIfConfigured(cfg *config.AgentConfig, fabric netfabric.Fabric, overlays ingress.OverlayResolver, log *slog.Logger) (*ingress.Plane, error) {
	if cfg.Gateway.Listen == "" {
		return nil, nil
	}
	plane, err := ingress.BuildPlane(ingress.PlaneDeps{
		Listen:       cfg.Gateway.Listen,
		TLS:          cfg.TLS,
		Fabric:       fabric,
		Overlays:     overlays,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		Log:          log,
	})
	if err != nil {
		return nil, fmt.Errorf("ingress plane: %w", err)
	}
	return plane, nil
}

// ingressCAStoreOf returns the plane's session-CA store, or nil when no ingress
// plane is active. The heartbeat sender fans a non-nil store into its response
// handler chain so the connect credential gate stays armed.
func ingressCAStoreOf(plane *ingress.Plane) *ingress.SessionCAStore {
	if plane == nil {
		return nil
	}
	return plane.CAStore
}

// startAgentListeners launches the control listener - and, when the ingress plane
// is active, the ingress listener - in goroutines, returning the channel their
// outcomes land on. The channel is buffered for every listener so a clean
// shutdown (each returns http.ErrServerClosed and sends) never blocks a sender: a
// cap-1 channel would wedge the second sender forever. A pure hypervisor runs a
// single listener on the cap-1 channel, unchanged.
func startAgentListeners(cfg *config.AgentConfig, srv *http.Server, plane *ingress.Plane, log *slog.Logger) chan error {
	n := 1
	if plane != nil {
		n = 2
	}
	errc := make(chan error, n)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()
	if plane != nil {
		go func() {
			log.Info("listening", "plane", "ingress", "addr", plane.Server.Addr)
			if err := plane.Server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("ingress listener: %w", err)
				return
			}
			errc <- nil
		}()
	}
	return errc
}

// stopAgentServers gracefully shuts the ingress server (when the plane is active)
// and then the control server down within grace, best-effort, then drains `drain`
// outstanding listener returns from errc so those goroutines have exited before
// the caller returns. It returns the control server's shutdown error (nil on
// success); ingress shutdown errors are logged, not returned. Mirrors
// RunGateway's dual shutdown + drain discipline.
func stopAgentServers(grace time.Duration, srv *http.Server, plane *ingress.Plane, errc <-chan error, drain int, log *slog.Logger) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if plane != nil {
		if err := plane.Server.Shutdown(shutdownCtx); err != nil {
			log.Warn("ingress server shutdown failed", "addr", plane.Server.Addr, "error", err.Error())
		}
	}
	var out error
	if err := srv.Shutdown(shutdownCtx); err != nil {
		out = fmt.Errorf("shutdown: %w", err)
	}
	for i := 0; i < drain; i++ {
		<-errc
	}
	return out
}

// agentReconcilers bundles the per-resource reconcilers the agent runs.
type agentReconcilers struct {
	pools              *reconciler.Pools
	networks           *reconciler.Networks
	vms                *reconciler.VMs
	wireGuard          *reconciler.WireGuard
	health             *reconciler.Health
	publishedListeners *reconciler.PublishedListeners
}

// buildReconcilers constructs the per-resource reconcilers. Each consumes its
// declared_* slice from heartbeat responses and publishes observed state back;
// see the field comments on agentReconcilers. Any construction failure is
// fatal - the agent cannot converge without its reconcilers.
func buildReconcilers(cfg *config.AgentConfig, manager *vm.Manager, fabric netfabric.Fabric, dhcpResponder dhcp4.Responder, log *slog.Logger) (agentReconcilers, error) {
	pools, err := reconciler.NewPools(manager, log, 0)
	if err != nil {
		return agentReconcilers{}, fmt.Errorf("pool reconciler: %w", err)
	}
	networks, err := reconciler.NewNetworks(fabric, dhcpResponder, log, 0, true)
	if err != nil {
		return agentReconcilers{}, fmt.Errorf("network reconciler: %w", err)
	}
	vms, err := reconciler.NewVMs(manager, log, 0)
	if err != nil {
		return agentReconcilers{}, fmt.Errorf("vm reconciler: %w", err)
	}
	wgKey, err := wgkey.LoadOrGenerateKey(cfg.WireGuard.PrivateKeyPath)
	if err != nil {
		return agentReconcilers{}, fmt.Errorf("wireguard key: %w", err)
	}
	wireGuard, err := reconciler.NewWireGuard(fabric, wgKey, cfg.WireGuard, log, 0)
	if err != nil {
		return agentReconcilers{}, fmt.Errorf("wireguard reconciler: %w", err)
	}
	// The probe resolves the guest IP/bridge from local NIC/lease state (the same
	// anti-SSRF datapath as the ssh pipe): the VM manager for name->running+NICs,
	// the DHCP responder for the managed-DHCP lease.
	health, err := reconciler.NewHealth(reconciler.NewHealthProbe(manager, dhcpResponder, log), log, 0)
	if err != nil {
		return agentReconcilers{}, fmt.Errorf("health reconciler: %w", err)
	}
	// The published-listener reconciler owns the gateway's public L4 listeners; a
	// nil ListenerManager falls back to the production net-backed binder and a nil
	// dialer to the production SO_BINDTODEVICE overlay dialer. networks resolves a
	// backend overlay IP to its gateway veth device; fabric resolves the neighbor
	// MAC for the anti-SSRF pin.
	publishedListeners := reconciler.NewPublishedListeners(nil, networks, fabric, nil, log, 0)
	return agentReconcilers{
		pools: pools, networks: networks, vms: vms, wireGuard: wireGuard,
		health: health, publishedListeners: publishedListeners,
	}, nil
}

// overlayService is the runtime contract shared by the DNS forwarder and the
// DHCPv4 responder: each has a Run loop the agent drives via runReconciler.
type overlayService interface {
	Run(ctx context.Context) error
}

// dhcpService is the per-node DHCP responder as the agent runtime consumes it:
// the reconciler-facing dhcp4.Responder plus the Run lifecycle the runtime drives.
type dhcpService interface {
	dhcp4.Responder
	Run(ctx context.Context) error
}

// newOverlayServices constructs the per-node overlay L3 services that bind at
// the gateway anycast address (169.254.1.1).
//
// The DNS forwarder listens on :53; IP_FREEBIND lets the bind succeed before
// any overlay bridge owns the address, and empty Upstreams => dnsproxy.New
// discovers the node's resolvers from resolv.conf at construction time. The
// DHCPv4 responder serves CP-IPAM reservations; its Gateway and DNS default to
// the overlay anycast gateway and its Lease to dhcp4.DefaultLease, so an
// otherwise-empty Config suffices. The reconciler registers both per
// dhcp-enabled overlay; only the responder is also threaded into NewNetworks.
func newOverlayServices(log *slog.Logger) (dns overlayService, dhcp dhcpService, err error) {
	dnsForwarder, err := dnsproxy.New(dnsproxy.Config{
		Listen: net.JoinHostPort(netfabric.OverlayGatewayAddr.String(), strconv.Itoa(netfabric.OverlayDNSPort)),
		Log:    log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dns forwarder: %w", err)
	}

	dhcpResponder, err := dhcp4.New(dhcp4.Config{Log: log})
	if err != nil {
		return nil, nil, fmt.Errorf("dhcp responder: %w", err)
	}

	return dnsForwarder, dhcpResponder, nil
}

// namedDone pairs a reconciler/sender done-channel with its name so a
// drain timeout can report exactly which goroutine failed to exit.
type namedDone struct {
	name string
	done <-chan struct{}
}

// drainReconcilers waits up to timeout for every done channel to close,
// logging a WARN naming any that did not drain in time. It never blocks
// past the bound: a reconciler goroutine stuck in an uninterruptible
// netfabric syscall is abandoned rather than waited on indefinitely. The
// process is terminating, so abandoning the wait is equivalent to the
// SIGKILL-mid-op state the reconcilers already recover from (retry-
// forever / eventually-consistent); it adds no destructive action.
func drainReconcilers(dones []namedDone, timeout time.Duration, log *slog.Logger) {
	timedOut, stuck := awaitDone(dones, timeout)
	if timedOut {
		log.Warn("reconciler drain timed out; proceeding with shutdown",
			"timeout", timeout,
			"stuck", stuck,
		)
	}
}

// awaitDone waits up to timeout for every channel in dones to close. It
// returns timedOut=false with no stuck names when all closed in time;
// otherwise timedOut=true and stuck lists the names whose channels had
// not closed when the bound elapsed.
func awaitDone(dones []namedDone, timeout time.Duration) (timedOut bool, stuck []string) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for i, d := range dones {
		select {
		case <-d.done:
			// Drained.
		case <-timer.C:
			// Budget exhausted. Collect this channel and every
			// later one that has not already closed.
			for _, rem := range dones[i:] {
				select {
				case <-rem.done:
				default:
					stuck = append(stuck, rem.name)
				}
			}
			return true, stuck
		}
	}
	return false, nil
}

// sweepLeftoverImageTempDirs removes any leftover `otherix-image-*` scratch dirs
// in the OS temp dir at boot. This is a backstop for the historical layout (and
// the no-image-store test path) where image downloads staged under
// os.MkdirTemp("", ...) and a crash left a partial multi-GB file that no other
// sweep reclaimed. Best-effort: glob and removal errors are logged, never fatal.
func sweepLeftoverImageTempDirs(log *slog.Logger) {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "otherix-image-*"))
	if err != nil {
		log.Warn("sweep leftover image temp dirs: glob", "error", err.Error())
		return
	}
	for _, dir := range matches {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			log.Warn("sweep leftover image temp dirs: remove", "dir", dir, "error", rmErr.Error())
		}
	}
}

// runReconciler launches one reconciler's Run loop in a goroutine and
// returns a channel closed when it exits. context.Canceled is the
// expected shutdown signal and is logged at info, not error.
func runReconciler(ctx context.Context, name string, run func(context.Context) error, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Info(name + " starting")
		if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error(name+" stopped with error", "error", err.Error())
		}
		log.Info(name + " stopped")
	}()
	return done
}

// dnsForwarderMinBackoff is the initial wait before the dns forwarder is
// restarted after a failed bind. It doubles after each consecutive failure.
const dnsForwarderMinBackoff = 1 * time.Second

// dnsForwarderMaxBackoff caps the restart backoff for the dns forwarder so a
// persistently failing bind retries at a bounded steady rate.
const dnsForwarderMaxBackoff = 30 * time.Second

// runSupervised launches run in a goroutine and restarts it with bounded
// exponential backoff whenever it returns a non-cancel error, returning a
// channel closed when supervision ends. It exists for loops (the dns
// forwarder) whose Run hard-returns on a transient failure such as an initial
// UDP bind error: without supervision a single failed bind would leave DNS
// permanently dark for the process lifetime. A clean return (nil or a
// context-cancel error) ends supervision without a restart.
//
// Backoff starts at minBackoff, doubles after each consecutive failure, and is
// capped at maxBackoff. The wait between restarts selects on ctx.Done() so a
// shutdown aborts the wait promptly. No reset logic is needed: a healthy Run
// blocks until ctx cancel and never returns, so the only thing ever retried is
// a still-failing start.
func runSupervised(ctx context.Context, name string, run func(context.Context) error, minBackoff, maxBackoff time.Duration, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Info(name + " starting")
		backoff := minBackoff
		for {
			err := run(ctx)
			if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
				log.Info(name + " stopped")
				return
			}
			log.Error(name+" stopped with error, restarting after backoff",
				"error", err.Error(), "backoff", backoff.String())
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				log.Info(name + " stopped")
				return
			case <-timer.C:
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()
	return done
}

// startImageCacheEviction wires and launches the image cache eviction
// sweeper, returning the channel closed when it exits. A node with no
// artifacts root has a nil image store; in that case eviction is disabled
// and the returned channel is nil (skipped from the drain set).
func startImageCacheEviction(ctx context.Context, cfg *config.AgentConfig, manager *vm.Manager, log *slog.Logger) <-chan struct{} {
	imgStore := manager.ImageStore()
	if imgStore == nil {
		return nil
	}
	nudgeCh := make(chan struct{}, 1)
	manager.SetImageEvictionNudge(func() {
		select {
		case nudgeCh <- struct{}{}:
		default: // a pass is already pending; coalesce
		}
	})
	sweeper := newImageCacheSweeper(imgStore, manager.TryLockImageBlob, freeBytesStatfs, cfg.Artifacts.ImageCache, nudgeCh, func() { manager.SweepOrphanImageMeta() }, log)
	return runReconciler(ctx, "image cache eviction", sweeper.Run, log)
}

// poolImageAdapter implements heartbeat.PoolImageLister over
// vm.Manager.ListImages. It maps each vm.CachedImage to the heartbeat
// PoolImageReport wire shape: ChecksumSHA256 -> SHA256, ImportedAt (a
// time.Time) -> RFC 3339 string, and VirtualSizeBytes = 0. The cache walk
// does not run qemu-img info per file, so the virtual size is
// observed-but-unknown here (known only at create time), not a defect. A
// ListImages error (e.g. an unknown pool name mid-reconcile) yields nil + false
// for that pool so the heartbeat liveness signal is never blocked on inventory,
// and the CP preserves the prior inventory rather than clearing it
// (fail-closed).
type poolImageAdapter struct {
	ctx     context.Context
	manager *vm.Manager
	log     *slog.Logger
}

// PoolImages returns the cached-image inventory for pool and true on success
// (including a genuinely empty inventory), or nil and false when the pool could
// not be enumerated this tick.
func (a poolImageAdapter) PoolImages(pool string) ([]heartbeat.PoolImageReport, bool) {
	images, err := a.manager.ListImages(a.ctx, pool)
	if err != nil {
		a.log.Warn("heartbeat: pool image inventory unavailable", "pool", pool, "error", err.Error())
		return nil, false
	}
	out := make([]heartbeat.PoolImageReport, 0, len(images))
	for _, img := range images {
		out = append(out, heartbeat.PoolImageReport{
			Basename:         img.Basename,
			SHA256:           img.ChecksumSHA256,
			SizeBytes:        img.SizeBytes,
			VirtualSizeBytes: 0,
			Format:           img.Format,
			ImportedAt:       img.ImportedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out, true
}

// poolSnapshotAdapter implements heartbeat.PoolSnapshotLister over
// vm.Manager.ListSnapshots. It maps each vm.SnapshotBlob to the heartbeat
// PoolSnapshotReport wire shape (the on-node blob path is dropped — observed
// state never surfaces on-node file paths; identity is the sha256). A
// ListSnapshots error (e.g. an unknown pool name mid-reconcile) yields nil +
// false for that pool so the heartbeat liveness signal is never blocked on
// inventory, and the CP preserves the prior inventory rather than clearing it
// (fail-closed). Mirrors poolImageAdapter.
type poolSnapshotAdapter struct {
	ctx     context.Context
	manager *vm.Manager
	log     *slog.Logger
}

// PoolSnapshots returns the cached snapshot-blob inventory for pool and true on
// success (including a genuinely empty inventory), or nil and false when the
// pool could not be enumerated this tick.
func (a poolSnapshotAdapter) PoolSnapshots(pool string) ([]heartbeat.PoolSnapshotReport, bool) {
	blobs, err := a.manager.ListSnapshots(a.ctx, pool)
	if err != nil {
		a.log.Warn("heartbeat: pool snapshot inventory unavailable", "pool", pool, "error", err.Error())
		return nil, false
	}
	out := make([]heartbeat.PoolSnapshotReport, 0, len(blobs))
	for _, b := range blobs {
		out = append(out, heartbeat.PoolSnapshotReport{
			SHA256:    b.SHA256,
			SizeBytes: b.SizeBytes,
		})
	}
	return out, true
}

// buildSender constructs the agent → CP heartbeat sender. It returns nil
// (logging a WARN) when the heartbeat path cannot be initialised:
// heartbeats are fire-and-forget from the agent's perspective, so
// misconfiguration must not block the rest of the agent (vm lifecycle,
// console, etc.) from running. The returned Sender is the single live
// instance — it backs both the heartbeat loop (startHeartbeat) and the
// POST /v1/heartbeat/nudge handler (no second Sender is constructed).
func buildSender(ctx context.Context, cfg *config.AgentConfig, nodeName string, manager *vm.Manager, artStore *artifactstore.Store, poolRec *reconciler.Pools, vmRec *reconciler.VMs, netRec *reconciler.Networks, wgRec *reconciler.WireGuard, healthRec *reconciler.Health, pubRec *reconciler.PublishedListeners, caStore *ingress.SessionCAStore, log *slog.Logger) *heartbeat.Sender {
	if nodeName == "" {
		log.Warn("heartbeat disabled: node_name is empty (cert CN parse failed upstream)")
		return nil
	}
	if cfg.ControlPlane.URL == "" {
		log.Warn("heartbeat disabled: control_plane.url is empty")
		return nil
	}

	deps := heartbeat.CollectorDeps{
		VMs:                manager,
		VMReporter:         vmRec,
		Pools:              poolRec,
		PoolImages:         poolImageAdapter{ctx: ctx, manager: manager, log: log},
		PoolSnapshots:      poolSnapshotAdapter{ctx: ctx, manager: manager, log: log},
		Blobs:              blobInventoryAdapter{store: artStore, log: log},
		Networks:           netRec,
		WireGuard:          wgRec,
		HealthChecks:       healthRec,
		PublishedListeners: pubRec,
		Migration:          cfg.Migration,
		QEMU:               cfg.QEMU,
	}
	// The image cache tier store may be nil when no artifacts root is
	// configured; only wire the adapter when present so it never dereferences a
	// nil store in List.
	if imgStore := manager.ImageStore(); imgStore != nil {
		deps.ImageBlobs = imageInventoryAdapter{store: imgStore, log: log}
	}
	// A co-located ingress plane self-reports its ingress endpoint so the CP can
	// hand out the splicer address once the gateway role is enabled. Reported only
	// when the plane is active (caStore != nil); a plain hypervisor leaves it empty.
	if caStore != nil {
		deps.IngressAdvertisedEndpoint = cfg.Gateway.AdvertisedEndpoint
	}
	collector, err := heartbeat.NewLinux(deps)
	if err != nil {
		log.Warn("heartbeat disabled: collector init failed", "error", err.Error())
		return nil
	}

	client, err := heartbeat.NewClient(heartbeat.ClientConfig{
		CPEndpoint: cfg.ControlPlane.URL,
		NodeName:   nodeName,
		TLS:        cfg.TLS,
	})
	if err != nil {
		log.Warn("heartbeat disabled: client init failed", "error", err.Error())
		return nil
	}

	// MultiResponseHandler fans the heartbeat response to every
	// reconciler. Pool reconciler consumes declared_pools; VM
	// reconciler consumes declared_vms; network reconciler consumes
	// declared_networks; WireGuard reconciler consumes self_overlay_ip +
	// declared_wireguard_peers; health reconciler consumes
	// declared_health_checks; published-listener reconciler consumes
	// declared_load_balancers. Each ignores the others' payload without
	// needing to know about it. A co-located ingress plane adds its
	// session-CA store so the same loop keeps the connect gate's key fresh.
	handler := heartbeat.MultiResponseHandler{poolRec, vmRec, netRec, wgRec, healthRec, pubRec}
	if caStore != nil {
		handler = append(handler, caStore)
	}
	return heartbeat.NewSender(collector, client, handler, heartbeat.SenderConfig{
		Interval: cfg.ControlPlane.HeartbeatInterval,
	}, log)
}

// startHeartbeat launches the heartbeat loop for sender alongside the
// HTTPS server. Returns a channel that is closed when the goroutine exits
// (so Run can wait for clean shutdown before returning). A nil sender
// (heartbeat disabled by buildSender) yields an already-closed channel.
func startHeartbeat(ctx context.Context, sender *heartbeat.Sender, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if sender == nil {
		close(done)
		return done
	}

	go func() {
		defer close(done)
		log.Info("heartbeat sender starting")
		if err := sender.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("heartbeat sender stopped with error", "error", err.Error())
		}
		log.Info("heartbeat sender stopped")
	}()
	return done
}

// nudgerFor returns the live Sender as the route's Nudger, or a no-op when
// the heartbeat sender could not be built (heartbeat disabled). Either way
// POST /v1/heartbeat/nudge answers 204; with no loop there is simply
// nothing to nudge.
func nudgerFor(sender *heartbeat.Sender) heartbeatHandlers.Nudger {
	if sender == nil {
		return noopNudger{}
	}
	return sender
}

// noopNudger satisfies heartbeatHandlers.Nudger when the heartbeat sender
// could not be built. The endpoint still answers 204; there is simply no
// loop to nudge, so the call is a harmless no-op.
type noopNudger struct{}

// Nudge does nothing.
func (noopNudger) Nudge() {}

// buildRouter constructs the chi router with the standard middleware
// chain (mirrors CP-side `internal/api.NewRouter`): RequestID first so
// every other middleware can read the id, Logger second so it observes
// final status, Recoverer third so panics turn into the standard error
// envelope. middleware.Timeout is NOT applied at root — long-lived
// WebSocket routes (vms.consoleStream) must opt out because the pump
// goroutines inherit r.Context() and would terminate at
// cfg.Server.ReadTimeout (~30s by default). The bounded-REST subtree
// below opts back in via a Group.
func buildRouter(cfg *config.AgentConfig, nodeName string, log *slog.Logger, manager *vm.Manager, consoleTokens *console.TokenStore, dhcpResponder dhcp4.Responder, heartbeatNudger heartbeatHandlers.Nudger, blobsHandler *blobshandlers.Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(log))
	r.Use(middleware.Recoverer(log))
	r.Use(middleware.RequireCPIdentity(log))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "the requested resource was not found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusMethodNotAllowed,
			response.CodeMethodNotAllowed, "method not allowed for this resource", nil)
	})

	vmsHandler := vmshandlers.New(manager, consoleTokens, log, cfg.Migration.Host, dhcpResponder)
	tasksHandler := taskshandlers.New(manager, log)
	storagePoolsHandler := storagepoolshandlers.New(manager, log)

	// Streaming endpoints - registered before the Timeout Group below.
	// Hijacked http.Server deadlines also cleared inside each handler.
	r.Get("/v1/vms/{vm_name}/console-stream", vmsHandler.ConsoleStream)
	r.Get("/v1/vms/{vm_name}/ssh-pipe", vmsHandler.SSHPipe)
	r.Get("/v1/vms/{vm_name}/logs", vmsHandler.Logs)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(cfg.Server.ReadTimeout))

		r.Get("/health", healthHandler(nodeName, log))

		r.Route("/v1", func(r chi.Router) {
			r.Route("/vms", vmsHandler.Mount)
			r.Route("/tasks", tasksHandler.Mount)
			r.Route("/storage-pools", storagePoolsHandler.Mount)
			// CP-driven blob pull control endpoints. Same CP-only
			// RequireCPIdentity group: only the CP calls serve / pull. The blob
			// DATA path is the separate blobpeer listener (peer node certs).
			r.Route("/blobs", blobsHandler.Mount)
			r.Post("/heartbeat/nudge", heartbeatHandlers.New(heartbeatNudger).Nudge)
			// Node-level snapshot delete keyed on the immutable vm_id (NOT the live
			// VM): the blob GC must outlive the source VM, so it is mounted here
			// rather than under /vms/{vm_name}.
			r.Delete("/snapshots/{vm_id}/{snapshot_name}", vmsHandler.SnapshotDeleteByID)
		})
	})

	return r
}

// loadTLS builds a TLS config that requires and verifies a client cert
// signed by the CA at cfg.CACertPath, and presents the agent's own cert
// from cfg.CertPath / cfg.KeyPath.
func loadTLS(cfg config.TLSConfig) (*tls.Config, error) {
	caData, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("parse ca: invalid PEM in %s", cfg.CACertPath)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}, nil
}

func healthHandler(nodeName string, log *slog.Logger) http.HandlerFunc {
	v := version.Current()
	return func(w http.ResponseWriter, r *http.Request) {
		log.Info("health check", "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"node_name": nodeName,
			"version":   "iteration-1",
			"build":     v.Version,
			"time":      time.Now().UTC().Format(time.RFC3339),
		})
	}
}
