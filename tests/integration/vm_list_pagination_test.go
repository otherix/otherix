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

// TestVMList_VerticalSlicePagination creates 5 VMs и walks the cursor
// pagination forward с limit=2 expecting 3 pages (2 + 2 + 1) и a
// nil next_cursor on the last.
func TestVMList_VerticalSlicePagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-page", 0x10, "private")

	const total = 5
	for i := 0; i < total; i++ {
		taskID, _ := v.createVM(t, ctx, vmCreateBody{
			Name:     "page-vm-" + uuid.NewString()[:8],
			Template: tpl.Name,
			Pool:     v.pool.ID.String(),
			VCPUs:    1,
			MemoryMB: 256,
		}, "")
		v.awaitVMCreateEvent(t, 15*time.Second)
		_ = taskID
	}

	type listResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}

	walkOnePage := func(cursor string) listResp {
		t.Helper()
		q := url.Values{}
		q.Set("limit", "2")
		if cursor != "" {
			q.Set("cursor", cursor)
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

	// Walk pages — collect ids, assert no duplicates, last page's
	// next_cursor nil. Note: the order is by (created_at, id) DESC,
	// which is stable but not predictable от outside.
	seen := map[string]struct{}{}
	cursor := ""
	pageNum := 0
	for {
		page := walkOnePage(cursor)
		pageNum++
		for _, item := range page.Data {
			if _, dup := seen[item.ID]; dup {
				t.Fatalf("duplicate id %s seen across pages", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
		if page.Meta.NextCursor == nil {
			// Last page reached. Check we observed all 5.
			if len(seen) != total {
				t.Errorf("total ids seen = %d, want %d", len(seen), total)
			}
			if pageNum < 2 {
				// 5 / 2 = 3 pages. Hitting the end on page 1 means
				// pagination is not actually paging.
				t.Errorf("only %d page(s) walked, want ≥ 3 для total=%d limit=2", pageNum, total)
			}
			return
		}
		cursor = *page.Meta.NextCursor
		if pageNum > 10 {
			t.Fatalf("walk did not terminate after 10 pages — pagination loop?")
		}
	}
}
