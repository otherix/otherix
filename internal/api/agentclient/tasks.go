// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// TaskTerminal is the projection of agentapi.Task at a terminal
// status (success / failed / cancelled). PollTask returns this once
// the agent reports any of those three; pending / running drive
// further polling.
type TaskTerminal struct {
	// Status is "success", "failed", or "cancelled" — verbatim from
	// the agent's Task.status field.
	Status string

	// Result is the OpenAPI Task.result map for success cases. nil
	// when the agent emits null / omits the field, including the
	// failed / cancelled cases (those carry Error instead).
	Result map[string]any

	// Error is populated on failed / cancelled. nil on success.
	Error *AgentError
}

// terminal task statuses, lifted to constants for readability of
// the polling loop.
const (
	taskStatusSuccess   = "success"
	taskStatusFailed    = "failed"
	taskStatusCancelled = "cancelled"
)

// PollTask polls GET /v1/tasks/{agentTaskID} on the agent until the
// task reaches a terminal status (success / failed / cancelled), the
// caller's ctx is cancelled, or the per-call budget cfg.Timeout is
// exhausted.
//
// The polling cadence is exponential: starts at cfg.PollInterval (1s
// by default), doubles after each pending / running response, and
// caps at cfg.PollMaxInterval (30s by default). Transient errors
// (network failure, 5xx, 429) retry with the same backoff curve;
// permanent 4xx (404, 400, 401, 403) abort the loop and return the
// AgentError verbatim.
//
// On budget exhaustion the returned error is *TimeoutError, which
// unwraps to context.DeadlineExceeded for callers that branch on the
// stdlib sentinel.
func (c *Client) PollTask(
	ctx context.Context,
	endpoint string,
	agentTaskID uuid.UUID,
) (TaskTerminal, error) {
	loopCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	delay := c.cfg.PollInterval

	for {
		task, err := c.getTask(loopCtx, endpoint, agentTaskID)
		switch {
		case err == nil:
			if terminal, done := terminalProjection(task); done {
				return terminal, nil
			}
			// pending / running — fall through to backoff sleep.

		case isAgentRetryable(err):
			// 5xx / 429: same backoff curve, no early return.

		default:
			// ctx cancellation, permanent 4xx, decode error.
			return TaskTerminal{}, c.classifyTerminalError(loopCtx, ctx, agentTaskID, err)
		}

		if err := sleepCtx(loopCtx, delay); err != nil {
			return TaskTerminal{}, c.classifyTerminalError(loopCtx, ctx, agentTaskID, err)
		}
		delay = nextDelay(delay, c.cfg.PollMaxInterval)
	}
}

// getTask issues one GET /v1/tasks/{id} and decodes the agent Task.
// 404 / 5xx surface as *AgentError; the polling loop branches on
// IsRetryable() to decide retry vs return.
func (c *Client) getTask(
	ctx context.Context,
	endpoint string,
	agentTaskID uuid.UUID,
) (*agentapi.Task, error) {
	target, _ := url.JoinPath(endpoint, "/v1/tasks/"+agentTaskID.String())

	req, err := newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}

	_, body, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("agentclient: get task: %w", err)
	}

	var task agentapi.Task
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, fmt.Errorf("agentclient: decode Task: %v", err)
	}
	return &task, nil
}

// terminalProjection inspects task.Status and either returns a
// populated TaskTerminal + true (terminal status reached) or the
// zero TaskTerminal + false (still polling). The agent's
// task.Status type is a string alias; comparison is by value.
func terminalProjection(task *agentapi.Task) (TaskTerminal, bool) {
	switch string(task.Status) {
	case taskStatusSuccess:
		return TaskTerminal{
			Status: taskStatusSuccess,
			Result: derefResult(task.Result),
		}, true
	case taskStatusFailed, taskStatusCancelled:
		t := TaskTerminal{Status: string(task.Status)}
		if task.Error != nil {
			details := map[string]any(nil)
			if task.Error.Details != nil {
				details = *task.Error.Details
			}
			code := ""
			if task.Error.Code != nil {
				code = string(*task.Error.Code)
			}
			message := ""
			if task.Error.Message != nil {
				message = *task.Error.Message
			}
			t.Error = &AgentError{
				Code:    code,
				Message: message,
				Details: details,
			}
		}
		return t, true
	}
	return TaskTerminal{}, false
}

// derefResult flattens *map[string]any to map[string]any (or nil)
// because agentapi codegen wraps optional object fields in pointers.
func derefResult(p *map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	return *p
}

// isAgentRetryable reports whether err is a transient agent error
// the polling loop should retry. Three categories qualify: a network
// error from the underlying transport (connection refused, DNS,
// dropped TCP), a 5xx AgentError, or a 429 rate-limit. Everything
// else is permanent.
func isAgentRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *AgentError
	if errors.As(err, &ae) {
		return ae.IsRetryable()
	}
	// Network / transport errors are retryable; ctx cancellation is
	// surfaced as ctx.Err() and intentionally NOT retryable. We
	// cannot use errors.Is(err, context.DeadlineExceeded) here
	// because the polling loop's own deadline produces that — we
	// route ctx cancellation through classifyTerminalError instead.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// classifyTerminalError converts a non-retryable polling error into
// the error returned to PollTask's caller. ctx-cancellation that
// originated from the loop's own WithTimeout (i.e. the parent ctx
// is still healthy) becomes *TimeoutError; ctx-cancellation that
// originated from the parent stays as ctx.Err() so callers that
// already branch on context.Canceled / context.DeadlineExceeded
// observe the right shape.
func (c *Client) classifyTerminalError(
	loopCtx, parentCtx context.Context,
	agentTaskID uuid.UUID,
	err error,
) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if parentCtx.Err() == nil && loopCtx.Err() != nil {
			return &TimeoutError{
				AgentTaskID: agentTaskID.String(),
				Budget:      c.cfg.Timeout.String(),
			}
		}
		return parentCtx.Err()
	}
	return err
}

// sleepCtx blocks for d or until ctx is cancelled. Returns ctx.Err()
// on cancellation, nil on natural wake-up.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// nextDelay doubles current up to max. The cap is inclusive: when
// current * 2 exceeds max, the next delay is exactly max, then stays
// there until the loop exits.
func nextDelay(current, max time.Duration) time.Duration {
	doubled := current * 2
	if doubled > max {
		return max
	}
	return doubled
}
