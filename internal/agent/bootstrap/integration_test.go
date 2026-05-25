// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package bootstrap_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/bootstrap"
	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

var sharedHarness *migrationtest.Harness

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := migrationtest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrationtest.Start: %v\n", err)
		os.Exit(1)
	}
	sharedHarness = h

	code := m.Run()

	stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	h.Stop(stopCtx)

	os.Exit(code)
}

// tlsHarness wraps a freshly-migrated store, the active cluster CA's
// fingerprint, and an httptest.NewTLSServer serving the full CP router.
// `httptest.NewTLSServer` uses a self-signed cert; bootstrap's
// InsecureSkipVerify posture lets that through and the chain pins
// everything via the operator-supplied fingerprint.
type tlsHarness struct {
	srv           *httptest.Server
	store         *store.Store
	caFingerprint string // operator-pinned, lowercase hex
}

func (h *tlsHarness) close() {
	h.srv.Close()
	h.store.Close()
}

func newTLSHarness(t *testing.T) *tlsHarness {
	t.Helper()
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN: sharedHarness.DSN, MaxConns: 4, MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}

	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		s.Close()
		t.Fatalf("auth.NewService: %v", err)
	}

	riverClient, err := api.BuildRiverClient(api.RiverDeps{
		Pool:   sharedHarness.Pool,
		Cfg:    config.WorkersConfig{Enabled: false, MaxWorkers: 1},
		Logger: discardLog(),
		Store:  s,
	})
	if err != nil {
		s.Close()
		t.Fatalf("BuildRiverClient: %v", err)
	}

	if err := api.BootstrapClusterCA(context.Background(), s, discardLog()); err != nil {
		s.Close()
		t.Fatalf("BootstrapClusterCA: %v", err)
	}

	caRow, err := s.Queries().GetActiveCACert(context.Background())
	if err != nil {
		s.Close()
		t.Fatalf("GetActiveCACert: %v", err)
	}
	fingerprint := hex.EncodeToString(caRow.FingerprintSha256)

	router := api.NewRouter(api.RouterDeps{
		Store:          s,
		AuthService:    svc,
		RiverClient:    riverClient,
		ImageDeleter:   nil,
		Logger:         discardLog(),
		RequestTimeout: 10 * time.Second,
	})
	srv := httptest.NewTLSServer(router)
	t.Cleanup(srv.Close)
	t.Cleanup(s.Close)

	return &tlsHarness{srv: srv, store: s, caFingerprint: fingerprint}
}

// mintOption tweaks a freshly-minted join token's params before insert.
type mintOption func(*store.CreateJoinTokenParams)

func withMaxUses(n int32) mintOption {
	return func(p *store.CreateJoinTokenParams) {
		p.MaxUses = &n
	}
}

func withNodeName(name string) mintOption {
	return func(p *store.CreateJoinTokenParams) {
		p.IntendedNodeName = &name
		// Pre-bound tokens must be single-use (CHECK constraint).
		one := int32(1)
		p.MaxUses = &one
	}
}

func withExpiry(when time.Time) mintOption {
	return func(p *store.CreateJoinTokenParams) {
		p.ExpiresAt = when
	}
}

// mintJoinToken creates a join token directly via SQL (bypassing the
// management endpoint to keep these tests focused on the bootstrap
// flow). Returns the plaintext token.
func mintJoinToken(t *testing.T, h *tlsHarness, opts ...mintOption) string {
	t.Helper()
	plaintext, hash, err := auth.GenerateJoinToken()
	if err != nil {
		t.Fatalf("GenerateJoinToken: %v", err)
	}
	params := store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	for _, o := range opts {
		o(&params)
	}
	if _, err := h.store.Queries().CreateJoinToken(context.Background(), params); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	return plaintext
}

func uniqueName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

func (h *tlsHarness) joinCfg(token, nodeName string) *config.BootstrapConfig {
	return &config.BootstrapConfig{
		Token:                   token,
		CAFingerprint:           "sha256:" + h.caFingerprint,
		CPURL:                   h.srv.URL,
		NodeName:                nodeName,
		Architecture:            "arm64",
		AdvertisedEndpoint:      "https://10.0.0.1:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		RequestTimeout:          15 * time.Second,
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBootstrap_HappyPath(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h)
	nodeName := uniqueName("happy")

	result, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, nodeName), discardLog())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := uuid.Parse(result.NodeID); err != nil {
		t.Errorf("Result.NodeID = %q, not a UUID: %v", result.NodeID, err)
	}
	if !strings.Contains(string(result.CertPEM), "BEGIN CERTIFICATE") {
		t.Error("CertPEM missing BEGIN CERTIFICATE")
	}
	if !strings.Contains(string(result.KeyPEM), "BEGIN PRIVATE KEY") {
		t.Error("KeyPEM missing BEGIN PRIVATE KEY")
	}
	if !strings.Contains(string(result.CACertPEM), "BEGIN CERTIFICATE") {
		t.Error("CACertPEM missing BEGIN CERTIFICATE")
	}
}

func TestBootstrap_BareHexFingerprintAccepted(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h)
	cfg := h.joinCfg(token, uniqueName("bare"))
	cfg.CAFingerprint = h.caFingerprint // no sha256: prefix

	if _, err := bootstrap.Bootstrap(context.Background(), cfg, discardLog()); err != nil {
		t.Errorf("Bootstrap with bare hex fingerprint: %v", err)
	}
}

