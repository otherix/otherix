// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package gateway implements the State B runtime of the otherix-gateway
// daemon: a VM-less ingress gateway that reuses the agent's WireGuard +
// overlay substrate but never constructs a VM manager or runs the anycast
// services plane. It joins the WireGuard mesh, brings up each declared
// overlay's datapath (bridge + VXLAN + FDB + its own tenant address),
// heartbeats to the control plane, and serves the CP-identity control
// endpoints (the heartbeat nudge and the connect/splice route).
package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	heartbeatHandlers "github.com/otherix/otherix/internal/agent/handlers/heartbeat"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/ingress"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/reconciler"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agent/wgkey"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/version"
)

// Run starts the gateway HTTPS (mTLS) server and the State B runtime, and
// blocks until ctx is cancelled or the server fails. It builds only the
// WireGuard reconciler and a gateway-mode network reconciler (no VM
// manager, no qemu, no blob store), the agent → CP heartbeat sender, and a
// router serving the CP-identity control endpoints. nodeName is the
// operator-supplied name parsed from the gateway cert CN by the caller.
func Run(ctx context.Context, cfg *config.AgentConfig, nodeName string, log *slog.Logger) error {
	tlsCfg, err := loadTLS(cfg.TLS)
	if err != nil {
		return fmt.Errorf("load tls: %w", err)
	}

	// Single fabric shared by the WireGuard reconciler (otwg0) and the
	// gateway-mode network reconciler (bridge / VTEP / FDB / tenant addr).
	// Linux-only impl; an unsupported stub on other platforms keeps the
	// binary compiling for development.
	fabric := netfabric.New()

	recs, err := buildReconcilers(cfg, fabric, log)
	if err != nil {
		return err
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	// Build the heartbeat sender BEFORE the router so the same live Sender
	// the gateway posts heartbeats with backs POST /v1/heartbeat/nudge. A
	// nil Sender (misconfiguration) yields a no-op nudger so the endpoint
	// still answers 204 without a nil-pointer panic.
	// The session-CA store learns the ingress-session CA public half from the
	// heartbeat response (down-channel) and backs the connect route's credential
	// gate. It is wired into the heartbeat sender's response fan-out and read by
	// the gate, so the same heartbeat loop that drives the reconcilers keeps the
	// verification key fresh.
	caStore := ingress.NewSessionCAStore(log)

	sender := buildSender(heartbeatCtx, cfg, nodeName, recs.networks, recs.wireGuard, caStore, log)
	router := buildRouter(cfg, nodeName, log, nudgerFor(sender), ingress.ConnectDeps{
		Fabric:   fabric,
		Overlays: recs.networks,
		CAStore:  caStore,
	})

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      router,
		TLSConfig:    tlsCfg,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	heartbeatDone := startHeartbeat(heartbeatCtx, sender, log)
	netReconcilerDone := runReconciler(heartbeatCtx, "network reconciler", recs.networks.Run, log)
	wgReconcilerDone := runReconciler(heartbeatCtx, "wireguard reconciler", recs.wireGuard.Run, log)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	dones := []namedDone{
		{name: "heartbeat sender", done: heartbeatDone},
		{name: "network reconciler", done: netReconcilerDone},
		{name: "wireguard reconciler", done: wgReconcilerDone},
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

// gatewayReconcilers bundles the two per-resource reconcilers a gateway
// runs. There is deliberately no VM or pool reconciler and no VM manager:
// a gateway hosts no VMs.
type gatewayReconcilers struct {
	wireGuard *reconciler.WireGuard
	networks  *reconciler.Networks
}

// buildReconcilers constructs the WireGuard reconciler and the gateway-mode
// network reconciler. The network reconciler is built via NewGatewayNetworks
// so it brings up each declared overlay's datapath without the anycast
// services plane. Any construction failure is fatal.
func buildReconcilers(cfg *config.AgentConfig, fabric netfabric.Fabric, log *slog.Logger) (gatewayReconcilers, error) {
	networks, err := reconciler.NewGatewayNetworks(fabric, log, 0)
	if err != nil {
		return gatewayReconcilers{}, fmt.Errorf("network reconciler: %w", err)
	}
	wgKey, err := wgkey.LoadOrGenerateKey(cfg.WireGuard.PrivateKeyPath)
	if err != nil {
		return gatewayReconcilers{}, fmt.Errorf("wireguard key: %w", err)
	}
	wireGuard, err := reconciler.NewWireGuard(fabric, wgKey, cfg.WireGuard, log, 0)
	if err != nil {
		return gatewayReconcilers{}, fmt.Errorf("wireguard reconciler: %w", err)
	}
	return gatewayReconcilers{wireGuard: wireGuard, networks: networks}, nil
}

// noVMs is the empty VMLister a gateway feeds the heartbeat collector: a
// gateway runs no VMs, so its VM inventory is always empty. The collector
// requires a non-nil lister; this satisfies it without a VM manager.
type noVMs struct{}

// List returns the empty VM list.
func (noVMs) List() []*vm.VM { return nil }

// buildSender constructs the gateway → CP heartbeat sender. It returns nil
// (logging a WARN) when the heartbeat path cannot be initialised, so a
// misconfiguration never blocks the rest of the runtime. The returned
// Sender backs both the heartbeat loop and POST /v1/heartbeat/nudge.
func buildSender(ctx context.Context, cfg *config.AgentConfig, nodeName string, netRec *reconciler.Networks, wgRec *reconciler.WireGuard, caStore *ingress.SessionCAStore, log *slog.Logger) *heartbeat.Sender {
	if nodeName == "" {
		log.Warn("heartbeat disabled: node_name is empty (cert CN parse failed upstream)")
		return nil
	}
	if cfg.ControlPlane.URL == "" {
		log.Warn("heartbeat disabled: control_plane.url is empty")
		return nil
	}

	collector, err := heartbeat.NewLinux(heartbeat.CollectorDeps{
		VMs:       noVMs{},
		Networks:  netRec,
		WireGuard: wgRec,
	})
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

	// MultiResponseHandler fans the heartbeat response to the reconcilers and the
	// session-CA store: the WireGuard reconciler consumes self_overlay_ip +
	// declared peers, the network reconciler consumes declared_networks +
	// declared_fdb, and the session-CA store consumes session_ca_public_pem to
	// arm the connect route's credential gate.
	handler := heartbeat.MultiResponseHandler{netRec, wgRec, caStore}
	return heartbeat.NewSender(collector, client, handler, heartbeat.SenderConfig{
		Interval: cfg.ControlPlane.HeartbeatInterval,
	}, log)
}

// nudgerFor returns the live Sender as the route's Nudger, or a no-op when
// the heartbeat sender could not be built. Either way POST
// /v1/heartbeat/nudge answers 204.
func nudgerFor(sender *heartbeat.Sender) heartbeatHandlers.Nudger {
	if sender == nil {
		return noopNudger{}
	}
	return sender
}

// noopNudger satisfies heartbeatHandlers.Nudger when the heartbeat sender
// could not be built. The endpoint still answers 204; there is no loop to
// nudge, so the call is a harmless no-op.
type noopNudger struct{}

// Nudge does nothing.
func (noopNudger) Nudge() {}

// buildRouter constructs the chi router with the standard middleware chain. The
// gateway surface is intentionally tiny and splits into two trust domains. The
// control routes - the health probe and the heartbeat nudge - are gated by
// RequireCPIdentity so only the control plane reaches them; a stolen node cert
// (itself a valid cluster-CA client cert) is rejected before any handler runs.
// The connect/splice route is reached by the CLI directly with a short-lived
// ingress session credential and is gated instead by the credential gate, not
// by a client certificate. RequireCPIdentity is therefore applied per-group
// rather than at the top level, so lowering the listener to accept a
// certificate-less connect client never widens the control routes: they still
// fail closed on a missing or non-CP client cert.
func buildRouter(cfg *config.AgentConfig, nodeName string, log *slog.Logger, heartbeatNudger heartbeatHandlers.Nudger, connect ingress.ConnectDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(log))
	r.Use(middleware.Recoverer(log))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "the requested resource was not found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusMethodNotAllowed,
			response.CodeMethodNotAllowed, "method not allowed for this resource", nil)
	})

	// CP-only control routes: health probe and heartbeat nudge, gated by
	// CP identity and bounded by the per-request timeout.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireCPIdentity(log))
		r.Use(middleware.Timeout(cfg.Server.ReadTimeout))

		r.Get("/health", healthHandler(nodeName, log))

		r.Route("/v1", func(r chi.Router) {
			r.Post("/heartbeat/nudge", heartbeatHandlers.New(heartbeatNudger).Nudge)
		})
	})

	// The connect/splice route verifies a short-lived ingress session credential
	// (its credential gate, not CP identity) and then hijacks the inbound
	// connection and splices it to the credential's guest target on the overlay.
	// It is deliberately outside the Timeout group: a long-lived spliced session
	// must not be killed by the per-request deadline, and the timeout's guarded
	// writer does not support hijacking.
	ingress.MountConnectRoutes(r, ingress.NewConnectHandler(connect, log))

	return r
}

