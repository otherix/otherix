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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/console"
	storagepoolshandlers "github.com/otherix/otherix/internal/agent/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/agent/handlers/tasks"
	vmshandlers "github.com/otherix/otherix/internal/agent/handlers/vms"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/reconciler"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/version"
)

// nodeCNPrefix is the literal prefix all agent cert CommonName values
// share — `node-<name>` per internal/auth/csr.go SignCSR template.
// parseNodeNameFromCert strips this prefix к surface the operator-
// supplied name.
const nodeCNPrefix = "node-"

// parseNodeNameFromCert loads the agent's own cert от certPath, parses
// the leaf, и returns the operator-supplied node name encoded в the
// Subject CN. The cert template (internal/auth/csr.go SignCSR) emits
// CN `node-<nodeName>`; this function strips the prefix.
//
// The agent identifies itself via the cert CN rather than а separate
// sidecar file. Cert tampering is out of scope - the file is owned
// by the agent uid и the CP-side mTLS verifier validates fingerprint
// + chain.
func parseNodeNameFromCert(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath) //nolint:gosec // path is operator-configured (agent.yaml tls.cert_path); the function exists к open this exact file
	if err != nil {
		return "", fmt.Errorf("read cert %s: %v", certPath, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("cert %s: not а CERTIFICATE PEM block", certPath)
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
// Name-keyed agent identity: the node name is parsed от the cert CN
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
	log.Info("agent: resolved node name от cert CN", "node_name", nodeName)

	manager, err := vm.New(cfg, log)
	if err != nil {
		return fmt.Errorf("vm manager: %w", err)
	}

	// Pool reconciler. Consumes desired_pools от heartbeat responses
	// и mutates manager's pool registry. Same instance plugs into the
	// heartbeat sender as both ResponseHandler и PoolReporter.
	poolReconciler, err := reconciler.NewPools(manager, log, 0)
	if err != nil {
		return fmt.Errorf("pool reconciler: %w", err)
	}

	// VM reconciler. Mirrors the pool reconciler shape - single
	// ownership of observed VM state (heartbeat.VMReporter) и
	// desired-vs-observed convergence (heartbeat.ResponseHandler).
	vmReconciler, err := reconciler.NewVMs(manager, log, 0)
	if err != nil {
		return fmt.Errorf("vm reconciler: %w", err)
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
	heartbeatDone := startHeartbeat(heartbeatCtx, cfg, nodeName, manager, poolReconciler, vmReconciler, log)

	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		log.Info("pool reconciler starting")
		if err := poolReconciler.Run(heartbeatCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("pool reconciler stopped with error", "error", err.Error())
		}
		log.Info("pool reconciler stopped")
	}()

	vmReconcilerDone := make(chan struct{})
	go func() {
		defer close(vmReconcilerDone)
		log.Info("vm reconciler starting")
		if err := vmReconciler.Run(heartbeatCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("vm reconciler stopped with error", "error", err.Error())
		}
		log.Info("vm reconciler stopped")
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errc:
		stopHeartbeat()
		<-heartbeatDone
		<-reconcilerDone
		<-vmReconcilerDone
		return err
	}

	stopHeartbeat()
	<-heartbeatDone
	<-reconcilerDone
	<-vmReconcilerDone

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// startHeartbeat wires up the agent → CP heartbeat sender alongside
// the HTTPS server. Returns а channel that is closed when the
// goroutine exits (so Run can wait for clean shutdown before
// returning). Returns а never-closed-but-immediately-closed channel
// если the heartbeat path cannot be initialised: heartbeats are
// fire-and-forget от the agent's perspective; misconfiguration must
// not block the rest of the agent (vm lifecycle, console, etc.)
// from running.
func startHeartbeat(ctx context.Context, cfg *config.AgentConfig, nodeName string, manager *vm.Manager, poolRec *reconciler.Pools, vmRec *reconciler.VMs, log *slog.Logger) <-chan struct{} {
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

	// MultiResponseHandler fans the heartbeat response к both
	// reconcilers (L3 D3). Pool reconciler consumes declared_pools;
	// VM reconciler consumes declared_vms. Each ignores the other's
	// payload без needing к know about it.
	handler := heartbeat.MultiResponseHandler{poolRec, vmRec}
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
// goroutines inherit r.Context() и would terminate at
// cfg.Server.ReadTimeout (~30s by default). The bounded-REST subtree
// below opts back в via а Group.
func buildRouter(cfg *config.AgentConfig, nodeName string, log *slog.Logger, manager *vm.Manager, consoleTokens *console.TokenStore) http.Handler {
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
