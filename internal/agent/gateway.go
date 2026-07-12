// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
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
)

// RunGateway starts the agent in gateway-only mode and blocks until ctx is
// cancelled or a listener fails. It runs no qemu, VM manager, storage pools, or
// health checks: it joins the WireGuard mesh, brings up each declared overlay's
// datapath for ingress, heartbeats to the control plane, and serves two TLS
// listeners in one process.
//
// The control listener (cfg.Server.Listen) requires and CP-gates a client cert
// - only the control plane reaches the health probe and the heartbeat nudge.
// The ingress listener (cfg.Gateway.Listen) accepts a certificate-less client
// and gates POST /v1/connect on a short-lived session credential, so the CLI can
// reach the splicer directly. The two planes are disjoint by construction:
// neither serves the other's routes.
func RunGateway(ctx context.Context, cfg *config.AgentConfig, log *slog.Logger) error {
	nodeName, err := parseNodeNameFromCert(cfg.TLS.CertPath)
	if err != nil {
		return fmt.Errorf("parse node name from cert: %w", err)
	}
	log.Info("gateway: resolved node name from cert CN", "node_name", nodeName)

	controlTLS, err := loadControlTLS(cfg.TLS)
	if err != nil {
		return fmt.Errorf("load control tls: %w", err)
	}

	// Single fabric shared by the WireGuard reconciler (otwg0) and the
	// network reconciler (bridge / VTEP / FDB / tenant addr). This node hosts no
	// VMs (hostsVMs=false), so the reconciler brings up only the overlay transport
	// plus its ingress veths, never the anycast services plane. Linux-only impl;
	// an unsupported stub on other platforms keeps the binary compiling for
	// development.
	fabric := netfabric.New()

	// hostsVMs=false (gateway-only), isGateway=true: this node is the relay
	// gateway, so the reconciler enables ip_forward every pass for A->G->B
	// WireGuard transit regardless of any egress=nat network.
	networks, err := reconciler.NewNetworks(fabric, nil, log, 0, false, true)
	if err != nil {
		return fmt.Errorf("network reconciler: %w", err)
	}
	wgKey, err := wgkey.LoadOrGenerateKey(cfg.WireGuard.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("wireguard key: %w", err)
	}
	wireGuard, err := reconciler.NewWireGuard(fabric, wgKey, cfg.WireGuard, log, 0)
	if err != nil {
		return fmt.Errorf("wireguard reconciler: %w", err)
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	// The shared ingress plane: the bearer-only /v1/connect listener plus the
	// session-CA store that learns the ingress-session CA public half from the
	// heartbeat response (down-channel) and backs the connect route's credential
	// gate. The store is wired into the heartbeat sender's response fan-out and
	// read by the gate, so the same heartbeat loop that drives the reconcilers
	// keeps the verification key fresh.
	plane, err := ingress.BuildPlane(ingress.PlaneDeps{
		Listen:       cfg.Gateway.Listen,
		TLS:          cfg.TLS,
		Fabric:       fabric,
		Overlays:     networks,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("ingress plane: %w", err)
	}

	sender := buildGatewaySender(heartbeatCtx, cfg, nodeName, networks, wireGuard, plane.CAStore, log)

	// The CP-only node-connect splice. Its anti-SSRF gate validates a CP-supplied
	// target against the gateway's known-node overlay set (the WG reconciler's
	// declared peers) on the agent control port, so it can only ever splice to a
	// meshed node's overlay IP, never a guest VM or the anycast service IP.
	nodeConnect := ingress.NewNodeConnectHandler(ingress.NodeConnectDeps{
		IsKnownNodeOverlayIP: wireGuard.IsKnownNodeOverlayIP,
		ControlPort:          controlListenPort(cfg.Server.Listen),
		Log:                  log,
	})

	controlSrv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      buildGatewayControlRouter(cfg, nodeName, log, nudgerFor(sender), nodeConnect),
		TLSConfig:    controlTLS,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}
	ingressSrv := plane.Server

	heartbeatDone := startHeartbeat(heartbeatCtx, sender, log)
	netReconcilerDone := runReconciler(heartbeatCtx, "network reconciler", networks.Run, log)
	wgReconcilerDone := runReconciler(heartbeatCtx, "wireguard reconciler", wireGuard.Run, log)

	dones := []namedDone{
		{name: "heartbeat sender", done: heartbeatDone},
		{name: "network reconciler", done: netReconcilerDone},
		{name: "wireguard reconciler", done: wgReconcilerDone},
	}

	// Buffered for both listener goroutines: at a clean shutdown BOTH return
	// http.ErrServerClosed and send, so a cap-1 channel would block the second
	// sender forever (goroutine leak, and a hung return on cancel).
	errc := make(chan error, 2)
	serve := func(name string, srv *http.Server) {
		log.Info("listening", "plane", name, "addr", srv.Addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("%s listener: %w", name, err)
			return
		}
		errc <- nil
	}
	go serve("control", controlSrv)
	go serve("ingress", ingressSrv)

	// Wait for a shutdown signal or the first listener failure. Either way both
	// servers are shut down (fail toward inaction: a bind failure tears the whole
	// runtime down rather than leaving a half-up gateway), then the heartbeat loop
	// stops and the reconcilers drain.
	var runErr error
	received := 0
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errc:
		received++
		if err != nil {
			log.Error("gateway listener failed; shutting down", "error", err.Error())
			runErr = err
		}
	}

	shutdownGatewayServers(cfg.Server.ShutdownGrace, log, controlSrv, ingressSrv)

	// Drain the remaining listener returns so both goroutines have exited (their
	// listeners fully closed) before returning: without this a torn-down sibling
	// listener may still be bound when RunGateway returns.
	for received < 2 {
		if err := <-errc; err != nil && runErr == nil {
			runErr = err
		}
		received++
	}

	stopHeartbeat()
	drainReconcilers(dones, cfg.Server.ShutdownGrace, log)
	return runErr
}

