// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
)

// agentClient is the narrow interface agentScanExecutor depends on
// (Go-style "consumer defines"). *agentclient.Client satisfies it
// structurally; tests pass a fake without importing the production
// client at all.
type agentClient interface {
	PostScan(ctx context.Context, endpoint string, poolName string, idempotencyKey string) (uuid.UUID, error)
	PollTask(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error)
}

// agentScanExecutor is the production ScanExecutor that
// translates a ScanArgs into the two-step PostScan + PollTask
// saga against an agent over mTLS. It is the executor wired by
// cmd/api/main.go when both Workers.Enabled and AgentClient.Enabled
// hold; tests pass an in-package fake (`fakeScanExecutor`) directly.
//
// Errors flow through verbatim (wrapped with phase context) so the
// upstream worker's fail() path can render the originating layer in
// the task error envelope. Retry classification is left to the
// queue's own MaxAttempts/backoff and is intentionally NOT pre-
// empted at the executor level.
type agentScanExecutor struct {
	client agentClient
}

// NewAgentScanExecutor returns the production ScanExecutor that
// drives storage_pool.scan against an agent. The constructor takes
// only the HTTP client: ScanArgs.AdvertisedEndpoint is resolved upstream
// in StoragePoolScanWorker.Work, so the executor does not need a
// node reader.
func NewAgentScanExecutor(client agentClient) ScanExecutor {
	return &agentScanExecutor{client: client}
}

// Execute drives one scan: POST /v1/storage-pools/{id}/scan against
// args.AdvertisedEndpoint, then poll the returned agent_task_id until a
// terminal state. A terminal `success` projects the agent's `result`
// map into ScanResult; `failed`/`cancelled` surface as errors that
// the worker writes into tasks.error.
//
// idempotencyKey is generated fresh per Execute call: the agent's
// idempotency scope is independent of the CP-side idempotency
// scope, and reusing a key across worker retries would let the agent
// replay a stale terminal result instead of running a fresh scan.
func (e *agentScanExecutor) Execute(ctx context.Context, args ScanArgs) (ScanResult, error) {
	idempotencyKey := uuid.NewString()

	agentTaskID, err := e.client.PostScan(ctx, args.AdvertisedEndpoint, args.PoolName, idempotencyKey)
	if err != nil {
		return ScanResult{}, fmt.Errorf("post scan: %w", err)
	}

	terminal, err := e.client.PollTask(ctx, args.AdvertisedEndpoint, agentTaskID)
	if err != nil {
		return ScanResult{}, fmt.Errorf("poll task: %w", err)
	}

	switch terminal.Status {
	case "success":
		return decodeScanResult(terminal.Result)
	case "failed", "cancelled":
		return ScanResult{}, fmt.Errorf("agent task %s: %s", terminal.Status, formatAgentError(terminal.Error))
	default:
		return ScanResult{}, fmt.Errorf("agent returned unexpected terminal status %q", terminal.Status)
	}
}

// decodeScanResult lifts the agent's free-form result map into the
// typed ScanResult the worker projects into storage_pools. JSON
// numbers come through as float64 unless the agent emits a different
// shape; we accept both float64 and the (rare) int64 fallback so the
// executor stays resilient to encoder differences.
func decodeScanResult(result map[string]any) (ScanResult, error) {
	if result == nil {
		return ScanResult{}, errors.New("agent success result is nil")
	}

	capacity, err := pickInt64(result, "capacity_bytes")
	if err != nil {
		return ScanResult{}, err
	}
	available, err := pickInt64(result, "available_bytes")
	if err != nil {
		return ScanResult{}, err
	}
	reportedAt, err := pickRFC3339(result, "reported_at")
	if err != nil {
		return ScanResult{}, err
	}

	return ScanResult{
		CapacityBytes:  capacity,
		AvailableBytes: available,
		ReportedAt:     reportedAt,
	}, nil
}

// pickInt64 reads key from m and coerces it to int64. JSON numbers
// arrive as float64 from encoding/json; we narrow with a range check
// so a value silently truncated past int64 surfaces as an error
// rather than wrapping.
func pickInt64(m map[string]any, key string) (int64, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return 0, fmt.Errorf("agent result missing %q", key)
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("agent result %q has unexpected type %T", key, raw)
	}
}

// pickRFC3339 reads key from m as an RFC 3339 timestamp.
func pickRFC3339(m map[string]any, key string) (time.Time, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return time.Time{}, fmt.Errorf("agent result missing %q", key)
	}
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("agent result %q has unexpected type %T", key, raw)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("agent result %q: %v", key, err)
	}
	return t.UTC(), nil
}

// formatAgentError renders an *agentclient.AgentError envelope into
// a single line suitable for embedding in the worker's error
// message. Nil envelope (agent omitted the field) yields "<no
// error>" so the wrapping fmt.Errorf still produces a parseable
// message.
func formatAgentError(ae *agentclient.AgentError) string {
	if ae == nil {
		return "<no error envelope>"
	}
	if ae.Code == "" && ae.Message == "" {
		return "<empty error envelope>"
	}
	if ae.Code == "" {
		return ae.Message
	}
	if ae.Message == "" {
		return ae.Code
	}
	return fmt.Sprintf("%s: %s", ae.Code, ae.Message)
}
