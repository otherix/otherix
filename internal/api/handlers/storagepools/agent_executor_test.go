// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
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

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/config"
)

// fakeAgentClient is a hand-rolled stand-in for *agentclient.Client
// in unit tests. Per Go style we keep it test-local rather than
// promoting a public interface — production callers depend on
// *agentclient.Client directly.
type fakeAgentClient struct {
	postScanFn func(ctx context.Context, endpoint string, poolName string, idempotencyKey string) (uuid.UUID, error)
	pollTaskFn func(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error)

	mu                sync.Mutex
	postScanCalls     []postScanCall
	pollTaskCalls     []pollTaskCall
	postScanCallCount int
	pollTaskCallCount int
}

type postScanCall struct {
	Endpoint       string
	PoolName       string
	IdempotencyKey string
}

type pollTaskCall struct {
	Endpoint    string
	AgentTaskID uuid.UUID
}

func (f *fakeAgentClient) PostScan(ctx context.Context, endpoint string, poolName string, idempotencyKey string) (uuid.UUID, error) {
	f.mu.Lock()
	f.postScanCalls = append(f.postScanCalls, postScanCall{endpoint, poolName, idempotencyKey})
	f.postScanCallCount++
	f.mu.Unlock()
	return f.postScanFn(ctx, endpoint, poolName, idempotencyKey)
}

func (f *fakeAgentClient) PollTask(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error) {
	f.mu.Lock()
	f.pollTaskCalls = append(f.pollTaskCalls, pollTaskCall{endpoint, agentTaskID})
	f.pollTaskCallCount++
	f.mu.Unlock()
	return f.pollTaskFn(ctx, endpoint, agentTaskID)
}

// nowRFC3339 returns the current instant in the RFC 3339 nano form
// the agent emits in result.reported_at.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func successResult() map[string]any {
	return map[string]any{
		"capacity_bytes":  float64(1 << 40),
		"available_bytes": float64(1 << 39),
		"reported_at":     nowRFC3339(),
	}
}

func TestAgentScanExecutor_Success(t *testing.T) {
	t.Parallel()
	agentTaskID := uuid.New()

	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return agentTaskID, nil
		},
		pollTaskFn: func(_ context.Context, _ string, gotID uuid.UUID) (agentclient.TaskTerminal, error) {
			if gotID != agentTaskID {
				return agentclient.TaskTerminal{}, fmt.Errorf("unexpected agent task id %s", gotID)
			}
			return agentclient.TaskTerminal{Status: "success", Result: successResult()}, nil
		},
	}

	exec := NewAgentScanExecutor(fake)
	res, err := exec.Execute(context.Background(), ScanArgs{
		PoolID:             uuid.New(),
		NodeID:             uuid.New(),
		AdvertisedEndpoint: "https://agent.example.local:8443",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.CapacityBytes != 1<<40 {
		t.Errorf("CapacityBytes = %d, want %d", res.CapacityBytes, int64(1<<40))
	}
	if res.AvailableBytes != 1<<39 {
		t.Errorf("AvailableBytes = %d, want %d", res.AvailableBytes, int64(1<<39))
	}
	if res.ReportedAt.IsZero() {
		t.Errorf("ReportedAt is zero")
	}
	if res.ReportedAt.Location() != time.UTC {
		t.Errorf("ReportedAt.Location() = %v, want UTC", res.ReportedAt.Location())
	}
}

func TestAgentScanExecutor_PostScanFails(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.Nil, &agentclient.AgentError{Status: 409, Code: "conflict", Message: "scan in flight"}
		},
		pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			t.Fatal("PollTask called after PostScan failure")
			return agentclient.TaskTerminal{}, nil
		},
	}

	_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	if !strings.Contains(err.Error(), "post scan") {
		t.Errorf("error missing 'post scan' phase: %v", err)
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Errorf("errors.As to *AgentError(409): err = %v", err)
	}
}

func TestAgentScanExecutor_PollTaskAgentError(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{}, &agentclient.AgentError{Status: 502, Code: "internal", Message: "bad gateway"}
		},
	}

	_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	if !strings.Contains(err.Error(), "poll task") {
		t.Errorf("error missing 'poll task' phase: %v", err)
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != 502 {
		t.Errorf("errors.As to *AgentError(502): err = %v", err)
	}
}

