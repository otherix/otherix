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

// ServeBlob tells the holder agent at endpoint to open its peer serve
// listener for the request's digest, primed with the CP-minted token, and
// returns the holder's reachable serve endpoint plus expiry. The work is
// synchronous: 200 returns the BlobServeResponse the CP threads into
// PullBlob on the consumer agent; non-2xx surfaces as *AgentError so the CP
// broker can classify it.
//
// The agent endpoint is fixed (`POST /v1/blobs/serve`).
func (c *Client) ServeBlob(
	ctx context.Context,
	endpoint string,
	req agentapi.BlobServeRequest,
) (agentapi.BlobServeResponse, error) {
	var zero agentapi.BlobServeResponse

	body, err := json.Marshal(req) //nolint:gosec // G117: token is part of the wire contract - the holder agent must hand the CP-minted single-use token to the consumer to open the serve channel; this marshals it onto the request by design, it is not a leak.
	if err != nil {
		return zero, fmt.Errorf("agentclient: encode BlobServeRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/blobs/serve")
	httpReq, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	_, respBody, err := c.do(httpReq)
	if err != nil {
		return zero, fmt.Errorf("agentclient: serve blob: %w", err)
	}

	var resp agentapi.BlobServeResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return zero, fmt.Errorf("agentclient: decode BlobServeResponse: %v", err)
	}
	return resp, nil
}

// PullBlob tells the consumer agent at endpoint to pull the request's digest
// from the holder's serve endpoint, presenting the per-op token. The work is
// async: 202 returns an AsyncTaskAccepted whose task id this method returns.
// The CP broker feeds that id into PollTask to drive the pull to a terminal
// status. Non-2xx surfaces as *AgentError.
//
// The agent endpoint is fixed (`POST /v1/blobs/pull`).
func (c *Client) PullBlob(
	ctx context.Context,
	endpoint string,
	req agentapi.BlobPullRequest,
) (string, error) {
	body, err := json.Marshal(req) //nolint:gosec // G117: token is part of the wire contract - the consumer agent must present the CP-minted single-use token to the holder to open the pull channel; this marshals it onto the request by design, it is not a leak.
	if err != nil {
		return "", fmt.Errorf("agentclient: encode BlobPullRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/blobs/pull")
	httpReq, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	_, respBody, err := c.do(httpReq)
	if err != nil {
		return "", fmt.Errorf("agentclient: pull blob: %w", err)
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
