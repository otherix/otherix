// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// reconciliationLogBuffer wraps bytes.Buffer with a sync.Mutex so the
// concurrent slog.JSONHandler writes (worker goroutine) and the
// test-side reads do not race under -race. Pure ergonomics — slog
// itself takes a sync.Locker via its handler implementation, but
// bytes.Buffer alone is not goroutine-safe.
type reconciliationLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *reconciliationLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *reconciliationLogBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	raw := append([]byte{}, b.buf.Bytes()...)
	b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// triggerScan POSTs a scan against v.pool and waits for the worker
// to project terminal-success. Reconciliation runs inline at the
// tail of Work() per Step 11, so by the time awaitScanEvent
// returns the captured log buffer holds the diagnostic lines.
func triggerScan(t *testing.T, ctx context.Context, v *verticalSlice) {
	t.Helper()
	v.mock.AddPoolScanResult(v.pool.Name, agentmock.PoolScanResult{
		Status:         "success",
		CapacityBytes:  1 << 40,
		AvailableBytes: 1 << 39,
		Delay:          30 * time.Millisecond,
	})
	_, _ = v.postScan(t, ctx, "")
	v.awaitScanEvent(t, 15*time.Second)
}

// pickReconciliationLines filters the captured records for the three
// Step 11 message values. Other goroutines (river internals, request
// loggers) emit unrelated entries; we only assert on the
// reconciliation messages.
func pickReconciliationLines(records []map[string]any) []map[string]any {
	var out []map[string]any
	for _, rec := range records {
		msg, _ := rec["msg"].(string)
		if strings.HasPrefix(msg, "storage_image_") || msg == "scan_reconciliation_failed" {
			out = append(out, rec)
		}
	}
	return out
}

// TestStorageImageReconciliation_VerticalSliceCleanState runs a scan
// when both sides agree on the inventory (one storage_images row,
// one mock-side cached image with the same sha256). Step 11's
// reconciliation must emit ZERO `storage_image_*` lines.
func TestStorageImageReconciliation_VerticalSliceCleanState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logBuf := &reconciliationLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v := newVerticalSliceWithLogger(t, ctx, fastAgentClientCfg(), log)

	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-recon-clean", 0x99)
	checksum := hexChecksum(tpl.ImageChecksumSha256)
	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status: "success", SizeBytes: 4096, Format: "qcow2", Delay: 30 * time.Millisecond,
	})
	_, _ = v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	triggerScan(t, ctx, v)

	got := pickReconciliationLines(logBuf.lines(t))
	if len(got) != 0 {
		t.Errorf("expected zero reconciliation log lines on clean state, got %d: %+v", len(got), got)
	}
}

// TestStorageImageReconciliation_VerticalSliceOrphan: an image
// staged on the agent without a CP-side storage_images row triggers
// a `storage_image_orphan_on_agent` WARN line carrying sha256,
// size_bytes, agent_path, and pool_id.
func TestStorageImageReconciliation_VerticalSliceOrphan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logBuf := &reconciliationLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v := newVerticalSliceWithLogger(t, ctx, fastAgentClientCfg(), log)

	const orphanChecksum = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const orphanSize int64 = 8192
	if err := v.mock.AddImage(v.pool.Name, agentmock.CachedImage{
		ChecksumSHA256: orphanChecksum,
		Format:         "qcow2",
		SizeBytes:      orphanSize,
		Path:           "/opt/otherix/pools/" + v.pool.ID.String() + "/" + orphanChecksum + ".qcow2",
	}); err != nil {
		t.Fatalf("mock.AddImage: %v", err)
	}

	triggerScan(t, ctx, v)

	got := pickReconciliationLines(logBuf.lines(t))
	if len(got) != 1 {
		t.Fatalf("got %d reconciliation lines, want 1: %+v", len(got), got)
	}
	rec := got[0]
	if rec["msg"] != "storage_image_orphan_on_agent" {
		t.Errorf("msg = %v, want storage_image_orphan_on_agent", rec["msg"])
	}
	if rec["sha256"] != orphanChecksum {
		t.Errorf("sha256 = %v, want %s", rec["sha256"], orphanChecksum)
	}
	if size, _ := rec["size_bytes"].(float64); int64(size) != orphanSize {
		t.Errorf("size_bytes = %v, want %d", rec["size_bytes"], orphanSize)
	}
	if rec["pool_id"] != v.pool.ID.String() {
		t.Errorf("pool_id = %v, want %s", rec["pool_id"], v.pool.ID)
	}
	if path, _ := rec["agent_path"].(string); !strings.Contains(path, orphanChecksum) {
		t.Errorf("agent_path = %v, want to contain %s", rec["agent_path"], orphanChecksum)
	}
}

// TestStorageImageReconciliation_VerticalSliceMissing: a CP-side
// storage_images row whose checksum is absent from the mock-agent's
// inventory triggers a `storage_image_missing_on_agent` WARN line
// carrying image_id, sha256, and pool_id.
func TestStorageImageReconciliation_VerticalSliceMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logBuf := &reconciliationLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v := newVerticalSliceWithLogger(t, ctx, fastAgentClientCfg(), log)

	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-recon-missing", 0xaa)
	missingChecksum := hexChecksum(tpl.ImageChecksumSha256)
	imageID := uuid.New()
	if _, err := v.store.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             imageID,
		TemplateID:     tpl.ID,
		PoolID:         v.pool.ID,
		ChecksumSha256: missingChecksum,
		SizeBytes:      4096,
		Format:         "qcow2",
	}); err != nil {
		t.Fatalf("CreateStorageImage: %v", err)
	}

	triggerScan(t, ctx, v)

	got := pickReconciliationLines(logBuf.lines(t))
	if len(got) != 1 {
		t.Fatalf("got %d reconciliation lines, want 1: %+v", len(got), got)
	}
	rec := got[0]
	if rec["msg"] != "storage_image_missing_on_agent" {
		t.Errorf("msg = %v, want storage_image_missing_on_agent", rec["msg"])
	}
	if rec["sha256"] != missingChecksum {
		t.Errorf("sha256 = %v, want %s", rec["sha256"], missingChecksum)
	}
	if rec["image_id"] != imageID.String() {
		t.Errorf("image_id = %v, want %s", rec["image_id"], imageID)
	}
	if rec["pool_id"] != v.pool.ID.String() {
		t.Errorf("pool_id = %v, want %s", rec["pool_id"], v.pool.ID)
	}
}
