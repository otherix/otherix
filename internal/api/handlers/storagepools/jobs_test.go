// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStoragePoolScanArgs_Kind(t *testing.T) {
	t.Parallel()
	if got, want := (StoragePoolScanArgs{}).Kind(), "storage_pool.scan"; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
}

func TestMarshalResult(t *testing.T) {
	t.Parallel()

	stamp, err := time.Parse(time.RFC3339Nano, "2026-05-07T12:34:56.789Z")
	if err != nil {
		t.Fatalf("parse stamp: %v", err)
	}
	raw, err := marshalResult(ScanResult{
		CapacityBytes:  1 << 40,
		AvailableBytes: 1 << 39,
		ReportedAt:     stamp,
	})
	if err != nil {
		t.Fatalf("marshalResult: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"capacity_bytes", "available_bytes", "reported_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("result JSON missing %q key (got: %v)", k, got)
		}
	}
	if c, _ := got["capacity_bytes"].(float64); int64(c) != 1<<40 {
		t.Errorf("capacity_bytes = %v, want %d", got["capacity_bytes"], int64(1<<40))
	}
}

func TestMarshalError(t *testing.T) {
	t.Parallel()

	raw, err := marshalError("scan_failed", "agent timed out")
	if err != nil {
		t.Fatalf("marshalError: %v", err)
	}
	var got struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "scan_failed" {
		t.Errorf("code = %q, want scan_failed", got.Code)
	}
	if got.Message != "agent timed out" {
		t.Errorf("message = %q, want %q", got.Message, "agent timed out")
	}
}
