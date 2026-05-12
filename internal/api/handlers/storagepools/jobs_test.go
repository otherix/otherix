// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/store"
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
	// json.Number-like float because Unmarshal-to-any uses float64; cast
	// for the single equality check we do care about.
	if cap, _ := got["capacity_bytes"].(float64); int64(cap) != 1<<40 {
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

// captureLogger returns a *slog.Logger that writes JSON-encoded
// records into the supplied buffer at LevelDebug. Mirrors the pattern
// used in middleware/recovery_test.go and middleware/timeout_test.go.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// decodeLogLines parses each line of buf as a JSON object and returns
// the decoded slice. Useful for asserting both the count and the
// structured fields of WARN log entries.
func decodeLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'}) {
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

func TestDiffAndLogInventory(t *testing.T) {
	t.Parallel()

	poolID := uuid.New()
	imageBID := uuid.New()
	imageAID := uuid.New()

	rowsA := []store.StorageImage{
		{ID: imageAID, PoolID: poolID, ChecksumSha256: "A"},
	}
	rowsAB := []store.StorageImage{
		{ID: imageAID, PoolID: poolID, ChecksumSha256: "A"},
		{ID: imageBID, PoolID: poolID, ChecksumSha256: "B"},
	}
	invA := []agentapi.CachedImage{{ChecksumSha256: "A", SizeBytes: 100, Path: "/p/A.qcow2"}}
	invAZ := []agentapi.CachedImage{
		{ChecksumSha256: "A", SizeBytes: 100, Path: "/p/A.qcow2"},
		{ChecksumSha256: "Z", SizeBytes: 200, Path: "/p/Z.qcow2"},
	}

	cases := []struct {
		name      string
		inventory []agentapi.CachedImage
		rows      []store.StorageImage
		// wantOrphans / wantMissing are sha256 sets we expect to see
		// surfaced; ordering is map-iteration-dependent so use sets.
		wantOrphans map[string]bool
		wantMissing map[string]bool
	}{
		{
			name:      "clean state — agent and CP agree",
			inventory: invA,
			rows:      rowsA,
		},
		{
			name:      "empty pool — both sides empty",
			inventory: nil,
			rows:      nil,
		},
		{
			name:        "orphan only — Z on agent, not in CP",
			inventory:   invAZ,
			rows:        rowsA,
			wantOrphans: map[string]bool{"Z": true},
		},
		{
			name:        "missing only — B in CP, not on agent",
			inventory:   invA,
			rows:        rowsAB,
			wantMissing: map[string]bool{"B": true},
		},
		{
			name:        "mixed — Z orphan AND B missing",
			inventory:   invAZ,
			rows:        rowsAB,
			wantOrphans: map[string]bool{"Z": true},
			wantMissing: map[string]bool{"B": true},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			diffAndLogInventory(context.Background(), captureLogger(&buf), poolID, tc.inventory, tc.rows)

			gotOrphans := map[string]bool{}
			gotMissing := map[string]bool{}
			for _, rec := range decodeLogLines(t, &buf) {
				msg, _ := rec["msg"].(string)
				sha, _ := rec["sha256"].(string)
				switch msg {
				case "storage_image_orphan_on_agent":
					gotOrphans[sha] = true
				case "storage_image_missing_on_agent":
					gotMissing[sha] = true
				default:
					t.Errorf("unexpected log msg %q in record %v", msg, rec)
				}
			}
			if !equalSet(gotOrphans, tc.wantOrphans) {
				t.Errorf("orphans = %v, want %v", gotOrphans, tc.wantOrphans)
			}
			if !equalSet(gotMissing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", gotMissing, tc.wantMissing)
			}
		})
	}
}

func TestDiffAndLogInventory_OrphanCarriesStructuredFields(t *testing.T) {
	t.Parallel()

	poolID := uuid.New()
	var buf bytes.Buffer
	diffAndLogInventory(context.Background(), captureLogger(&buf), poolID,
		[]agentapi.CachedImage{{ChecksumSha256: "Z", SizeBytes: 4096, Path: "/var/pools/p/Z.qcow2"}},
		nil)

	recs := decodeLogLines(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d log lines, want 1", len(recs))
	}
	rec := recs[0]
	if rec["msg"] != "storage_image_orphan_on_agent" {
		t.Errorf("msg = %v, want storage_image_orphan_on_agent", rec["msg"])
	}
	if rec["sha256"] != "Z" {
		t.Errorf("sha256 = %v, want Z", rec["sha256"])
	}
	if rec["agent_path"] != "/var/pools/p/Z.qcow2" {
		t.Errorf("agent_path = %v, want /var/pools/p/Z.qcow2", rec["agent_path"])
	}
	if rec["pool_id"] != poolID.String() {
		t.Errorf("pool_id = %v, want %s", rec["pool_id"], poolID)
	}
	// JSON numbers come through as float64; size_bytes was 4096.
	if size, _ := rec["size_bytes"].(float64); int64(size) != 4096 {
		t.Errorf("size_bytes = %v, want 4096", rec["size_bytes"])
	}
}

func TestDiffAndLogInventory_MissingCarriesStructuredFields(t *testing.T) {
	t.Parallel()

	poolID := uuid.New()
	imageID := uuid.New()
	var buf bytes.Buffer
	diffAndLogInventory(context.Background(), captureLogger(&buf), poolID, nil,
		[]store.StorageImage{{ID: imageID, PoolID: poolID, ChecksumSha256: "B"}})

	recs := decodeLogLines(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d log lines, want 1", len(recs))
	}
	rec := recs[0]
	if rec["msg"] != "storage_image_missing_on_agent" {
		t.Errorf("msg = %v, want storage_image_missing_on_agent", rec["msg"])
	}
	if rec["sha256"] != "B" {
		t.Errorf("sha256 = %v, want B", rec["sha256"])
	}
	if rec["image_id"] != imageID.String() {
		t.Errorf("image_id = %v, want %s", rec["image_id"], imageID)
	}
	if rec["pool_id"] != poolID.String() {
		t.Errorf("pool_id = %v, want %s", rec["pool_id"], poolID)
	}
}

// errLister is the in-package imageLister fake used to inject a
// listing failure into reconcileInventory. Mirrors the
// "consumer-defines" interface convention used by agent_executor.go's
// agentClient fake.
type errLister struct{ err error }

func (e errLister) ListImages(_ context.Context, _ string, _ string) ([]agentapi.CachedImage, error) {
	return nil, e.err
}

func TestReconcileInventory_ReconcilerErrorLogsAndReturns(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := &StoragePoolScanWorker{
		// store nil intentionally — the failure path returns before
		// touching the store. Asserts the diagnostic stays diagnostic.
		lister: errLister{err: errors.New("agent unreachable")},
		logger: captureLogger(&buf),
	}
	w.reconcileInventory(context.Background(), uuid.New(), "test-pool", "https://node.test")

	recs := decodeLogLines(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d log lines, want exactly 1", len(recs))
	}
	rec := recs[0]
	if rec["msg"] != "scan_reconciliation_failed" {
		t.Errorf("msg = %v, want scan_reconciliation_failed", rec["msg"])
	}
	if got, _ := rec["error"].(string); got != "agent unreachable" {
		t.Errorf("error = %v, want agent unreachable", rec["error"])
	}
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
}

// equalSet compares two string-keyed boolean sets ignoring nil-vs-empty.
func equalSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
