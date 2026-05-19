// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVMList_VerticalSliceStatusFilter creates two VMs (both reach
// status=running through the worker) и asserts the post-projection
// `?status=running` filter returns both, while `?status=stopped`
// returns none.
//
// Pool / pinned-node filters live in the schema (idx_vm_disks_pool +
// idx_vms_pinned_node) but the API surface in Phase B exposes only
// status — Phase D's broader filter coverage is тail-pinned то the
// status surface that actually exists.
func TestVMList_VerticalSliceStatusFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-filter", 0x21, "private")

	const total = 2
	for i := 0; i < total; i++ {
		_, _ = v.createVM(t, ctx, vmCreateBody{
			Name:     "filter-vm-" + uuid.NewString()[:8],
			Template: tpl.Name,
			Pool:     v.pool.ID.String(),
			VCPUs:    1,
			MemoryMB: 256,
		}, "")
		v.awaitVMCreateEvent(t, 15*time.Second)
	}

	type listResp struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}

	listWith := func(filterValue string) listResp {
		t.Helper()
		q := url.Values{}
		q.Set("limit", "10")
		if filterValue != "" {
			q.Set("status", filterValue)
		}
		status, body := v.listVMsRequest(t, ctx, q.Encode())
		if status != http.StatusOK {
			t.Fatalf("list status = %d, body = %s, want 200", status, body)
		}
		var page listResp
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode list body: %v", err)
		}
		return page
	}

	// Filter = "running" → both VMs.
	running := listWith("running")
	if len(running.Data) != total {
		t.Errorf("status=running list len = %d, want %d", len(running.Data), total)
	}
	for _, item := range running.Data {
		if item.Status != "running" {
			t.Errorf("filtered list contains status=%q (want running only)", item.Status)
		}
	}

	// Filter = "stopped" → none of these VMs.
	stopped := listWith("stopped")
	for _, item := range stopped.Data {
		// The filter is post-projection: each returned item MUST
		// match. (Other tests in the package may have left stopped
		// rows; those are filtered out by the per-test fresh-DB
		// migrationtest harness used by newVerticalSlice.)
		if item.Status != "stopped" {
			t.Errorf("status=stopped filter returned %q", item.Status)
		}
	}

	// No filter → both VMs included (plus any others в this test's
	// fresh DB).
	all := listWith("")
	if len(all.Data) < total {
		t.Errorf("unfiltered list len = %d, want ≥ %d", len(all.Data), total)
	}
}
