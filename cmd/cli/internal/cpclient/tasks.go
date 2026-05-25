// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Task mirrors components/schemas/Task that GET /v1/tasks/{id}
// returns. Status is one of pending / running / success / failed /
// cancelled. Result and Error are JSONB blobs the server projected
// from the worker's terminal envelope.
type Task struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	Progress     *int            `json:"progress"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *string         `json:"resource_id"`
	Result       json.RawMessage `json:"result"`
	Error        json.RawMessage `json:"error"`
	Attempts     int             `json:"attempts"`
	MaxAttempts  int             `json:"max_attempts"`
	CreatedAt    string          `json:"created_at"`
	StartedAt    *string         `json:"started_at"`
	FinishedAt   *string         `json:"finished_at"`
}

// IsTerminal reports whether the task has reached one of the three
// terminal statuses. Used by WaitTask's polling loop and by callers
// that want to render a one-shot summary without awaiting.
func (t *Task) IsTerminal() bool {
	switch t.Status {
	case "success", "failed", "cancelled":
		return true
	}
	return false
}

// TaskErrorEnvelope is the inner-error shape the worker writes into
// tasks.error on failed / cancelled terminals. Exported so that the
// CLI can render it verbatim.
type TaskErrorEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// DecodeError unmarshals the task's error JSONB into a typed envelope.
// Returns (nil, nil) when error is empty (success terminal or not
// yet finished); a populated envelope on failed / cancelled.
func (t *Task) DecodeError() (*TaskErrorEnvelope, error) {
	if len(t.Error) == 0 || string(t.Error) == "null" {
		return nil, nil
	}
	var env TaskErrorEnvelope
	if err := json.Unmarshal(t.Error, &env); err != nil {
		return nil, fmt.Errorf("decode task error: %v", err)
	}
	return &env, nil
}

// WaitOptions configures WaitTask's polling cadence. Zero values fall
// back to sensible defaults (1s initial, 5s ceiling, 5min total).
type WaitOptions struct {
	Initial time.Duration
	Max     time.Duration
	Timeout time.Duration
	OnPoll  func(t Task) // optional progress callback
}

const (
	defaultPollInitial = 1 * time.Second
	defaultPollMax     = 5 * time.Second
	defaultPollTimeout = 5 * time.Minute
)

// GetTask fetches GET /v1/tasks/{id}. 200 returns the parsed Task;
// 404 / 5xx surface as *APIError.
func (c *Client) GetTask(ctx context.Context, taskID uuid.UUID) (Task, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/tasks/"+taskID.String(), nil)
	if err != nil {
		return Task{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Task{}, err
	}
	var out Task
	if err := decodeJSON(body, &out); err != nil {
		return Task{}, err
	}
	return out, nil
}

// WaitTask polls /v1/tasks/{id} until the task reaches a terminal
// status, opts.Timeout is hit, or ctx is cancelled. The polling
// cadence starts at opts.Initial and doubles up to opts.Max — the same
// shape agentclient.PollTask uses, lighter parameters because the
// CP-side latency is human-poll friendly.
//
// Transient *APIError (5xx / 429) retries silently; permanent errors
// abort. On opts.Timeout the returned error wraps
// context.DeadlineExceeded so callers can errors.Is them.
//
// opts.OnPoll fires after every successful poll (terminal or not),
// letting the CLI emit progress dots / status updates without the wait
// helper having to know about output formatting.
func (c *Client) WaitTask(ctx context.Context, taskID uuid.UUID, opts WaitOptions) (Task, error) {
	if opts.Initial <= 0 {
		opts.Initial = defaultPollInitial
	}
	if opts.Max <= 0 {
		opts.Max = defaultPollMax
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultPollTimeout
	}

	loopCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	delay := opts.Initial
	for {
		task, err := c.GetTask(loopCtx, taskID)
		if err == nil {
			if opts.OnPoll != nil {
				opts.OnPoll(task)
			}
			if task.IsTerminal() {
				return task, nil
			}
		} else {
			var apiErr *APIError
			if errors.As(err, &apiErr) && !apiErr.IsRetryable() {
				return Task{}, err
			}
			if !errors.As(err, &apiErr) && loopCtx.Err() != nil {
				return Task{}, fmt.Errorf("wait task: %w", loopCtx.Err())
			}
			// Transient — fall through to sleep + retry.
		}

		if err := sleepCtx(loopCtx, delay); err != nil {
			return Task{}, fmt.Errorf("wait task: %w", err)
		}
		delay *= 2
		if delay > opts.Max {
			delay = opts.Max
		}
	}
}

// sleepCtx blocks for d or until ctx is done, whichever fires first.
// Returns ctx.Err() on cancellation, nil on full sleep.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
