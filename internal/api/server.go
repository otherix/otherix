// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package api hosts the REST API server (chi router, middleware, handlers)
// and, in subsequent tasks, the in-process river worker pool. Built into
// the otherix-api binary.
package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// schedulerResourcesFromConfig translates the koanf-bound
// config.ResourcesConfig into the scheduler-local mirror.
// internal/scheduler is a leaf package so it does not import config —
// the api binary copies fields at construction time. Field shapes are
// kept in lock-step by hand; the per-resource Validate calls run at
// startup so an inconsistent ratio cannot reach this conversion.
func schedulerResourcesFromConfig(c config.ResourcesConfig) scheduler.ResourcesConfig {
	convert := func(r config.ResourceConfig) scheduler.ResourceConfig {
		return scheduler.ResourceConfig{
			Enabled:         r.Enabled,
			OvercommitRatio: r.OvercommitRatio,
		}
	}
	return scheduler.ResourcesConfig{
		CPU:    convert(c.CPU),
		Memory: convert(c.Memory),
		Disk:   convert(c.Disk),
	}
}

// Server wraps net/http.Server with the api-server's lifecycle: a single
// Run call that blocks until the supplied context is cancelled, then
// performs a graceful shutdown bounded by ServerConfig.ShutdownGrace.
//
// When the optional agent listener is configured (AgentServer.Enabled),
// the same Server runs a second TLS-only HTTP server on a different
// address with mTLS. Both listeners share the lifecycle.
type Server struct {
	httpServer  *http.Server
	agentServer *http.Server
	log         *slog.Logger
	grace       time.Duration
}

// NewServer constructs a Server bound to cfg, the given Store + auth
// service + river client, and the supplied logger. The router is built
// here; callers do not need to wire it up themselves. When
// cfg.AgentServer.Enabled is true, the agent listener is wired
// alongside the user listener — material's Cert + ClusterCA build the
// TLS config (mTLS on the agent listener side, ClientCAs = cluster CA
// trust anchor).
//
// material is produced upstream by LoadOrGenerateCPCert. When
// AgentServer.Enabled is false the material may be zero (Source =
// "skipped") and no listener is constructed; the validation here only
// activates when AgentServer.Enabled = true.
//
// The river client is required because at least one user-facing route
// (`tasks.cancel`) needs transactional access to river_job. Tests that
// build the router directly via NewRouter may pass a real client (the
// JobCancelTx call works on the pool without Start) or a no-op fixture.
func NewServer(cfg config.APIConfig, s *store.Store, riverClient *river.Client[pgx.Tx], imageDeleter storagepoolshandlers.ImageDeleter, vmLifecycle vmshandlers.LifecycleDeps, vmConsole vmshandlers.ConsoleDeps, authSvc *auth.Service, material TLSMaterial, log *slog.Logger) (*Server, error) {
	handler := NewRouter(RouterDeps{
		Store:              s,
		AuthService:        authSvc,
		RiverClient:        riverClient,
		ImageDeleter:       imageDeleter,
		StoragePools:       cfg.StoragePools,
		Logger:             log,
		RequestTimeout:     cfg.Server.WriteTimeout,
		PlacementAlgorithm: cfg.Placement.Algorithm,
		PlacementResources: schedulerResourcesFromConfig(cfg.Placement.Resources),
		PressureMemory:     cfg.Placement.Pressure.Memory,
		PressureSystemDisk: cfg.Placement.Pressure.SystemDisk,
		PressureDisk:       cfg.Placement.Pressure.Disk,
		VMLifecycle:        vmLifecycle,
		VMConsole:          vmConsole,
	})

	srv := &Server{
		httpServer: &http.Server{
			Addr:              cfg.Server.Listen,
			Handler:           handler,
			ReadTimeout:       cfg.Server.ReadTimeout,
			ReadHeaderTimeout: cfg.Server.ReadTimeout,
			WriteTimeout:      cfg.Server.WriteTimeout,
			IdleTimeout:       120 * time.Second,
		},
		log:   log,
		grace: cfg.Server.ShutdownGrace,
	}

	if cfg.AgentServer.Enabled {
		tlsCfg, err := buildAgentServerTLSConfig(material)
		if err != nil {
			return nil, fmt.Errorf("agent server tls: %v", err)
		}
		agentHandler := NewAgentRouter(RouterDeps{
			Store:              s,
			AuthService:        authSvc,
			RiverClient:        riverClient,
			ImageDeleter:       imageDeleter,
			Logger:             log,
			RequestTimeout:     cfg.Server.WriteTimeout,
			PressureMemory:     cfg.Placement.Pressure.Memory,
			PressureSystemDisk: cfg.Placement.Pressure.SystemDisk,
			PressureDisk:       cfg.Placement.Pressure.Disk,
		})
		srv.agentServer = &http.Server{
			Addr:              cfg.AgentServer.Listen,
			Handler:           agentHandler,
			TLSConfig:         tlsCfg,
			ReadTimeout:       cfg.Server.ReadTimeout,
			ReadHeaderTimeout: cfg.Server.ReadTimeout,
			WriteTimeout:      cfg.Server.WriteTimeout,
			IdleTimeout:       120 * time.Second,
		}
	}

	return srv, nil
}

