// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageImport_VerticalSliceAgentFailure exercises the
// agent-reports-failure branch of the import saga across the three
// representative agent error codes promoted to public ErrorCode
// entries (qcow2_header_invalid, checksum_mismatch, pool_full).
// Each subtest stages the mock with a terminal-failed outcome
// carrying the matching envelope, fires CP import, and asserts the
// task ends in `failed` with the agent envelope code passing
// through to tasks.error.code (classifyImportError agent-envelope
// passthrough).
func TestStorageImageImport_VerticalSliceAgentFailure(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		message string
	}{
		{"checksum_mismatch", "checksum_mismatch", "computed sha256 differs from advertised"},
		{"qcow2_header_invalid", "qcow2_header_invalid", "backing file present"},
		{"pool_full", "pool_full", "advertised available bytes too small"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			v := newVerticalSlice(t, ctx, fastAgentClientCfg())
			tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-fail-"+tc.name, 0x55)
			checksum := hexChecksum(tpl.ImageChecksumSha256)

			v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
				Status: "failed",
				Error: &agentmock.ErrorEnvelope{
					Code:    tc.code,
					Message: tc.message,
				},
				Delay: 30 * time.Millisecond,
			})

			taskID, _ := v.importImage(t, ctx, tpl.ID, "")
			v.awaitImportEvent(t, 15*time.Second)

			row, err := v.store.Queries().GetTask(ctx, taskID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if row.Status != store.TaskStatusFailed {
				t.Fatalf("task.Status = %q, want failed (error=%s)", row.Status, string(row.Error))
			}
			if row.Result != nil {
				t.Errorf("task.Result = %s, want nil on failure", string(row.Result))
			}
			if row.Error == nil {
				t.Fatal("task.Error = nil, want envelope")
			}

			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(row.Error, &envelope); err != nil {
				t.Fatalf("unmarshal task.error: %v", err)
			}
			if envelope.Code != tc.code {
				t.Errorf("envelope.code = %q, want %q (agent-envelope passthrough)", envelope.Code, tc.code)
			}

			page, err := v.store.Queries().ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{
				PoolID:     v.pool.ID,
				LimitCount: 10,
			})
			if err != nil {
				t.Fatalf("ListStorageImagesByPool: %v", err)
			}
			if len(page) != 0 {
				t.Errorf("storage_images rows = %d, want 0 on failure", len(page))
			}
		})
	}
}
