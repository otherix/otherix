// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// poolReportsSpy records the pool-image inventory writes applyPoolReports makes
// and stubs the (node_id, name) -> id resolver. It embeds
// store.HeartbeatProjection (left nil) so it satisfies the interface while
// implementing only the methods applyPoolReports exercises; any other call would
// panic, the desired failure mode for an unexpected projection step.
type poolReportsSpy struct {
	store.HeartbeatProjection
	// resolve maps lower(name) -> (id, found).
	resolve map[string]uuid.UUID
	// reconciliations records UpdateStoragePoolReconciliation calls.
	reconciliations []store.UpdateStoragePoolReconciliationParams
	// inventories records UpsertPoolImageInventory calls in order.
	inventories []poolInventoryCall
	// failInventory makes UpsertPoolImageInventory return an error.
	failInventory bool
	// failResolve makes StoragePoolIDByNodeName return an error.
	failResolve bool
}

type poolInventoryCall struct {
	poolID uuid.UUID
	images []store.PoolImage
}

func (s *poolReportsSpy) UpdateStoragePoolReconciliation(_ context.Context, arg store.UpdateStoragePoolReconciliationParams) error {
	s.reconciliations = append(s.reconciliations, arg)
	return nil
}

func (s *poolReportsSpy) StoragePoolIDByNodeName(_ context.Context, _ uuid.UUID, name string) (uuid.UUID, bool, error) {
	if s.failResolve {
		return uuid.Nil, false, errors.New("boom")
	}
	// Mirror the real store, which resolves on lower(name).
	id, ok := s.resolve[strings.ToLower(name)]
	return id, ok, nil
}

func (s *poolReportsSpy) UpsertPoolImageInventory(_ context.Context, poolID uuid.UUID, images []store.PoolImage) error {
	s.inventories = append(s.inventories, poolInventoryCall{poolID: poolID, images: images})
	if s.failInventory {
		return errors.New("boom")
	}
	return nil
}

// TestApplyPoolReportsImageInventory verifies the consumer side of the heartbeat
// inventory seam: a reported pool's images are parsed into []store.PoolImage and
// written through UpsertPoolImageInventory, keyed by the pool's resolved UUID.
func TestApplyPoolReportsImageInventory(t *testing.T) {
	nodeID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	poolID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	reports := []poolReport{{
		Name:                 "Pool-A",
		ReconciliationStatus: "ready",
		Images: []poolImageReport{{
			Basename:         "noble.qcow2",
			SHA256:           "abc123",
			SizeBytes:        100,
			VirtualSizeBytes: 200,
			Format:           "qcow2",
			ImportedAt:       "2026-06-05T10:00:00Z",
		}},
	}}

	spy := &poolReportsSpy{resolve: map[string]uuid.UUID{"pool-a": poolID}}
	h := newQuietHandler()
	if err := h.applyPoolReports(context.Background(), spy, nodeID, reports); err != nil {
		t.Fatalf("applyPoolReports returned error: %v", err)
	}

	wantImported, _ := time.Parse(time.RFC3339Nano, "2026-06-05T10:00:00Z")
	want := []poolInventoryCall{{
		poolID: poolID,
		images: []store.PoolImage{{
			Basename:         "noble.qcow2",
			ChecksumSha256:   "abc123",
			SizeBytes:        100,
			VirtualSizeBytes: 200,
			Format:           "qcow2",
			ImportedAt:       wantImported.UTC(),
		}},
	}}
	if diff := cmp.Diff(want, spy.inventories, cmp.AllowUnexported(poolInventoryCall{})); diff != "" {
		t.Errorf("UpsertPoolImageInventory calls mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyPoolReportsUnknownPoolSkipsInventory verifies a pool the CP no longer
// has (resolver returns found=false, e.g. operator deleted it mid-tick) skips
// the inventory write entirely without failing the heartbeat.
func TestApplyPoolReportsUnknownPoolSkipsInventory(t *testing.T) {
	nodeID := uuid.New()
	reports := []poolReport{{
		Name:                 "ghost",
		ReconciliationStatus: "ready",
		Images:               []poolImageReport{{Basename: "x.qcow2", SHA256: "h"}},
	}}

	spy := &poolReportsSpy{resolve: map[string]uuid.UUID{}}
	h := newQuietHandler()
	if err := h.applyPoolReports(context.Background(), spy, nodeID, reports); err != nil {
		t.Fatalf("applyPoolReports returned error: %v", err)
	}
	if len(spy.inventories) != 0 {
		t.Errorf("inventory writes = %d, want 0 for unresolved pool", len(spy.inventories))
	}
}

// TestApplyPoolReportsTolerantImportedAt verifies a malformed imported_at
// degrades to zero time (the image is still stored - identity is basename+sha,
// not the timestamp) and does not fail the heartbeat.
func TestApplyPoolReportsTolerantImportedAt(t *testing.T) {
	nodeID := uuid.New()
	poolID := uuid.New()
	reports := []poolReport{{
		Name:                 "p",
		ReconciliationStatus: "ready",
		Images: []poolImageReport{{
			Basename:   "x.qcow2",
			SHA256:     "h",
			ImportedAt: "not-a-timestamp",
		}},
	}}

	spy := &poolReportsSpy{resolve: map[string]uuid.UUID{"p": poolID}}
	h := newQuietHandler()
	if err := h.applyPoolReports(context.Background(), spy, nodeID, reports); err != nil {
		t.Fatalf("applyPoolReports returned error: %v", err)
	}
	if len(spy.inventories) != 1 || len(spy.inventories[0].images) != 1 {
		t.Fatalf("inventories = %+v, want one image stored", spy.inventories)
	}
	if got := spy.inventories[0].images[0]; !got.ImportedAt.IsZero() || got.Basename != "x.qcow2" {
		t.Errorf("image = %+v, want basename x.qcow2 with zero ImportedAt", got)
	}
}

// TestApplyPoolReportsInventoryStoreFailure verifies a genuine store failure on
// the inventory write propagates so the projection transaction rolls back,
// matching the surrounding applyPoolReports error posture (reconciliation
// failures already propagate).
func TestApplyPoolReportsInventoryStoreFailure(t *testing.T) {
	nodeID := uuid.New()
	poolID := uuid.New()
	reports := []poolReport{{
		Name:                 "p",
		ReconciliationStatus: "ready",
		Images:               []poolImageReport{{Basename: "x.qcow2", SHA256: "h"}},
	}}
	spy := &poolReportsSpy{resolve: map[string]uuid.UUID{"p": poolID}, failInventory: true}
	h := newQuietHandler()
	if err := h.applyPoolReports(context.Background(), spy, nodeID, reports); err == nil {
		t.Errorf("applyPoolReports = nil, want error on inventory store failure")
	}
}

// VMSoftDeleted answers "not deleted" for every id: these tests do not drive
// the heartbeat teardown path. A test that does asserts on its own answer.
func (s *poolReportsSpy) VMSoftDeleted(context.Context, uuid.UUID) (bool, string, error) {
	return false, "", nil
}
