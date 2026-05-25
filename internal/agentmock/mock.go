// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package agentmock is the in-process server-side fixture for the
// Otherix Agent API contract (api/openapi/agent.yaml). It implements
// the codegen-derived agentapi.ServerInterface in two-mode TLS, drives
// an opt-in heartbeat goroutine to a Control Plane endpoint, and
// exposes a small Test API for state preload, mutation, inspection
// and fault injection.
//
// The package is contract-bound: it serves the OpenAPI surface so
// integration tests exercise the wire contract without booting a
// real agent.
//
// Six functional endpoints are implemented (health, info,
// storage-pools list/get, storage-images list, tasks.get always-404)
// out of forty-four; the remaining thirty-eight are mounted as 501
// stubs so the contract is compile-time-covered without runtime
// reach. The Test API (see Mock.Start, Mock.AddFirmware,
// Mock.SetPoolCapacity, Mock.InjectError, Mock.SendHeartbeatNow,
// etc.) is the test-author surface; production code does not import
// this package.
package agentmock

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agentapi"
)

// agentVersionString is the value reported by /v1/info and the
// heartbeat goroutine. The mock advertises a stable build-time
// constant; the real agent will populate this from internal/version.
const agentVersionString = "mock-0.1.0"

// defaultMigrationCapability is the fallback advertised when the
// caller does not set Options.MigrationCapability. The range mirrors
// the production default.
var defaultMigrationCapability = MigrationCapability{
	Host:           "127.0.0.1",
	PortRangeStart: 49152,
	PortRangeEnd:   49251,
}

// Mock is the in-process agent-mock fixture. All methods are safe for
// concurrent use unless the doc explicitly says otherwise. Construct
// it via Start; the Test API surface lives across this package.
type Mock struct {
	state *state

	// Console state - token store + per-VM single-session lock.
	// Mirror the real agent shape so the mock-driven integration
	// tests exercise the same token / 409 plumbing the production
	// agent owns. The lock is a sync.Map of vm-name -> struct{}
	// (presence == in-use), mirroring the ErrConsoleInUse semantic
	// the real agent's multiplexer enforces inside SubscribeConsole.
	consoleTokens *console.TokenStore
	consoleLocks  sync.Map

	httpServer *httptest.Server

	// Lifecycle plumbing for the heartbeat goroutine. The context
	// itself is passed to the goroutine as a parameter rather than
	// stored on the struct — only the cancel func and done channel
	// need to survive on Mock for Stop() to coordinate shutdown.
	stopOnce      sync.Once
	heartbeatStop context.CancelFunc
	heartbeatDone chan struct{}

	heartbeatHTTP *http.Client
	cpURL         string
	interval      time.Duration

	logger *slog.Logger

	// Cached immutables — read without taking state.mu.
	nodeID       uuid.UUID
	nodeName     string
	architecture string
	tlsMode      TLSMode
}

// Options configures a single Mock instance.
type Options struct {
	NodeID            uuid.UUID     // defaults to a random uuid.
	NodeName          string        // defaults to "mock-node-1".
	Architecture      string        // "amd64" | "arm64"; defaults to "amd64".
	TLS               TLSMode       // defaults to TLSDisabled.
	ControlPlaneURL   string        // required when HeartbeatInterval > 0.
	HeartbeatInterval time.Duration // 0 disables heartbeats.
	Logger            *slog.Logger  // defaults to a discard logger.
}

// RequestRecord is a captured handler invocation. Returned by
// ReceivedRequests in chronological order. Body carries up to
// 64 KiB; longer payloads set Truncated.
type RequestRecord struct {
	OperationID string
	Method      string
	Path        string
	Status      int
	ReceivedAt  time.Time
	Body        []byte
	Truncated   bool
	PeerCN      string
}

