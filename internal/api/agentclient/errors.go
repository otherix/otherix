// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"context"
	"fmt"
)

// AgentError is the typed error returned when the agent responds with
// a non-2xx HTTP status. Status carries the raw HTTP code; Code /
// Message / Details mirror the OpenAPI Error envelope inner shape
// (api/openapi/agent.yaml#components/schemas/Error).
//
// Code defaults to "" when the response body is malformed or absent;
// callers that need to discriminate must inspect Status first.
type AgentError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

// Error implements error. Renders status + code + message in a stable
// form so logs and assertions can match on substring.
func (e *AgentError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("agent: HTTP %d", e.Status)
	}
	return fmt.Sprintf("agent: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// IsRetryable reports whether the error category warrants retrying
// the request inside the polling loop. 5xx and 429 are transient;
// 4xx (other than 429) are permanent: the agent has rejected the
// request shape or scope and retries cannot recover. Used by
// PollTask to decide whether to keep polling after a non-2xx
// response on GET /v1/tasks/{id}.
func (e *AgentError) IsRetryable() bool {
	if e == nil {
		return false
	}
	if e.Status == 429 {
		return true
	}
	return e.Status >= 500 && e.Status < 600
}

// TimeoutError is returned by PollTask when the polling budget
// (cfg.Timeout) elapses before the agent task reaches a terminal
// status. Wraps context.DeadlineExceeded so errors.Is(err,
// context.DeadlineExceeded) continues to hold for callers that
// already branch on the standard sentinel.
type TimeoutError struct {
	AgentTaskID string
	Budget      string
}

// Error implements error.
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("agentclient: poll budget %s exceeded for task %s", e.Budget, e.AgentTaskID)
}

// Unwrap exposes context.DeadlineExceeded for errors.Is matching.
func (e *TimeoutError) Unwrap() error { return context.DeadlineExceeded }
