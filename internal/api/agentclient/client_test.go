// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/config"
)

// fastConfig returns an enabled AgentClientConfig with compressed
// polling intervals so backoff / timeout tests finish in tens of
// milliseconds. dir is unused after the inter-step refactor —
// mTLS material now flows through agentclient.New's
// cert + clusterCA params (loaded via loadMaterial helper).
//
// PollMaxInterval doubles as the per-request httpClient.Timeout (see
// agentclient.New). 400ms preserves the "fast tests" intent while
// giving the race-detector + parallel-test scheduler enough headroom
// for TLS handshake + roundtrip variance under concurrent load (lint +
// api-validate + test running together). Production callers pass real
// PollMaxInterval values from config.
func fastConfig(t *testing.T, _ string) config.AgentClientConfig {
	t.Helper()
	return config.AgentClientConfig{
		Enabled:         true,
		Timeout:         500 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		PollMaxInterval: 400 * time.Millisecond,
	}
}

// loadMaterial reads the tls.crt / tls.key / ca.crt files writeMaterial
// dropped in dir and parses them via agentclient.LoadMaterialFromFiles.
// Tests use this together with fastConfig to build the (cfg, cert, ca)
// triple agentclient.New expects after the cp_cert lifecycle refactor.
func loadMaterial(t *testing.T, dir string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
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

// newClient is a one-stop wrapper that pairs fastConfig with loadMaterial
// and invokes agentclient.New. Used by the bulk of agentclient tests
// that don't need to vary individual config / material pieces.
func newClient(t *testing.T, dir string) *agentclient.Client {
	t.Helper()
	cert, ca := loadMaterial(t, dir)
	cli, err := agentclient.New(fastConfig(t, dir), cert, ca)
	if err != nil {
		t.Fatalf("agentclient.New: %v", err)
	}
	return cli
}

// writeMaterial dumps the cluster CA + the CP-side leaf cert / key
// from agentmock testdata into a temp directory and returns its
// path. Tests pass the path to fastConfig to seed AgentClientConfig.
func writeMaterial(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	caPEM, err := agentmock.CACertPEM()
	if err != nil {
		t.Fatalf("agentmock.CACertPEM(): %v", err)
	}
	cpCertPEM, err := agentmock.ControlPlaneCertPEM()
	if err != nil {
		t.Fatalf("agentmock.ControlPlaneCertPEM(): %v", err)
	}
	cpKeyPEM, err := agentmock.ControlPlaneKeyPEM()
	if err != nil {
		t.Fatalf("agentmock.ControlPlaneKeyPEM(): %v", err)
	}

	for _, w := range []struct {
		name  string
		bytes []byte
	}{
		{"ca.crt", caPEM},
		{"tls.crt", cpCertPEM},
		{"tls.key", cpKeyPEM},
	} {
		if err := os.WriteFile(filepath.Join(dir, w.name), w.bytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.name, err)
		}
	}
	return dir
}

