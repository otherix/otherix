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
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/dnsproxy"
	storagepoolshandlers "github.com/otherix/otherix/internal/agent/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/agent/handlers/tasks"
	vmshandlers "github.com/otherix/otherix/internal/agent/handlers/vms"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/reconciler"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agent/wgkey"
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
// cancelled or the server fails. Iteration 1 mounts the full agent
// surface — /v1/vms, /v1/tasks — alongside the Iteration 0 /health
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

	// Pool reconciler. Consumes desired_pools from heartbeat responses
	// and mutates manager's pool registry. Same instance plugs into the
	// heartbeat sender as both ResponseHandler and PoolReporter.
	poolReconciler, err := reconciler.NewPools(manager, log, 0)
	if err != nil {
		return fmt.Errorf("pool reconciler: %w", err)
	}

	// Network reconciler. Consumes declared_networks from heartbeat
	// responses and materialises node-local bridges + NAT via the same
	// fabric the VM manager uses. Plugs into the sender as both
	// ResponseHandler and NetworkReporter.
	netReconciler, err := reconciler.NewNetworks(fabric, dhcpResponder, log, 0)
	if err != nil {
		return fmt.Errorf("network reconciler: %w", err)
	}

	// VM reconciler. Mirrors the pool reconciler shape - single
	// ownership of observed VM state (heartbeat.VMReporter) and
	// desired-vs-observed convergence (heartbeat.ResponseHandler).
	vmReconciler, err := reconciler.NewVMs(manager, log, 0)
	if err != nil {
		return fmt.Errorf("vm reconciler: %w", err)
	}

	// WireGuard reconciler. The key is generated lazily on first serve and
	// reused thereafter; its pubkey is reported up the heartbeat
	// independent of whether otwg0 exists, so the CP can allocate the
	// overlay address before the interface comes up. Plugs into the sender
	// as both ResponseHandler (self_overlay_ip + declared peers) and
	// WireGuardReporter.
	wgKey, err := wgkey.LoadOrGenerateKey(cfg.WireGuard.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("wireguard key: %w", err)
	}
	wgReconciler, err := reconciler.NewWireGuard(fabric, wgKey, cfg.WireGuard, log, 0)
	if err != nil {
		return fmt.Errorf("wireguard reconciler: %w", err)
	}

	// Console token store - in-memory, lifecycle bound to the agent
	// process; restart drops the tokens alongside the QEMU `-serial`
	// sockets they reference. The single-session lock that used to
	// live next to it now lives inside serialmux.Multiplexer
	// (ErrConsoleInUse) per ADR 0029.
	consoleTokens := console.NewTokenStore()
	consoleTokens.Start(ctx)

	router := buildRouter(cfg, nodeName, log, manager, consoleTokens)

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      router,
		TLSConfig:    tlsCfg,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	heartbeatDone := startHeartbeat(heartbeatCtx, cfg, nodeName, manager, poolReconciler, vmReconciler, netReconciler, wgReconciler, log)

	reconcilerDone := runReconciler(heartbeatCtx, "pool reconciler", poolReconciler.Run, log)
	netReconcilerDone := runReconciler(heartbeatCtx, "network reconciler", netReconciler.Run, log)
	vmReconcilerDone := runReconciler(heartbeatCtx, "vm reconciler", vmReconciler.Run, log)
	wgReconcilerDone := runReconciler(heartbeatCtx, "wireguard reconciler", wgReconciler.Run, log)
	dnsForwarderDone := runReconciler(heartbeatCtx, "dns forwarder", dnsForwarder.Run, log)
	dhcpResponderDone := runReconciler(heartbeatCtx, "dhcp responder", dhcpResponder.Run, log)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

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
		{name: "dns forwarder", done: dnsForwarderDone},
		{name: "dhcp responder", done: dhcpResponderDone},
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errc:
		stopHeartbeat()
		drainReconcilers(dones, cfg.Server.ShutdownGrace, log)
		return err
	}

	stopHeartbeat()
	drainReconcilers(dones, cfg.Server.ShutdownGrace, log)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
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

// poolImageAdapter implements heartbeat.PoolImageLister over
// vm.Manager.ListImages. It maps each vm.CachedImage to the heartbeat
// PoolImageReport wire shape: ChecksumSHA256 -> SHA256, ImportedAt (a
// time.Time) -> RFC 3339 string, and VirtualSizeBytes = 0. The cache walk
// does not run qemu-img info per file, so the virtual size is
// observed-but-unknown here (known only at create time), not a defect. A
// ListImages error (e.g. an unknown pool name mid-reconcile) yields nil for
// that pool so the heartbeat liveness signal is never blocked on inventory.
type poolImageAdapter struct {
	ctx     context.Context
	manager *vm.Manager
	log     *slog.Logger
}