func TestBootstrap_PersistsToDisk(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h)
	nodeName := uniqueName("persist")

	result, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, nodeName), discardLog())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")
	caPath := filepath.Join(dir, "ca.crt")
	if err := bootstrap.Persist(certPath, keyPath, caPath, result); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	type expect struct {
		path string
		mode os.FileMode
	}
	for _, f := range []expect{
		{keyPath, 0o600},
		{certPath, 0o644},
		{caPath, 0o644},
	} {
		info, err := os.Stat(f.path)
		if err != nil {
			t.Errorf("Stat %s: %v", f.path, err)
			continue
		}
		if info.Mode().Perm() != f.mode {
			t.Errorf("%s mode = %v, want %v", f.path, info.Mode().Perm(), f.mode)
		}
	}
	// node-id sidecar is no longer written (name-keyed identity).
	if _, err := os.Stat(filepath.Join(dir, "node-id")); !os.IsNotExist(err) {
		t.Errorf("node-id sidecar should not exist after bootstrap; stat err = %v", err)
	}
}

func TestBootstrap_FingerprintMismatch(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h)

	cfg := h.joinCfg(token, uniqueName("fp"))
	cfg.CAFingerprint = "sha256:" + strings.Repeat("0", 64)

	_, err := bootstrap.Bootstrap(context.Background(), cfg, discardLog())
	if err == nil {
		t.Fatal("Bootstrap with wrong fingerprint = nil err, want error")
	}
	var fpErr *bootstrap.FingerprintMismatchError
	if !errors.As(err, &fpErr) {
		t.Errorf("expected *FingerprintMismatchError, got %T: %v", err, err)
	}
}

func TestBootstrap_TokenInvalid(t *testing.T) {
	h := newTLSHarness(t)
	cfg := h.joinCfg("otx_join_nonexistent_value_aaaaaaaaaaaaaaaaaa", uniqueName("invalid"))

	_, err := bootstrap.Bootstrap(context.Background(), cfg, discardLog())
	if err == nil {
		t.Fatal("Bootstrap with invalid token = nil err, want error")
	}
	if !errors.Is(err, bootstrap.ErrCSRRejected) {
		t.Errorf("expected ErrCSRRejected, got %v", err)
	}
}

func TestBootstrap_TokenExpired(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h, withExpiry(time.Now().Add(-1*time.Minute)))

	_, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, uniqueName("expired")), discardLog())
	if err == nil {
		t.Fatal("Bootstrap with expired token = nil err, want error")
	}
	if !errors.Is(err, bootstrap.ErrCSRRejected) {
		t.Errorf("expected ErrCSRRejected, got %v", err)
	}
}

func TestBootstrap_PreboundMismatch(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h, withNodeName("expected-name-"+uuid.NewString()[:8]))

	cfg := h.joinCfg(token, "wrong-name-"+uuid.NewString()[:8])
	_, err := bootstrap.Bootstrap(context.Background(), cfg, discardLog())
	if err == nil {
		t.Fatal("Bootstrap with mismatched name = nil err, want error")
	}
	if !errors.Is(err, bootstrap.ErrCSRRejected) {
		t.Errorf("expected ErrCSRRejected, got %v", err)
	}
}

func TestBootstrap_MultiUseToken(t *testing.T) {
	h := newTLSHarness(t)
	const cap = 3
	token := mintJoinToken(t, h, withMaxUses(cap))

	for i := 0; i < cap; i++ {
		nodeName := uniqueName(fmt.Sprintf("multi-%d", i))
		result, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, nodeName), discardLog())
		if err != nil {
			t.Fatalf("attempt %d: Bootstrap = %v", i, err)
		}
		if result.NodeID == "" {
			t.Errorf("attempt %d: empty NodeID", i)
		}
	}

	if _, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, uniqueName("multi-over")), discardLog()); err == nil {
		t.Fatal("attempt 4 with max_uses=3 = nil err, want exhausted")
	} else if !errors.Is(err, bootstrap.ErrCSRRejected) {
		t.Errorf("expected ErrCSRRejected, got %v", err)
	}
}

func TestBootstrap_ResponseChainVerified(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h)
	nodeName := uniqueName("chain")

	result, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, nodeName), discardLog())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	caRow, err := h.store.Queries().GetActiveCACert(context.Background())
	if err != nil {
		t.Fatalf("GetActiveCACert: %v", err)
	}
	if strings.TrimSpace(string(caRow.CertPem)) != strings.TrimSpace(string(result.CACertPEM)) {
		t.Error("bootstrap returned CA does not match active DB CA")
	}
}

func TestBootstrap_ConcurrentMultiUse(t *testing.T) {
	h := newTLSHarness(t)
	const cap = 5
	const goroutines = 10

	token := mintJoinToken(t, h, withMaxUses(cap))

	var (
		successes int32
		wg        sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			nodeName := uniqueName(fmt.Sprintf("conc-%d", i))
			if _, err := bootstrap.Bootstrap(context.Background(), h.joinCfg(token, nodeName), discardLog()); err == nil {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()

	if int(successes) != cap {
		t.Errorf("successes = %d, want %d", successes, cap)
	}
}

func TestBootstrap_BadCPURL(t *testing.T) {
	h := newTLSHarness(t)
	token := mintJoinToken(t, h)

	cfg := h.joinCfg(token, uniqueName("badurl"))
	cfg.CPURL = "https://127.0.0.1:1" // refuses connection

	_, err := bootstrap.Bootstrap(context.Background(), cfg, discardLog())
	if err == nil {
		t.Fatal("Bootstrap with unreachable CP = nil err, want error")
	}
}