// startAgentTLSServer wraps mux in an httptest TLS server that
// presents the agentmock node cert and verifies client certs against
// the cluster CA. Returns the server URL so tests don't have to
// re-derive .URL on the agent side.
func startAgentTLSServer(t *testing.T, mux http.Handler) (*httptest.Server, string) {
	t.Helper()
	tlsCfg, err := agentmock.AgentServerTLSConfig()
	if err != nil {
		t.Fatalf("AgentServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

// jsonWrite serialises v as JSON, writes status, body. testHelper-style.
func jsonWrite(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestNew_DisabledRejected(t *testing.T) {
	t.Parallel()
	_, err := agentclient.New(config.AgentClientConfig{Enabled: false}, tls.Certificate{}, nil)
	if err == nil {
		t.Fatal("agentclient.New(disabled) returned nil error")
	}
}

func TestLoadMaterialFromFiles_RejectsBadCAFile(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	// Overwrite ca.crt with non-PEM garbage.
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("overwrite ca.crt: %v", err)
	}
	_, _, err := agentclient.LoadMaterialFromFiles(
		filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"),
		filepath.Join(dir, "ca.crt"),
	)
	if err == nil {
		t.Fatal("LoadMaterialFromFiles with non-PEM CA returned nil error")
	}
}

func TestLoadMaterialFromFiles_RejectsMissingCertFile(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	if err := os.Remove(filepath.Join(dir, "tls.crt")); err != nil {
		t.Fatalf("remove tls.crt: %v", err)
	}
	_, _, err := agentclient.LoadMaterialFromFiles(
		filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"),
		filepath.Join(dir, "ca.crt"),
	)
	if err == nil {
		t.Fatal("LoadMaterialFromFiles with missing cert file returned nil error")
	}
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	cfg := fastConfig(t, dir)
	cfg.PollMaxInterval = cfg.PollInterval / 2
	cert, ca := loadMaterial(t, dir)
	_, err := agentclient.New(cfg, cert, ca)
	if err == nil {
		t.Fatal("New with PollMaxInterval < PollInterval returned nil error")
	}
}

func TestNew_RejectsEmptyMaterial(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	cfg := fastConfig(t, dir)
	if _, err := agentclient.New(cfg, tls.Certificate{}, nil); err == nil {
		t.Fatal("New with empty cert returned nil error")
	}
}

func TestPostScan_HappyPath(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	wantTaskID := uuid.New()
	poolID := "test-pool"
	idemKey := "idem-" + uuid.NewString()

	var seenIdemKey atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		seenIdemKey.Store(r.Header.Get("Idempotency-Key"))
		jsonWrite(t, w, http.StatusAccepted, map[string]any{
			"task_id": wantTaskID,
			"status":  "pending",
			"links":   map[string]any{"self": fmt.Sprintf("/v1/tasks/%s", wantTaskID)},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	gotID, err := cli.PostScan(context.Background(), agentURL, poolID, idemKey)
	if err != nil {
		t.Fatalf("PostScan: %v", err)
	}
	if gotID != wantTaskID {
		t.Errorf("PostScan task_id = %s, want %s", gotID, wantTaskID)
	}
	if got := seenIdemKey.Load().(string); got != idemKey {
		t.Errorf("Idempotency-Key on agent = %q, want %q", got, idemKey)
	}
}

func TestPostScan_AgentError(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	poolID := "test-pool"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/scan", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "conflict",
				"message": "scan already running",
				"details": map[string]any{"existing_task_id": "abc"},
			},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	_, err := cli.PostScan(context.Background(), agentURL, poolID, "k")
	if err == nil {
		t.Fatal("PostScan returned nil error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("PostScan err = %v (%T), want *AgentError", err, err)
	}
	if ae.Status != http.StatusConflict {
		t.Errorf("AgentError.Status = %d, want %d", ae.Status, http.StatusConflict)
	}
	if ae.Code != "conflict" {
		t.Errorf("AgentError.Code = %q, want %q", ae.Code, "conflict")
	}
	if got, want := ae.Details["existing_task_id"], "abc"; got != want {
		t.Errorf("AgentError.Details[existing_task_id] = %v, want %q", got, want)
	}
}

func TestPostScan_TLSHandshakeFails(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	// Stand up a TLS server with a self-signed cert NOT in the CP's
	// trust root. Handshake from the CP must fail before any HTTP
	// turn-around.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	cli := newClient(t, dir)

	_, err := cli.PostScan(context.Background(), srv.URL, "test-pool", "k")
	if err == nil {
		t.Fatal("PostScan against untrusted server returned nil error")
	}
	// Stdlib renders this as "tls:" or "x509:" depending on negotiation
	// order; assert one of the two so the test stays robust.
	msg := err.Error()
	if !containsAny(msg, "tls", "x509", "certificate") {
		t.Errorf("PostScan err = %q, want TLS/x509 substring", msg)
	}
}

func TestPollTask_TerminalSuccess(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "success", map[string]any{
			"capacity_bytes":  int64(1 << 40),
			"available_bytes": int64(1 << 39),
		}, nil))
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	got, err := cli.PollTask(context.Background(), agentURL, taskID)
	if err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if got.Status != "success" {
		t.Errorf("Status = %q, want success", got.Status)
	}
	if got.Error != nil {
		t.Errorf("Error = %+v, want nil on success", got.Error)
	}
	if v, ok := got.Result["capacity_bytes"].(float64); !ok || int64(v) != 1<<40 {
		t.Errorf("Result.capacity_bytes = %v, want %d", got.Result["capacity_bytes"], int64(1<<40))
	}
}

func TestPollTask_TerminalFailed(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "failed", nil, map[string]any{
			"code":    "internal",
			"message": "scan crashed",
		}))
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	got, err := cli.PollTask(context.Background(), agentURL, taskID)
	if err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.Error == nil {
		t.Fatal("Error = nil, want populated")
	}
	if got.Error.Code != "internal" || got.Error.Message != "scan crashed" {
		t.Errorf("Error = %+v, want code=internal message='scan crashed'", got.Error)
	}
}

func TestPollTask_BackoffProgression(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	var (
		mu     sync.Mutex
		stamps []time.Time
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		count := len(stamps)
		mu.Unlock()

		// First three responses keep the loop going; fourth completes.
		if count < 4 {
			jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "running", nil, nil))
			return
		}
		jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "success", map[string]any{}, nil))
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cfg := fastConfig(t, dir)
	cfg.Timeout = 5 * time.Second

	cert, ca := loadMaterial(t, dir)
	cli, err := agentclient.New(cfg, cert, ca)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := cli.PollTask(context.Background(), agentURL, taskID); err != nil {
		t.Fatalf("PollTask: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stamps) < 4 {
		t.Fatalf("got %d polls, want >=4", len(stamps))
	}
	// Gap N is between poll N and poll N+1. Expected progression:
	// gap1 ≈ PollInterval (10ms), gap2 ≈ 20ms, gap3 ≈ 40ms (well under
	// PollMaxInterval=400ms). Assertions are deliberately loose: under
	// -race + parallel-test scheduler, per-roundtrip overhead jitter
	// (20-100ms) can dwarf the intended sleep step (10-20ms), so strict
	// monotonicity fails at tied gaps. We assert direction-of-growth
	// only ("non-decreasing"), not magnitude.
	gap := func(i int) time.Duration { return stamps[i+1].Sub(stamps[i]) }

	g1, g2, g3 := gap(0), gap(1), gap(2)
	t.Logf("backoff gaps: %v %v %v", g1, g2, g3)

	// Lower-bound sanity: each gap is at least the configured initial
	// interval (10ms) — minus 2ms scheduling slack.
	if g1 < 8*time.Millisecond {
		t.Errorf("gap1 = %v, want >= 8ms (~PollInterval)", g1)
	}
	// Growth direction: g2 >= g1 (non-strict — ms ties on loaded CI are
	// not regressions), g3 >= g2/2 (cap clamping and overhead jitter can
	// pull g3 below g2; halving threshold keeps "backoff didn't collapse"
	// signal without being wall-clock-strict).
	if g2 < g1 {
		t.Errorf("gap2 (%v) < gap1 (%v) — backoff regressed", g2, g1)
	}
	if g3 < g2/2 {
		t.Errorf("gap3 (%v) collapsed below half of gap2 (%v)", g3, g2)
	}
}

