// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
)

// agentImportClient is the narrow interface agentImportExecutor
// depends on (Go-style "consumer defines"). *agentclient.Client
// satisfies it structurally; tests pass a fake without importing
// the production client at all.
type agentImportClient interface {
	PostImageImport(
		ctx context.Context,
		endpoint string,
		poolName string,
		idempotencyKey string,
		req agentapi.ImageImportRequest,
	) (uuid.UUID, error)
	PollTask(
		ctx context.Context,
		endpoint string,
		agentTaskID uuid.UUID,
	) (agentclient.TaskTerminal, error)
}

// agentImportExecutor is the production ImportExecutor that
// translates an ImportArgs into the two-step
// PostImageImport + PollTask saga against an agent over mTLS.
//
// Errors flow through verbatim (wrapped with phase context) so the
// upstream worker's classifyImportError can derive the right
// tasks.error.code. River retry classification is left to the
// queue's own MaxAttempts/backoff and is intentionally NOT
// pre-empted at the executor level.
//
// Resumption: when args.AgentTaskID is non-nil, the executor
// skips the POST and goes straight to PollTask - a CP-side
// restart mid-poll picks back up against the same agent_task_id
// without issuing a duplicate import. On the first run
// (AgentTaskID nil), the executor invokes args.OnAgentTaskID
// immediately after the agent's 202 returns so the persisted id is
// available for the next attempt.
type agentImportExecutor struct {
	client agentImportClient
}

// NewAgentImportExecutor returns the production ImportExecutor
// that drives storage_image.import against an agent.
func NewAgentImportExecutor(client agentImportClient) ImportExecutor {
	return &agentImportExecutor{client: client}
}

// Execute implements ImportExecutor.
func (e *agentImportExecutor) Execute(ctx context.Context, args ImportArgs) (ImportResult, error) {
	agentTaskID, err := e.postOrResume(ctx, args)
	if err != nil {
		return ImportResult{}, err
	}
	terminal, err := e.client.PollTask(ctx, args.Node.AdvertisedEndpoint, agentTaskID)
	if err != nil {
		return ImportResult{}, fmt.Errorf("poll task: %w", err)
	}
	switch terminal.Status {
	case "success":
		return decodeImportResult(terminal.Result)
	case "failed", "cancelled":
		if terminal.Error != nil {
			return ImportResult{}, terminal.Error
		}
		return ImportResult{}, fmt.Errorf("agent task %s without error envelope", terminal.Status)
	default:
		return ImportResult{}, fmt.Errorf("unexpected agent terminal status %q", terminal.Status)
	}
}

// postOrResume returns the agent task id to poll. On a fresh
// attempt (args.AgentTaskID nil) it issues the agent POST and
// invokes OnAgentTaskID with the returned id; on a resumption it
// echoes the supplied id back without contacting the agent.
func (e *agentImportExecutor) postOrResume(ctx context.Context, args ImportArgs) (uuid.UUID, error) {
	if args.AgentTaskID != nil {
		return *args.AgentTaskID, nil
	}
	idemKey := uuid.NewString()
	url := args.Template.ImageUrl
	body := agentapi.ImageImportRequest{
		SourceUrl: &url,
		Format:    agentapi.ImageImportRequestFormat(args.Template.ImageFormat),
	}
	// nil bytes → compute mode (agent computes during
	// streaming download). Non-nil bytes → verify mode (agent compares
	// against this value, fails с `checksum_mismatch` on disagreement).
	// agentapi.ImageImportRequest.ExpectedChecksumSha256 is *string
	// (omitempty), so leaving it nil produces а wire body without
	// the field — agent treats it as compute mode.
	if len(args.Template.ImageChecksumSha256) > 0 {
		hexChecksum := hex.EncodeToString(args.Template.ImageChecksumSha256)
		body.ExpectedChecksumSha256 = &hexChecksum
	}
	agentTaskID, err := e.client.PostImageImport(ctx, args.Node.AdvertisedEndpoint, args.Pool.Name, idemKey, body)
	if err != nil {
		return uuid.Nil, fmt.Errorf("post import: %w", err)
	}
	if args.OnAgentTaskID == nil {
		return uuid.Nil, errors.New("ImportArgs.OnAgentTaskID is nil; cannot persist agent_task_id")
	}
	if err := args.OnAgentTaskID(ctx, agentTaskID); err != nil {
		return uuid.Nil, fmt.Errorf("persist agent_task_id: %v", err)
	}
	return agentTaskID, nil
}

// decodeImportResult lifts the agent's free-form result map into
// the typed ImportResult the worker projects into storage_images.
// JSON numbers come through as float64; pickInt64 (shared with the
// scan executor) narrows safely.
func decodeImportResult(result map[string]any) (ImportResult, error) {
	if result == nil {
		return ImportResult{}, errors.New("agent success result is nil")
	}
	sha, ok := result["checksum_sha256"].(string)
	if !ok || sha == "" {
		return ImportResult{}, errors.New("agent result missing checksum_sha256")
	}
	size, err := pickInt64(result, "size_bytes")
	if err != nil {
		return ImportResult{}, err
	}
	format, ok := result["format"].(string)
	if !ok || format == "" {
		return ImportResult{}, errors.New("agent result missing format")
	}
	return ImportResult{
		ChecksumSHA256: sha,
		SizeBytes:      size,
		Format:         format,
	}, nil
}
