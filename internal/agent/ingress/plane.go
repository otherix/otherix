// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingress

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/config"
)

// PlaneDeps carries the collaborators BuildPlane needs to stand up
// the bearer-only ingress listener: the listen address and TLS material, the
// host fabric and overlay resolver the connect splicer dials through, the server
// timeouts, and a logger.
type PlaneDeps struct {
	// Listen is the ingress listener address, distinct from the control listener.
	Listen string
	// TLS carries the leaf cert/key + cluster CA paths. The listener presents the
	// node's own leaf and verifies a client cert only if one is offered
	// (VerifyClientCertIfGiven), so a certificate-less connect client completes the
	// handshake and is gated on its session credential instead.
	TLS config.TLSConfig
	// Fabric is the host fabric the connect handler uses for the neighbor-table
	// anti-SSRF check.
	Fabric netfabric.Fabric
	// Overlays resolves a guest IP to its overlay datapath and tracks live ingress
	// sessions per network. The network reconciler (*reconciler.Networks) satisfies
	// it; a co-located node passes its single hypervisor reconciler here.
	Overlays OverlayResolver
	// ReadTimeout / WriteTimeout mirror the control listener's server timeouts.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	Log          *slog.Logger
}

// Plane is the built (but not yet started) ingress listener handle: the
// HTTP server the caller serves and shuts down, and the SessionCAStore the
// caller fans into the heartbeat response handler so the connect credential gate
// stays armed. The caller owns the serve/shutdown lifecycle so it can multiplex
// this second listener into its own listener-goroutine + shutdown discipline
// (both agent.Run and RunGateway run it alongside the control listener).
type Plane struct {
	// Server is the ingress HTTP server, configured with the bearer-only TLS
	// config and the /v1/connect router. Not started; the caller calls
	// ListenAndServeTLS and Shutdown.
	Server *http.Server
	// CAStore learns the session CA public half from the heartbeat down-channel and
	// backs the connect route's credential gate. Wire it into the heartbeat
	// response handler chain.
	CAStore *SessionCAStore
}

// BuildPlane constructs the bearer-only ingress listener shared by the
// standalone gateway daemon and a co-located hypervisor agent: it loads the
// VerifyClientCertIfGiven TLS config, creates the SessionCAStore, mounts the
// credential-gated POST /v1/connect splicer, and returns the assembled server
// handle. It does not start the server; the caller serves and shuts it down so
// the ingress listener joins the caller's existing listener-goroutine and
// graceful-shutdown discipline.
func BuildPlane(deps PlaneDeps) (*Plane, error) {
	tlsCfg, err := ingressTLS(deps.TLS)
	if err != nil {
		return nil, fmt.Errorf("load ingress tls: %w", err)
	}
	caStore := NewSessionCAStore(deps.Log)
	router := buildIngressRouter(deps.Log, ConnectDeps{
		Fabric:   deps.Fabric,
		Overlays: deps.Overlays,
		CAStore:  caStore,
	})
	srv := &http.Server{
		Addr:         deps.Listen,
		Handler:      router,
		TLSConfig:    tlsCfg,
		ReadTimeout:  deps.ReadTimeout,
		WriteTimeout: deps.WriteTimeout,
	}
	return &Plane{Server: srv, CAStore: caStore}, nil
}

// buildIngressRouter builds the ingress listener's router. It serves only the
// credential-gated POST /v1/connect splicer and is deliberately not gated by CP
// identity: the client reaches it directly with a short-lived session credential
// and no client certificate. The connect route carries no per-request timeout -
// a long-lived spliced session must not hit the deadline, and the timeout's
// guarded writer does not support hijacking.
func buildIngressRouter(log *slog.Logger, connect ConnectDeps) http.Handler {
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

	MountConnectRoutes(r, NewConnectHandler(connect, log))

	return r
}

// ingressTLS builds the ingress listener's TLS config: it presents the node's own
// leaf and verifies any client cert that IS presented against the cluster CA, but
// does not require one. A certificate-less connect client completes the handshake
// and is gated on its session credential instead.
func ingressTLS(cfg config.TLSConfig) (*tls.Config, error) {
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
		ClientAuth:   tls.VerifyClientCertIfGiven,
	}, nil
}
