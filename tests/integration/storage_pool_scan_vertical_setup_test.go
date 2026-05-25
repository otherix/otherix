// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/api/agentclient"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// verticalSlice bundles every component the vertical-slice e2e tests
// drive: a fresh Postgres, a real agentmock (functional scan +
// import handlers), the production scan / import executors and
// reconciler wired against a real *agentclient.Client, the river
// client, the CP HTTP router, and a pre-seeded admin token. Each
// test gets its own instance so state is fully isolated.
type verticalSlice struct {
	store       *store.Store
	mock        *agentmock.Mock
	cpServer    *httptest.Server
	riverClient *river.Client[pgx.Tx]
	agentClient *agentclient.Client
	logger      *slog.Logger

	// completions multiplexes EventKindJobCompleted and
	// EventKindJobFailed so callers see whichever terminal a single
	// attempt produced. The first event for Kind matching the
	// scenario is the one tests assert against.
	completions <-chan *river.Event
	cancelSub   func()

	pool       store.StoragePool
	node       store.Node
	adminID    uuid.UUID
	adminToken string

	// agentClientCfg is the live config the helper used so tests
	// that need to assert against polling parameters (e.g. the
	// timeout-budget test) can re-derive them.
	agentClientCfg config.AgentClientConfig
}

// fastAgentClientCfg returns the standard test configuration: 10s
// budget, 10ms initial poll, 100ms cap. Suitable for happy /
// failure / idempotency tests where the agent-side delay is in
// tens of milliseconds.
func fastAgentClientCfg() config.AgentClientConfig {
	return config.AgentClientConfig{
		Enabled:         true,
		Timeout:         10 * time.Second,
		PollInterval:    10 * time.Millisecond,
		PollMaxInterval: 100 * time.Millisecond,
	}
}

// newVerticalSlice spins up the full vertical slice with a discard
// logger. Callers that need to capture log output (e.g.
// reconciliation tests asserting on WARN entries) use
// newVerticalSliceWithLogger instead.
func newVerticalSlice(t *testing.T, ctx context.Context, agentClientCfg config.AgentClientConfig) *verticalSlice {
	t.Helper()
	return newVerticalSliceWithLogger(t, ctx, agentClientCfg, nil)
}