// PoolImages returns the cached-image inventory for pool, or nil on error.
func (a poolImageAdapter) PoolImages(pool string) []heartbeat.PoolImageReport {
	images, err := a.manager.ListImages(a.ctx, pool)
	if err != nil {
		a.log.Warn("heartbeat: pool image inventory unavailable", "pool", pool, "error", err.Error())
		return nil
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
	return out
}

// startHeartbeat wires up the agent → CP heartbeat sender alongside
// the HTTPS server. Returns a channel that is closed when the
// goroutine exits (so Run can wait for clean shutdown before
// returning). Returns a never-closed-but-immediately-closed channel
// if the heartbeat path cannot be initialised: heartbeats are
// fire-and-forget from the agent's perspective; misconfiguration must
// not block the rest of the agent (vm lifecycle, console, etc.)
// from running.
func startHeartbeat(ctx context.Context, cfg *config.AgentConfig, nodeName string, manager *vm.Manager, poolRec *reconciler.Pools, vmRec *reconciler.VMs, netRec *reconciler.Networks, wgRec *reconciler.WireGuard, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})

	if nodeName == "" {
		log.Warn("heartbeat disabled: node_name is empty (cert CN parse failed upstream)")
		close(done)
		return done
	}
	if cfg.ControlPlane.URL == "" {
		log.Warn("heartbeat disabled: control_plane.url is empty")
		close(done)
		return done
	}

	collector, err := heartbeat.NewLinux(heartbeat.CollectorDeps{
		VMs:        manager,
		VMReporter: vmRec,
		Pools:      poolRec,
		PoolImages: poolImageAdapter{ctx: ctx, manager: manager, log: log},
		Networks:   netRec,
		WireGuard:  wgRec,
		Migration:  cfg.Migration,
		QEMU:       cfg.QEMU,
	})
	if err != nil {
		log.Warn("heartbeat disabled: collector init failed", "error", err.Error())
		close(done)
		return done
	}

	client, err := heartbeat.NewClient(heartbeat.ClientConfig{
		CPEndpoint: cfg.ControlPlane.URL,
		NodeName:   nodeName,
		TLS:        cfg.TLS,
	})
	if err != nil {
		log.Warn("heartbeat disabled: client init failed", "error", err.Error())
		close(done)
		return done
	}

	// MultiResponseHandler fans the heartbeat response to every
	// reconciler (L3 D3). Pool reconciler consumes declared_pools; VM
	// reconciler consumes declared_vms; network reconciler consumes
	// declared_networks; WireGuard reconciler consumes self_overlay_ip +
	// declared_wireguard_peers. Each ignores the others' payload without
	// needing to know about it.
	handler := heartbeat.MultiResponseHandler{poolRec, vmRec, netRec, wgRec}
	sender := heartbeat.NewSender(collector, client, handler, heartbeat.SenderConfig{
		Interval: cfg.ControlPlane.HeartbeatInterval,
	}, log)

	go func() {
		defer close(done)
		log.Info("heartbeat sender starting",
			"cp_endpoint", cfg.ControlPlane.URL,
			"interval", cfg.ControlPlane.HeartbeatInterval,
		)
		if err := sender.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("heartbeat sender stopped with error", "error", err.Error())
		}
		log.Info("heartbeat sender stopped")
	}()
	return done
}

// buildRouter constructs the chi router with the standard middleware
// chain (mirrors CP-side `internal/api.NewRouter`): RequestID first so
// every other middleware can read the id, Logger second so it observes
// final status, Recoverer third so panics turn into the standard error
// envelope. middleware.Timeout is NOT applied at root — long-lived
// WebSocket routes (vms.consoleStream) must opt out because the pump
// goroutines inherit r.Context() and would terminate at
// cfg.Server.ReadTimeout (~30s by default). The bounded-REST subtree
// below opts back in via a Group.
func buildRouter(cfg *config.AgentConfig, nodeName string, log *slog.Logger, manager *vm.Manager, consoleTokens *console.TokenStore) http.Handler {
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

	vmsHandler := vmshandlers.New(manager, consoleTokens, log)
	tasksHandler := taskshandlers.New(manager, log)
	storagePoolsHandler := storagepoolshandlers.New(manager, log)

	// Streaming endpoints - registered before the Timeout Group below.
	// Hijacked http.Server deadlines also cleared inside each handler.
	r.Get("/v1/vms/{vm_name}/console-stream", vmsHandler.ConsoleStream)
	r.Get("/v1/vms/{vm_name}/logs", vmsHandler.Logs)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(cfg.Server.ReadTimeout))

		r.Get("/health", healthHandler(nodeName, log))

		r.Route("/v1", func(r chi.Router) {
			r.Route("/vms", vmsHandler.Mount)
			r.Route("/tasks", tasksHandler.Mount)
			r.Route("/storage-pools", storagePoolsHandler.Mount)
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