// shutdownGatewayServers gracefully shuts down every server, bounded by grace,
// logging any shutdown error. Both are always shut down so a failure on one
// listener never leaks the sibling's live listener.
func shutdownGatewayServers(grace time.Duration, log *slog.Logger, servers ...*http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("gateway server shutdown failed", "addr", srv.Addr, "error", err.Error())
		}
	}
}

// noVMs is the empty VMLister a gateway feeds the heartbeat collector: a gateway
// runs no VMs, so its VM inventory is always empty. The collector requires a
// non-nil lister; this satisfies it without a VM manager.
type noVMs struct{}

// List returns the empty VM list.
func (noVMs) List() []*vm.VM { return nil }

// buildGatewaySender constructs the gateway to CP heartbeat sender. It returns
// nil (logging a WARN) when the heartbeat path cannot be initialised, so a
// misconfiguration never blocks the rest of the runtime. The returned Sender
// backs both the heartbeat loop and POST /v1/heartbeat/nudge.
//
// It is named distinctly from the hypervisor buildSender in server.go, whose
// signature is VM-manager-specific and incompatible with a gateway.
func buildGatewaySender(ctx context.Context, cfg *config.AgentConfig, nodeName string, netRec *reconciler.Networks, wgRec *reconciler.WireGuard, caStore *ingress.SessionCAStore, log *slog.Logger) *heartbeat.Sender {
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

// buildGatewayControlRouter builds the CP-only control listener's router. Unlike
// the shipped daemon (which mounted control routes behind a RequireCPIdentity
// group on a shared listener), RequireCPIdentity is applied at the top level:
// this listener is dedicated to the control plane and has no certificate-less
// route to accommodate, so a global gate leaves no route certless-reachable.
func buildGatewayControlRouter(cfg *config.AgentConfig, nodeName string, log *slog.Logger, heartbeatNudger heartbeatHandlers.Nudger, nodeConnect *ingress.NodeConnectHandler) http.Handler {
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

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(cfg.Server.ReadTimeout))

		r.Get("/health", healthHandler(nodeName, log))
		r.Post("/v1/heartbeat/nudge", heartbeatHandlers.New(heartbeatNudger).Nudge)
	})

	// Mounted OUTSIDE the Timeout group: it hijacks and splices a long-lived
	// tunnel, which a per-request deadline would tear. Still under the router-root
	// RequireCPIdentity gate, so only the control plane can reach it.
	ingress.MountNodeConnectRoute(r, nodeConnect)

	return r
}

// controlListenPort extracts the port from the control listener address so the
// node-connect splice validates a CP-supplied target against the agent control
// port (a cluster convention: every node's control listener shares this port).
// A malformed address falls back to the default control port rather than failing
// the gateway.
func controlListenPort(listen string) int {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return defaultControlPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return defaultControlPort
	}
	return port
}

// defaultControlPort mirrors the agent config default (config/agent.go
// "0.0.0.0:9443"): the port a node's mTLS control listener binds by convention.
const defaultControlPort = 9443

// loadControlTLS builds the control listener's TLS config: it presents the
// gateway's own leaf and requires and verifies a client cert signed by the
// cluster CA at cfg.CACertPath. Only the control plane, presenting its CP cert,
// completes the handshake.
func loadControlTLS(cfg config.TLSConfig) (*tls.Config, error) {
	base, err := gatewayLeafTLS(cfg)
	if err != nil {
		return nil, err
	}
	base.ClientAuth = tls.RequireAndVerifyClientCert
	return base, nil
}

// gatewayLeafTLS loads the gateway leaf cert/key and the cluster CA pool into a
// base TLS config (TLS 1.3, cluster CA as ClientCAs). The caller sets ClientAuth
// for its plane.
func gatewayLeafTLS(cfg config.TLSConfig) (*tls.Config, error) {
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
		ClientCAs:    caPool,
	}, nil
}