// newVerticalSliceWithLogger spins up the full vertical slice routing
// every component's logger through `log`. A nil log falls back to a
// discard logger, matching the existing default. The
// agentClientCfg parameter overrides the production polling defaults
// so timing-sensitive tests run in milliseconds rather than five
// minutes; CertFile / KeyFile / CAFile fields are populated by the
// helper from agentmock testdata, the caller need only set the
// timing fields plus Enabled=true.
func newVerticalSliceWithLogger(t *testing.T, ctx context.Context, agentClientCfg config.AgentClientConfig, log *slog.Logger) *verticalSlice {
	t.Helper()
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	h, err := migrationtest.Start(ctx)
	if err != nil {
		t.Fatalf("migrationtest.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})

	s := store.FromPool(h.Pool)
	t.Cleanup(s.Close)

	mock := agentmock.Start(t, agentmock.Options{
		Architecture: "amd64",
		TLS:          agentmock.TLSEnabled,
	})

	cert, clusterCA := wireCertMaterial(t)
	client, err := agentclient.New(agentClientCfg, cert, clusterCA)
	if err != nil {
		t.Fatalf("agentclient.New: %v", err)
	}
	executor := storagepoolshandlers.NewAgentScanExecutor(client)
	importExecutor := storagepoolshandlers.NewAgentImportExecutor(client)
	vmCreateExecutor := vmshandlers.NewAgentVMCreateExecutor(client)
	vmDeleteExecutor := vmshandlers.NewAgentVMDeleteExecutor(client)
	vmLifecycleExecutor := vmshandlers.NewAgentVMLifecycleExecutor(client)

	authSvc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	riverClient, err := api.BuildRiverClient(api.RiverDeps{
		Pool:                h.Pool,
		Cfg:                 config.WorkersConfig{Enabled: true, MaxWorkers: 4},
		Logger:              log,
		Store:               s,
		ScanExecutor:        executor,
		ImportExecutor:      importExecutor,
		VMCreateExecutor:    vmCreateExecutor,
		VMDeleteExecutor:    vmDeleteExecutor,
		VMLifecycleExecutor: vmLifecycleExecutor,
		AgentClient:         client,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}

	completions, cancelSub := riverClient.Subscribe(
		river.EventKindJobCompleted,
		river.EventKindJobFailed,
	)
	t.Cleanup(cancelSub)

	if err := riverClient.Start(ctx); err != nil {
		t.Fatalf("riverClient.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = riverClient.Stop(stopCtx)
	})

	router := api.NewRouter(api.RouterDeps{
		Store:          s,
		AuthService:    authSvc,
		RiverClient:    riverClient,
		ImageDeleter:   client,
		Logger:         log,
		RequestTimeout: 10 * time.Second,
		VMLifecycle:    vmshandlers.LifecycleDeps{AgentClient: client},
		VMConsole:      vmshandlers.ConsoleDeps{AgentClient: client, AccessMode: "proxy"},
	})
	cpServer := httptest.NewServer(router)
	t.Cleanup(cpServer.Close)

	adminID, adminEmail, adminPW := seedAdmin(t, ctx, s)
	node := seedNode(t, ctx, s, mock.URL())
	pool := seedPool(t, ctx, s, node.ID)

	// Register the same pool with the mock-agent so handlers gated on
	// pool registration (storageImages.list, storageImages.import,
	// AddImage) succeed. The storagePools.scan handler does not gate
	// on registration, but the reconciliation step that calls
	// storageImages.list at the tail of every scan makes the
	// registration mandatory.
	mock.AddStoragePool(agentmock.StoragePool{
		ID:   pool.ID,
		Name: pool.Name,
		Type: "local_dir",
		Path: pool.Path,
	})

	adminToken := loginAdmin(t, cpServer.URL, adminEmail, adminPW)

	return &verticalSlice{
		store:          s,
		mock:           mock,
		cpServer:       cpServer,
		riverClient:    riverClient,
		agentClient:    client,
		logger:         log,
		completions:    completions,
		cancelSub:      cancelSub,
		pool:           pool,
		node:           node,
		adminID:        adminID,
		adminToken:     adminToken,
		agentClientCfg: agentClientCfg,
	}
}

// wireCertMaterial writes the agentmock testdata cert chain to a
// fresh temp directory и loads it via agentclient.LoadMaterialFromFiles.
// The CA chain agentmock distributes is the same material the real
// agent presents (node.crt) и the real CP presents (controlplane.crt),
// so the helper stays stable while the per-replica CP cert lifecycle
// moved DB-side.
func wireCertMaterial(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	for _, item := range []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"ca.crt", agentmock.CACertPEM},
		{"tls.crt", agentmock.ControlPlaneCertPEM},
		{"tls.key", agentmock.ControlPlaneKeyPEM},
	} {
		bytes, err := item.fn()
		if err != nil {
			t.Fatalf("agentmock %s: %v", item.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, item.name), bytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", item.name, err)
		}
	}
	cert, ca, err := agentclient.LoadMaterialFromFiles(
		filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"),
		filepath.Join(dir, "ca.crt"),
	)
	if err != nil {
		t.Fatalf("LoadMaterialFromFiles: %v", err)
	}
	return cert, ca
}

func seedAdmin(t *testing.T, ctx context.Context, s *store.Store) (uuid.UUID, string, string) {
	t.Helper()
	id := uuid.New()
	email := fmt.Sprintf("vertical-admin-%s@example.test", uuid.NewString()[:8])
	password := "correct-horse-battery-staple"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: hash,
		DisplayName:  "vertical admin",
		Role:         "admin",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id, email, password
}

func seedNode(t *testing.T, ctx context.Context, s *store.Store, agentURL string) store.Node {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()

	// Wrap the CreateNode + last_heartbeat_at bump in a single
	// transaction so the heartbeat reconciler's RunOnStart fire never
	// observes the freshly-seeded row in the intermediate
	// `last_heartbeat_at IS NULL` state. Production reaches `status =
	// ready` only after register → heartbeat → PromoteHealthyNodes;
	// the vertical-slice tests shortcut to `status = ready` directly,
	// so we shortcut the timestamp too. The store's typed CreateNode
	// surface intentionally omits `last_heartbeat_at` (heartbeat-only
	// territory), so we issue a raw UPDATE through the same pgx.Tx
	// and rely on READ COMMITTED isolation in other backends to hide
	// the intermediate state from MarkNodesUnreachable.
	var node store.Node
	err := s.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		n, err := q.CreateNode(ctx, store.CreateNodeParams{
			ID:                      id,
			Name:                    "vertical-node-" + uuid.NewString()[:8],
			Architecture:            store.CpuArchAmd64,
			AdvertisedEndpoint:      agentURL,
			MigrationHost:           "10.0.0.10",
			MigrationPortRangeStart: 49152,
			MigrationPortRangeEnd:   49251,
			Status:                  store.NodeStatusReady,
		})
		if err != nil {
			return fmt.Errorf("CreateNode: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`update nodes set last_heartbeat_at = $1 where id = $2`, now, id,
		); err != nil {
			return fmt.Errorf("bump heartbeat: %w", err)
		}
		node = n
		return nil
	})
	if err != nil {
		t.Fatalf("seedNode: %v", err)
	}
	node.LastHeartbeatAt = &now
	return node
}

