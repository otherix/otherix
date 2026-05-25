// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// PostScan submits a scan request for poolName and returns the agent's
// task id from the AsyncTaskAccepted body. The agent is expected to
// respond with 202 + JSON matching
// api/openapi/agent.yaml#components/schemas/AsyncTaskAccepted; any
// non-2xx response surfaces as *AgentError, malformed body as a
// generic error.
//
// The agent's pool registry is name-keyed; CP-side callers load the
// `storage_pools` row and pass `pool.Name`. The per-instance UUID
// stays at the operator API edge.
//
// idempotencyKey is the value the CP attaches as Idempotency-Key on
// the request. The CP generates it fresh for each scan attempt
// (UUID v7 in production); the agent's idempotency scope is
// independent of the user-facing CP idempotency scope.
func (c *Client) PostScan(
	ctx context.Context,
	endpoint string,
	poolName string,
	idempotencyKey string,
) (uuid.UUID, error) {
	target, _ := url.JoinPath(endpoint, "/v1/storage-pools/"+url.PathEscape(poolName)+"/scan")

	req, err := newRequest(ctx, http.MethodPost, target, nil)
	if err != nil {
		return uuid.Nil, err
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	_, body, err := c.do(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: post scan: %w", err)
	}

	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(body, &accepted); err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid")
	}
	return accepted.TaskID, nil
}
