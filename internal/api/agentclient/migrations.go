// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// StartIncomingMigration prepares the **target** agent to receive a
// migration of vmName. It is the first call in the two-phase
// peer-to-peer handshake: the CP allocates the migration_id, posts it
// here, and the target picks a free ingress port plus mints a
// single-use auth token. 200 returns the MigrationIncomingResponse the
// CP then threads into StartOutgoingMigration on the source agent;
// non-2xx (e.g. 409 when no ingress port is free) surfaces as
// *AgentError so the CP worker can classify it.
//
// The agent endpoint is name-keyed (`POST /v1/vms/{vm_name}/migrations/incoming`).
func (c *Client) StartIncomingMigration(
	ctx context.Context,
	endpoint, vmName string,
	req agentapi.MigrationIncomingRequest,
) (agentapi.MigrationIncomingResponse, error) {
	var zero agentapi.MigrationIncomingResponse

	body, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: encode MigrationIncomingRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/migrations/incoming")
	httpReq, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	_, respBody, err := c.do(httpReq)
	if err != nil {
		return zero, fmt.Errorf("agentclient: start incoming migration: %w", err)
	}

	var resp agentapi.MigrationIncomingResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return zero, fmt.Errorf("agentclient: decode MigrationIncomingResponse: %v", err)
	}
	return resp, nil
}

// StartOutgoingMigration tells the **source** agent to begin pushing
// vmName to the target, using the auth token + listen endpoint the
// target returned from StartIncomingMigration. The work is async: 202
// returns an AsyncTaskAccepted whose task id this method returns. The
// CP worker feeds that id into PollTask to drive the transfer to a
// terminal status; richer migration progress is observed through
// GetMigration. Non-2xx surfaces as *AgentError.
//
// The agent endpoint is name-keyed (`POST /v1/vms/{vm_name}/migrations/outgoing`).
func (c *Client) StartOutgoingMigration(
	ctx context.Context,
	endpoint, vmName string,
	req agentapi.MigrationOutgoingRequest,
) (string, error) {
	body, err := json.Marshal(req) //nolint:gosec // G117: auth_token is part of the wire contract — the source agent must present the target-minted single-use token to open the migration channel; this marshals it onto the request by design, it is not a leak.
	if err != nil {
		return "", fmt.Errorf("agentclient: encode MigrationOutgoingRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/migrations/outgoing")
	httpReq, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	_, respBody, err := c.do(httpReq)
	if err != nil {
		return "", fmt.Errorf("agentclient: start outgoing migration: %w", err)
	}

	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		return "", fmt.Errorf("agentclient: decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskID == uuid.Nil {
		return "", fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid")
	}
	return accepted.TaskID.String(), nil
}

// GetMigration fetches the agent's view of a single migration for
// progress polling (phase, bytes transferred, downtime). 200 returns a
// parsed Migration; 404 / 5xx surface as *AgentError; malformed body as
// a wrapped generic error.
//
// The agent endpoint is name-keyed (`GET /v1/vms/{vm_name}/migrations/{migration_id}`).
func (c *Client) GetMigration(
	ctx context.Context,
	endpoint, vmName, migrationID string,
) (agentapi.Migration, error) {
	var zero agentapi.Migration

	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/migrations/"+migrationID)
	req, err := newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return zero, err
	}

	_, body, err := c.do(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: get migration: %w", err)
	}

	var m agentapi.Migration
	if err := json.Unmarshal(body, &m); err != nil {
		return zero, fmt.Errorf("agentclient: decode Migration: %v", err)
	}
	return m, nil
}

// PostHeartbeatNudge triggers an immediate heartbeat on the agent at
// endpoint (CP fast-push after a migration cutover so overlay peers
// re-pull their FDB without waiting for the next periodic tick). The
// agent endpoint returns 204 with no body; best-effort at the call site.
//
// The agent endpoint is fixed (`POST /v1/heartbeat/nudge`).
func (c *Client) PostHeartbeatNudge(ctx context.Context, endpoint string) error {
	target, _ := url.JoinPath(endpoint, "/v1/heartbeat/nudge")
	req, err := newRequest(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	if _, _, err := c.do(req); err != nil {
		return fmt.Errorf("agentclient: post heartbeat nudge: %w", err)
	}
	return nil
}

// CancelMigration requests best-effort cancellation of an in-flight
// migration. The agent acts synchronously: 200 returns the refreshed
// Migration view (phase typically `cancelled` once the abort lands);
// non-2xx (e.g. 409 when the migration already reached a terminal
// phase) surfaces as *AgentError.
//
// The agent endpoint is name-keyed
// (`POST /v1/vms/{vm_name}/migrations/{migration_id}/cancel`).
func (c *Client) CancelMigration(
	ctx context.Context,
	endpoint, vmName, migrationID string,
) (agentapi.Migration, error) {
	var zero agentapi.Migration

	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/migrations/"+migrationID+"/cancel")
	req, err := newRequest(ctx, http.MethodPost, target, nil)
	if err != nil {
		return zero, err
	}

	_, body, err := c.do(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: cancel migration: %w", err)
	}

	var m agentapi.Migration
	if err := json.Unmarshal(body, &m); err != nil {
		return zero, fmt.Errorf("agentclient: decode Migration: %v", err)
	}
	return m, nil
}