func seedPool(t *testing.T, ctx context.Context, s *store.Store, nodeID uuid.UUID) store.StoragePool {
	t.Helper()
	id := uuid.New()
	pool, err := s.Queries().CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID:     id,
		NodeID: nodeID,
		Name:   "vertical-pool-" + uuid.NewString()[:8],
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/vertical",
		Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	return pool
}

func loginAdmin(t *testing.T, cpURL, email, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	resp, err := http.Post(cpURL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return login.AccessToken
}

// scanRequest builds an authenticated POST /v1/storage-pools/{id}/scan
// request. Body and Idempotency-Key headers are caller-supplied — both
// are optional. Returns *http.Request so callers can inspect headers
// or extend before sending.
func (v *verticalSlice) scanRequest(t *testing.T, ctx context.Context, body []byte, idemKey string) *http.Request {
	t.Helper()
	if body == nil {
		body = []byte(`{}`)
	}
	url := v.cpServer.URL + "/v1/storage-pools/" + v.pool.ID.String() + "/scan"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new scan request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return req
}

// postScan sends a scan request and decodes the AsyncTaskAccepted body.
// Returns the response (already drained) so callers can inspect status
// and headers, plus the decoded task_id. Fails the test on non-202.
func (v *verticalSlice) postScan(t *testing.T, ctx context.Context, idemKey string) (acceptedBody []byte, taskID uuid.UUID) {
	t.Helper()
	req := v.scanRequest(t, ctx, []byte(`{}`), idemKey)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("scan POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scan body: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scan status = %d, body = %s, want 202", resp.StatusCode, body)
	}
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatalf("decode AsyncTaskAccepted: %v", err)
	}
	id, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("task_id %q is not a uuid: %v", accepted.TaskID, err)
	}
	return body, id
}

// awaitScanEvent drains the river completions channel until a
// storage_pool.scan event surfaces (or the deadline fires). Returns
// the event so callers can branch on its Kind. Other periodic
// workers (auth.refresh_token_cleanup, idempotency.cleanup) run with
// RunOnStart=false so are not expected to interfere, but the strict
// Kind filter keeps the test stable regardless.
func (v *verticalSlice) awaitScanEvent(t *testing.T, deadline time.Duration) *river.Event {
	t.Helper()
	timer := time.After(deadline)
	for {
		select {
		case ev := <-v.completions:
			if ev.Job.Kind == "storage_pool.scan" {
				return ev
			}
		case <-timer:
			t.Fatalf("storage_pool.scan event did not arrive within %v", deadline)
		}
	}
}

// agentScanCallCount returns how many times the mock-agent has
// observed POST /v1/storage-pools/{id}/scan, used by idempotency
// tests to assert the CP did not double-call the agent on retry.
func (v *verticalSlice) agentScanCallCount() int {
	count := 0
	for _, rec := range v.mock.ReceivedRequests() {
		if rec.OperationID == "storagePools.scan" {
			count++
		}
	}
	return count
}

// pollTaskUntil reads tasks rows directly from the database until
// pred returns true or the deadline fires. Returns the last-read
// task. Useful for cancel-running tests that need to wait for the
// worker to push the row into status='running' before cancelling.
func (v *verticalSlice) pollTaskUntil(t *testing.T, ctx context.Context, taskID uuid.UUID, pred func(store.Task) bool, deadline time.Duration) store.Task {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		row, err := v.store.Queries().GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if pred(row) {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not satisfy predicate within %v", taskID, deadline)
	return store.Task{}
}
