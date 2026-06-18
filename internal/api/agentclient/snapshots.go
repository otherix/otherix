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

// PostSnapshot submits a snapshot-create request for vmName and returns the
// agent's task id from the AsyncTaskAccepted body. The work is async: the agent
// responds 202 (`POST /v1/vms/{vm_name}/snapshots`) with an AsyncTaskAccepted; the
// CP worker feeds the returned id into PollTask to drive the blob produce to a
// terminal status. Non-2xx surfaces as *AgentError; a malformed body as a generic
// wrapped error.
//
// The agent endpoint is name-keyed; CP-side callers resolve the snapshot's VM and
// pass vm.Name.
func (c *Client) PostSnapshot(
	ctx context.Context,
	endpoint, vmName string,
	body agentapi.SnapshotCreateRequest,
) (uuid.UUID, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: encode SnapshotCreateRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/vms/"+url.PathEscape(vmName)+"/snapshots")
	req, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return uuid.Nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	_, respBody, err := c.do(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: post snapshot: %w", err)
	}

	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid")
	}
	return accepted.TaskID, nil
}

// DeleteSnapshot requests deletion of the snapshot snapshotName captured from VM
// vmID on the agent (`DELETE /v1/snapshots/{vm_id}/{snapshot_name}`). The endpoint
// is node-level and keyed on the immutable vm_id (NOT the live VM): the agent
// locates the manifest by vm_uuid and removes the content-addressed blobs by digest,
// so deletion works after the source VM is gone. The orphaned digests - the
// fail-closed-GC set the CP reference graph proved are no longer referenced by any
// other snapshot - are passed as repeated `orphaned` query params so the agent
// removes exactly those local blobs (a still-shared blob is never in the set). The
// agent responds 202 (async); a non-terminal poll is not awaited here (the agent
// removal is bounded). Non-2xx surfaces as *AgentError.
func (c *Client) DeleteSnapshot(
	ctx context.Context,
	endpoint string,
	vmID uuid.UUID,
	snapshotName string,
	orphaned []string,
) error {
	target, _ := url.JoinPath(endpoint, "/v1/snapshots/"+url.PathEscape(vmID.String())+"/"+url.PathEscape(snapshotName))
	if len(orphaned) > 0 {
		q := url.Values{}
		for _, d := range orphaned {
			q.Add("orphaned", d)
		}
		target += "?" + q.Encode()
	}

	req, err := newRequest(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	if _, _, err := c.do(req); err != nil {
		return fmt.Errorf("agentclient: delete snapshot: %w", err)
	}
	return nil
}