// Start spins up an httptest-backed Mock bound to t's lifecycle. It
// registers t.Cleanup(m.Stop), so callers do not need to invoke Stop
// themselves. Misconfiguration (heartbeat interval set without a
// ControlPlaneURL, or vice versa) calls t.Fatalf.
func Start(t TestingT, opts Options) *Mock {
	t.Helper()

	if opts.Architecture == "" {
		opts.Architecture = "amd64"
	}
	if opts.NodeID == uuid.Nil {
		opts.NodeID = uuid.New()
	}
	if opts.NodeName == "" {
		opts.NodeName = "mock-node-1"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// HeartbeatInterval > 0 starts the periodic-push goroutine, which
	// needs a destination. ControlPlaneURL alone is fine: the test
	// drives the push synchronously via SendHeartbeatNow.
	if opts.HeartbeatInterval > 0 && opts.ControlPlaneURL == "" {
		t.Fatalf("agentmock.Start: HeartbeatInterval set but ControlPlaneURL is empty (no destination)")
	}

	st := newState(opts)
	m := &Mock{
		state:         st,
		logger:        opts.Logger,
		cpURL:         strings.TrimRight(opts.ControlPlaneURL, "/"),
		interval:      opts.HeartbeatInterval,
		nodeID:        opts.NodeID,
		nodeName:      opts.NodeName,
		architecture:  opts.Architecture,
		tlsMode:       opts.TLS,
		consoleTokens: console.NewTokenStore(),
	}

	router := chi.NewRouter()
	router.Use(captureBodyMiddleware)
	agentapi.HandlerFromMux(m, router)

	srv := httptest.NewUnstartedServer(router)
	if opts.TLS == TLSEnabled {
		cfg, err := serverTLSConfig()
		if err != nil {
			t.Fatalf("agentmock.Start: server TLS config: %v", err)
		}
		srv.TLS = cfg
		srv.StartTLS()
	} else {
		srv.Start()
	}
	m.httpServer = srv

	// Heartbeat HTTP client is built whenever a CP destination is
	// configured — both the periodic goroutine (when interval > 0)
	// and synchronous SendHeartbeatNow callers need it.
	if opts.ControlPlaneURL != "" {
		client, err := m.newHeartbeatClient()
		if err != nil {
			srv.Close()
			t.Fatalf("agentmock.Start: heartbeat client: %v", err)
		}
		m.heartbeatHTTP = client
	}
	if opts.HeartbeatInterval > 0 {
		ctx, stop := context.WithCancel(context.Background())
		m.heartbeatStop = stop
		m.heartbeatDone = make(chan struct{})
		go m.heartbeatLoop(ctx)
	}

	t.Cleanup(m.Stop)
	return m
}

// Stop halts the heartbeat goroutine and shuts the HTTP server down.
// Idempotent — repeat calls are no-ops. Registered automatically with
// t.Cleanup by Start.
func (m *Mock) Stop() {
	m.stopOnce.Do(func() {
		if m.heartbeatStop != nil {
			m.heartbeatStop()
			select {
			case <-m.heartbeatDone:
			case <-time.After(5 * time.Second):
				m.logger.Warn("agentmock: heartbeat goroutine did not exit within 5s; abandoning")
			}
		}
		if m.httpServer != nil {
			m.httpServer.Close()
		}
	})
}

// URL returns the agent's base URL — "https://..." when TLSEnabled,
// "http://..." otherwise.
func (m *Mock) URL() string {
	return m.httpServer.URL
}

// NodeID returns the UUID this mock identifies as.
func (m *Mock) NodeID() uuid.UUID {
	return m.nodeID
}

// ReceivedRequests returns a snapshot of every HTTP request the mock
// has handled, oldest first. The slice is safe to inspect without
// further locking.
func (m *Mock) ReceivedRequests() []RequestRecord {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	return m.state.snapshotRequestsLocked()
}

// TestingT is the subset of *testing.T the Mock relies on. Defining
// it as an interface keeps the package importable from non-test
// callers (e.g. a future stand-alone binary) without dragging in
// testing.T.
type TestingT interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

func newState(opts Options) *state {
	return &state{
		nodeID:       opts.NodeID,
		architecture: opts.Architecture,
		agentVersion: agentVersionString,
		startedAt:    time.Now().UTC(),
		hostname:     opts.NodeName,
		// Capability fields start zero-valued; tests preload what they
		// need through SetCapability / AddFirmware / AddStoragePool /
		// etc.
		qemuBinaries:       map[string]string{},
		migration:          defaultMigrationCapability,
		pools:              map[string]storagePool{},
		images:             map[string]map[string]CachedImage{},
		tasks:              map[uuid.UUID]*agentTask{},
		poolScanResults:    map[string][]PoolScanResult{},
		imageImportResults: map[imageImportKey][]ImageImportResult{},
		storedVMs:          map[string]AgentVM{},
		vmCreateResults:    map[string][]VMCreateResult{},
		vmDeleteResults:    map[string][]VMDeleteResult{},
	}
}