func TestAgentScanExecutor_PollTaskTimeout(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, _ string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{}, &agentclient.TimeoutError{
				AgentTaskID: agentTaskID.String(),
				Budget:      "5m0s",
			}
		},
	}

	_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	var te *agentclient.TimeoutError
	if !errors.As(err, &te) {
		t.Errorf("errors.As to *TimeoutError: err = %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false, want true")
	}
}

func TestAgentScanExecutor_TerminalFailed(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{
				Status: "failed",
				Error:  &agentclient.AgentError{Code: "permission_denied", Message: "agent rejected scan"},
			}, nil
		},
	}

	_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error on terminal failed")
	}
	if !strings.Contains(err.Error(), "permission_denied") || !strings.Contains(err.Error(), "agent rejected scan") {
		t.Errorf("error missing agent code/message: %v", err)
	}
}

func TestAgentScanExecutor_TerminalCancelled(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{Status: "cancelled"}, nil
		},
	}

	_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error on terminal cancelled")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error missing 'cancelled' substring: %v", err)
	}
}

func TestAgentScanExecutor_UnexpectedTerminalStatus(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{Status: "bogus"}, nil
		},
	}

	_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error on unexpected status")
	}
	if !strings.Contains(err.Error(), "unexpected terminal status") {
		t.Errorf("error missing 'unexpected terminal status': %v", err)
	}
}

