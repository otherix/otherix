// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// PostImageImport submits an image import for poolName and returns the
// agent's task id from the AsyncTaskAccepted body. The agent is
// expected to respond with 202 + JSON matching
// api/openapi/agent.yaml#components/schemas/AsyncTaskAccepted; non-2xx
// responses surface as *AgentError, malformed body as а generic error.
//
// The agent's pool registry is name-keyed; CP-side callers load the
// `storage_pools` row and pass `pool.Name`. The per-instance UUID
// stays at the operator API edge.
//
// req carries the agent-side wire shape verbatim. idempotencyKey is
// forwarded as Idempotency-Key.
func (c *Client) PostImageImport(
	ctx context.Context,
	endpoint string,
	poolName string,
	idempotencyKey string,
	req agentapi.ImageImportRequest,
) (uuid.UUID, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: encode ImageImportRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/storage-pools/"+url.PathEscape(poolName)+"/images")
	httpReq, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return uuid.Nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}

	_, respBody, err := c.do(httpReq)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: post image import: %w", err)
	}

	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskId == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid")
	}
	return accepted.TaskId, nil
}

// DeleteImage removes the image identified by its lowercase-hex
// sha256 from poolID. The agent contract is synchronous — 204 on
// success, 404 when the file is already gone.
//
// 404 is mapped to nil so the helper is idempotent: the CP-side
// refcount-gated delete handler (Step 9) calls this only when the
// last storage_images row referencing the file is being removed,
// and a stale state where the agent has already collected the file
// (manual operator cleanup, separate reconciliation pass, …) must
// not flip the CP transaction into rollback. Other 4xx (e.g.
// agent's `image_in_use` 409) and 5xx surface as *AgentError; the
// caller branches on Status to distinguish retryable from
// permanent failures.
func (c *Client) DeleteImage(
	ctx context.Context,
	endpoint string,
	poolName string,
	checksumSHA256 string,
	idempotencyKey string,
) error {
	target, _ := url.JoinPath(endpoint, "/v1/storage-pools/"+url.PathEscape(poolName)+"/images/"+checksumSHA256)
	httpReq, err := newRequest(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}

	_, _, err = c.do(httpReq)
	if err == nil {
		return nil
	}
	var ae *AgentError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("agentclient: delete image: %w", err)
}

// ListImages walks the agent's cached-image inventory for poolID,
// following cursor pagination until exhausted, and returns the
// aggregated slice. Used by the post-scan reconciliation step to
// compare agent state against the CP's `storage_images` projection.
//
// The cursor is opaque; the agent emits next_cursor
// as null on the last page (decoded as nil pointer in
// agentapi.PaginationMeta).
func (c *Client) ListImages(
	ctx context.Context,
	endpoint string,
	poolName string,
) ([]agentapi.CachedImage, error) {
	var aggregated []agentapi.CachedImage
	cursor := ""
	for {
		page, next, err := c.listImagesPage(ctx, endpoint, poolName, cursor)
		if err != nil {
			return nil, err
		}
		aggregated = append(aggregated, page...)
		if next == "" {
			return aggregated, nil
		}
		cursor = next
	}
}

// listImagesPage issues one GET /v1/storage-pools/{pool_name}/images
// page request and returns the page's data plus the decoded
// next-cursor (empty string when last page). Network / transport
// errors and non-2xx responses propagate verbatim.
func (c *Client) listImagesPage(
	ctx context.Context,
	endpoint string,
	poolName string,
	cursor string,
) ([]agentapi.CachedImage, string, error) {
	target, _ := url.JoinPath(endpoint, "/v1/storage-pools/"+url.PathEscape(poolName)+"/images")
	if cursor != "" {
		target += "?cursor=" + url.QueryEscape(cursor)
	}

	httpReq, err := newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}

	_, body, err := c.do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("agentclient: list images: %w", err)
	}

	var page agentapi.CachedImageList
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, "", fmt.Errorf("agentclient: decode CachedImageList: %v", err)
	}
	next := ""
	if page.Meta.NextCursor != nil {
		next = *page.Meta.NextCursor
	}
	return page.Data, next, nil
}