// NewServerWithListeners is a test helper that wires both listeners
// from already-built http.Servers. Production code uses NewServer;
// integration tests assemble the agent listener via httptest and
// inject it here.
func NewServerWithListeners(httpServer, agentServer *http.Server, log *slog.Logger, grace time.Duration) *Server {
	return &Server{
		httpServer:  httpServer,
		agentServer: agentServer,
		log:         log,
		grace:       grace,
	}
}

// Run starts the HTTP server (and the agent server when present) and
// blocks until either listener fails or ctx is cancelled. On
// cancellation it calls Shutdown on both with a deadline of
// ShutdownGrace and returns nil on a clean stop.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() {
		s.log.InfoContext(ctx, "http server listening", "address", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http listen: %v", err)
			return
		}
		errCh <- nil
	}()

	if s.agentServer != nil {
		go func() {
			s.log.InfoContext(ctx, "agent server listening", "address", s.agentServer.Addr)
			if err := s.agentServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("agent listen: %v", err)
				return
			}
			errCh <- nil
		}()
	}

	expected := 1
	if s.agentServer != nil {
		expected = 2
	}

	select {
	case <-ctx.Done():
		s.log.InfoContext(ctx, "http server shutting down", "grace", s.grace)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.grace)
		defer cancel()
		var firstErr error
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			firstErr = fmt.Errorf("shutdown: %v", err)
		}
		if s.agentServer != nil {
			if err := s.agentServer.Shutdown(shutdownCtx); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("agent shutdown: %v", err)
			}
		}
		// Drain both ListenAndServe goroutines.
		for i := 0; i < expected; i++ {
			<-errCh
		}
		return firstErr
	case err := <-errCh:
		return err
	}
}

// buildAgentServerTLSConfig builds the TLS config for the agent
// listener: presents material.Cert (the replica's per-boot server
// cert), verifies client certs against material.ClusterCA (the
// cluster-wide trust anchor) only when present, pins TLS 1.2 minimum.
//
// ClientAuth is `tls.VerifyClientCertIfGiven` rather than
// `RequireAndVerifyClientCert` because the same listener serves both
// authenticated agent routes (heartbeat — gated by the AgentMTLS
// middleware which rejects connections without PeerCertificates) and the
// anonymous bootstrap routes (`/v1/ca`, `/v1/nodes/join`).
// Bootstrap clients have no cert material yet — that's the
// whole point of the bootstrap protocol — so the listener must accept
// anonymous TLS connections and let routing + middleware enforce
// per-endpoint requirements. The Step 4 real-agent smoke surfaced
// the prior posture (`RequireAndVerifyClientCert`) returning
// `tls: certificate required` to the bootstrapping agent.
//
// material is produced upstream by LoadOrGenerateCPCert. Empty
// material here is a programmer error — caller should have skipped
// this construction path when material.Skipped().
func buildAgentServerTLSConfig(material TLSMaterial) (*tls.Config, error) {
	if len(material.Cert.Certificate) == 0 {
		return nil, errors.New("material.Cert is empty (no leaf bytes)")
	}
	if material.ClusterCA == nil {
		return nil, errors.New("material.ClusterCA is nil")
	}
	pool := x509.NewCertPool()
	pool.AddCert(material.ClusterCA)
	return &tls.Config{
		Certificates: []tls.Certificate{material.Cert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