// namedDone pairs a reconciler/sender done-channel with its name so a drain
// timeout can report exactly which goroutine failed to exit.
type namedDone struct {
	name string
	done <-chan struct{}
}

// drainReconcilers waits up to timeout for every done channel to close,
// logging a WARN naming any that did not drain in time. It never blocks
// past the bound: a goroutine stuck in an uninterruptible netfabric syscall
// is abandoned rather than waited on indefinitely. The process is
// terminating, so abandoning the wait is equivalent to the SIGKILL-mid-op
// state the reconcilers already recover from (retry-forever / eventually
// consistent).
func drainReconcilers(dones []namedDone, timeout time.Duration, log *slog.Logger) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for i, d := range dones {
		select {
		case <-d.done:
			// Drained.
		case <-timer.C:
			var stuck []string
			for _, rem := range dones[i:] {
				select {
				case <-rem.done:
				default:
					stuck = append(stuck, rem.name)
				}
			}
			log.Warn("reconciler drain timed out; proceeding with shutdown",
				"timeout", timeout, "stuck", stuck)
			return
		}
	}
}

// runReconciler launches one reconciler's Run loop in a goroutine and
// returns a channel closed when it exits. context.Canceled is the expected
// shutdown signal and is logged at info, not error.
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

// startHeartbeat launches the heartbeat loop for sender alongside the HTTPS
// server. Returns a channel closed when the goroutine exits. A nil sender
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

// loadTLS builds the gateway's TLS config: it presents the gateway's own cert
// and verifies any client cert that IS presented against the cluster CA at
// cfg.CACertPath, but does not require one. The single leaf serves both the
// inbound listener and the outbound heartbeat dialer.
//
// ClientAuth is VerifyClientCertIfGiven rather than RequireAndVerifyClientCert
// because the connect route is reached by the CLI directly with a bearer
// ingress session credential and no client certificate - a required-cert
// listener would reject that client at the handshake. A client that does present
// a cert is still fully cluster-CA-verified, and the CP-only control routes stay
// authoritative-gated by RequireCPIdentity, which fails closed on a missing or
// non-CP client cert. So accepting a certificate-less connect client never
// widens the control surface.
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
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    caPool,
	}, nil
}

// healthHandler answers the liveness probe with the gateway's resolved name
// and build metadata.
func healthHandler(nodeName string, log *slog.Logger) http.HandlerFunc {
	v := version.Current()
	return func(w http.ResponseWriter, r *http.Request) {
		log.Info("health check", "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"node_name": nodeName,
			"build":     v.Version,
			"time":      time.Now().UTC().Format(time.RFC3339),
		})
	}
}