func TestAgentScanExecutor_MalformedSuccessResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		result map[string]any
		want   string
	}{
		{"nil_result", nil, "agent success result is nil"},
		{"missing_capacity", map[string]any{
			"available_bytes": float64(1),
			"reported_at":     nowRFC3339(),
		}, `missing "capacity_bytes"`},
		{"missing_available", map[string]any{
			"capacity_bytes": float64(1),
			"reported_at":    nowRFC3339(),
		}, `missing "available_bytes"`},
		{"missing_reported_at", map[string]any{
			"capacity_bytes":  float64(1),
			"available_bytes": float64(1),
		}, `missing "reported_at"`},
		{"capacity_wrong_type", map[string]any{
			"capacity_bytes":  "lots",
			"available_bytes": float64(1),
			"reported_at":     nowRFC3339(),
		}, `unexpected type`},
		{"reported_at_invalid", map[string]any{
			"capacity_bytes":  float64(1),
			"available_bytes": float64(1),
			"reported_at":     "not a date",
		}, `"reported_at"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAgentClient{
				postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
					return uuid.New(), nil
				},
				pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
					return agentclient.TaskTerminal{Status: "success", Result: tc.result}, nil
				},
			}
			_, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"})
			if err == nil {
				t.Fatalf("Execute returned nil error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing substring %q", err, tc.want)
			}
		})
	}
}

func TestAgentScanExecutor_FreshIdempotencyKeyPerAttempt(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{Status: "success", Result: successResult()}, nil
		},
	}

	exec := NewAgentScanExecutor(fake)
	for range 3 {
		if _, err := exec.Execute(context.Background(), ScanArgs{AdvertisedEndpoint: "x"}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}

	if got := len(fake.postScanCalls); got != 3 {
		t.Fatalf("postScan calls = %d, want 3", got)
	}
	seen := map[string]int{}
	for _, c := range fake.postScanCalls {
		if c.IdempotencyKey == "" {
			t.Errorf("PostScan called with empty Idempotency-Key")
		}
		seen[c.IdempotencyKey]++
	}
	if len(seen) != 3 {
		t.Errorf("distinct idempotency keys = %d, want 3 (got %v)", len(seen), seen)
	}
}

func TestAgentScanExecutor_ContextCancelled(t *testing.T) {
	t.Parallel()
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, _ string, _ string, _ string) (uuid.UUID, error) {
			return uuid.New(), nil
		},
		pollTaskFn: func(ctx context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			return agentclient.TaskTerminal{}, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewAgentScanExecutor(fake).Execute(ctx, ScanArgs{AdvertisedEndpoint: "x"})
	if err == nil {
		t.Fatal("Execute returned nil error on cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true (err = %v)", err)
	}
}

func TestAgentScanExecutor_AdvertisedEndpointPropagated(t *testing.T) {
	t.Parallel()
	const wantEndpoint = "https://agent-12.example.local:8443"
	fake := &fakeAgentClient{
		postScanFn: func(_ context.Context, gotEndpoint string, _ string, _ string) (uuid.UUID, error) {
			if gotEndpoint != wantEndpoint {
				return uuid.Nil, fmt.Errorf("PostScan endpoint = %q, want %q", gotEndpoint, wantEndpoint)
			}
			return uuid.New(), nil
		},
		pollTaskFn: func(_ context.Context, gotEndpoint string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
			if gotEndpoint != wantEndpoint {
				return agentclient.TaskTerminal{}, fmt.Errorf("PollTask endpoint = %q, want %q", gotEndpoint, wantEndpoint)
			}
			return agentclient.TaskTerminal{Status: "success", Result: successResult()}, nil
		},
	}

	if _, err := NewAgentScanExecutor(fake).Execute(context.Background(), ScanArgs{AdvertisedEndpoint: wantEndpoint}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestAgentScanExecutor_RealClientHappyPath is the higher-fidelity
// test: real *agentclient.Client (mTLS handshake, real polling) talks
// to an httptest.NewUnstartedServer that runs both /scan and
// /tasks/{id} handlers, asserting the executor wires correctly when
// the client behind it is the production type rather than a fake.
//
// The CP-side cert/key/CA come from the agentmock testdata fixture;
// the server presents node.crt and verifies the CP client cert via
// agentmock.AgentServerTLSConfig().
func TestAgentScanExecutor_RealClientHappyPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, w := range []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"ca.crt", agentmock.CACertPEM},
		{"tls.crt", agentmock.ControlPlaneCertPEM},
		{"tls.key", agentmock.ControlPlaneKeyPEM},
	} {
		bytes, err := w.fn()
		if err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, w.name), bytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.name, err)
		}
	}

	poolID := uuid.New()
	poolName := "test-pool"
	agentTaskID := uuid.New()
	var pollCount atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolName+"/scan",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id": agentTaskID,
				"status":  "pending",
				"links":   map[string]any{"self": "/v1/tasks/" + agentTaskID.String()},
			})
		})
	mux.HandleFunc("/v1/tasks/"+agentTaskID.String(),
		func(w http.ResponseWriter, _ *http.Request) {
			n := pollCount.Add(1)
			body := map[string]any{
				"id":            agentTaskID,
				"type":          "storage_pool.scan",
				"resource_type": "storage_pool",
				"attempts":      1,
				"max_attempts":  25,
				"created_at":    nowRFC3339(),
			}
			if n < 2 {
				body["status"] = "running"
			} else {
				body["status"] = "success"
				body["result"] = successResult()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(body)
		})

	tlsCfg, err := agentmock.AgentServerTLSConfig()
	if err != nil {
		t.Fatalf("AgentServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	cert, ca, err := agentclient.LoadMaterialFromFiles(
		filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"),
		filepath.Join(dir, "ca.crt"),
	)
	if err != nil {
		t.Fatalf("LoadMaterialFromFiles: %v", err)
	}
	cli, err := agentclient.New(config.AgentClientConfig{
		Enabled:         true,
		Timeout:         3 * time.Second,
		PollInterval:    10 * time.Millisecond,
		PollMaxInterval: 100 * time.Millisecond,
	}, cert, ca)
	if err != nil {
		t.Fatalf("agentclient.New: %v", err)
	}

	exec := NewAgentScanExecutor(cli)
	res, err := exec.Execute(context.Background(), ScanArgs{
		PoolID:             poolID,
		PoolName:           poolName,
		NodeID:             uuid.New(),
		AdvertisedEndpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.CapacityBytes != 1<<40 || res.AvailableBytes != 1<<39 {
		t.Errorf("ScanResult = %+v, want canned 1TiB/512GiB", res)
	}
	if pollCount.Load() < 2 {
		t.Errorf("pollCount = %d, want >= 2 (running → success transition)", pollCount.Load())
	}
}
