// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// HealthResponse is the parsed shape of GET /health on the agent. The
// agent serves /health at the root path (not under /v1/) for
// Kubernetes-style probe compatibility — same convention as the CP-side
// /healthz / /readyz.
type HealthResponse struct {
	Status  string    `json:"status"`
	NodeID  string    `json:"node_id"`
	Build   string    `json:"build"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// Health invokes the agent's GET /health endpoint via mTLS. The endpoint
// argument is the agent's base URL (e.g. https://127.0.0.1:9443);
// /health is appended internally so callers do not have to know the
// path convention. Used by the otherix CLI's ping-agent subcommand and
// suitable for any future probe-style validation tooling.
//
// Returns the parsed envelope on 2xx; *AgentError on non-2xx (callers
// can branch on errors.As); a wrapped error на TLS / network failure
// or a malformed response body.
func (c *Client) Health(ctx context.Context, endpoint string) (HealthResponse, error) {
	var zero HealthResponse

	target, err := url.JoinPath(endpoint, "/health")
	if err != nil {
		return zero, fmt.Errorf("agentclient: join /health url: %w", err)
	}

	req, err := newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return zero, err
	}

	_, body, err := c.do(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: get /health: %w", err)
	}

	var resp HealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return zero, fmt.Errorf("agentclient: decode health response: %w", err)
	}
	return resp, nil
}