func TestPollTask_TimeoutExceeded(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "running", nil, nil))
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cfg := fastConfig(t, dir)
	cfg.Timeout = 100 * time.Millisecond

	cert, ca := loadMaterial(t, dir)
	cli, err := agentclient.New(cfg, cert, ca)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = cli.PollTask(context.Background(), agentURL, taskID)
	if err == nil {
		t.Fatal("PollTask returned nil error on timeout")
	}
	var te *agentclient.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("PollTask err = %v (%T), want *TimeoutError", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false, want true")
	}
}

func TestPollTask_RetryOn5xx(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			jsonWrite(t, w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"code": "internal", "message": "temporary"},
			})
			return
		}
		jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "success", map[string]any{}, nil))
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	got, err := cli.PollTask(context.Background(), agentURL, taskID)
	if err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if got.Status != "success" {
		t.Errorf("Status = %q, want success after retry", got.Status)
	}
	if calls.Load() < 2 {
		t.Errorf("calls = %d, want >= 2 (5xx + success)", calls.Load())
	}
}

func TestPollTask_4xxNoRetry(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		jsonWrite(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "no such task"},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	_, err := cli.PollTask(context.Background(), agentURL, taskID)
	if err == nil {
		t.Fatal("PollTask returned nil error on 404")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("PollTask err = %v (%T), want *AgentError", err, err)
	}
	if ae.Status != http.StatusNotFound {
		t.Errorf("AgentError.Status = %d, want 404", ae.Status)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (4xx must not retry)", got)
	}
}

func TestPollTask_ContextCancel(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	taskID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks/"+taskID.String(), func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusOK, agentTaskBody(taskID, "running", nil, nil))
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cfg := fastConfig(t, dir)
	cfg.Timeout = 10 * time.Second

	cert, ca := loadMaterial(t, dir)
	cli, err := agentclient.New(cfg, cert, ca)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = cli.PollTask(ctx, agentURL, taskID)
	if err == nil {
		t.Fatal("PollTask returned nil error on parent ctx cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true (err = %v)", err)
	}
	// Caller cancellation must NOT surface as TimeoutError — that
	// shape is reserved for the loop's own deadline.
	var te *agentclient.TimeoutError
	if errors.As(err, &te) {
		t.Errorf("PollTask returned *TimeoutError on caller ctx cancel; want bare ctx.Err()")
	}
}

func TestConfig_DefaultsRoundtrip(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	cfg := fastConfig(t, dir)
	cert, ca := loadMaterial(t, dir)
	cli, err := agentclient.New(cfg, cert, ca)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if diff := cmp.Diff(cfg, cli.Config()); diff != "" {
		t.Errorf("Client.Config() roundtrip mismatch (-want +got):\n%s", diff)
	}
}

// agentTaskBody composes the minimum-viable Task projection the
// agentapi codegen accepts: required fields populated, optional
// fields wired when non-nil.
func agentTaskBody(id uuid.UUID, status string, result, errEnv map[string]any) map[string]any {
	body := map[string]any{
		"id":            id,
		"type":          "storage_pool.scan",
		"status":        status,
		"resource_type": "storage_pool",
		"attempts":      1,
		"max_attempts":  25,
		"created_at":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if result != nil {
		body["result"] = result
	}
	if errEnv != nil {
		body["error"] = errEnv
	}
	return body
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
