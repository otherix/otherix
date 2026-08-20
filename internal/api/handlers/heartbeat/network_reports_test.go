// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// upsertNetworkStatusSpy records every UpsertNetworkNodeStatus call.
// It embeds store.HeartbeatProjection (left nil) so it satisfies the
// interface while implementing only the one method applyNetworkReports
// exercises; any other call would panic, which is the desired failure
// mode for an unexpected projection step in these tests.
type upsertNetworkStatusSpy struct {
	store.HeartbeatProjection
	calls   []store.UpsertNetworkNodeStatusParams
	failNth int // 1-based call index to fail; 0 disables.
}

func (s *upsertNetworkStatusSpy) UpsertNetworkNodeStatus(_ context.Context, arg store.UpsertNetworkNodeStatusParams) error {
	s.calls = append(s.calls, arg)
	if s.failNth != 0 && len(s.calls) == s.failNth {
		return errors.New("boom")
	}
	return nil
}

func newQuietHandler() *Handler {
	return &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestApplyNetworkReportsTolerance verifies the up-channel ingest:
// valid uuid ids produce one UpsertNetworkNodeStatus call each carrying
// the status + error, and a non-uuid id is skipped without failing the
// heartbeat (no call, no error returned).
func TestApplyNetworkReportsTolerance(t *testing.T) {
	nodeID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	readyID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	failedID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	errMsg := "bridge create failed"

	reports := []networkReport{
		{ID: readyID.String(), ReconciliationStatus: "ready"},
		{ID: "not-a-uuid", ReconciliationStatus: "ready"},
		{ID: failedID.String(), ReconciliationStatus: "failed", ReconciliationError: &errMsg},
	}

	spy := &upsertNetworkStatusSpy{}
	h := newQuietHandler()
	if err := h.applyNetworkReports(context.Background(), spy, nodeID, reports); err != nil {
		t.Fatalf("applyNetworkReports returned error: %v", err)
	}

	want := []store.UpsertNetworkNodeStatusParams{
		{NetworkID: readyID, NodeID: nodeID, ReconciliationStatus: "ready"},
		{NetworkID: failedID, NodeID: nodeID, ReconciliationStatus: "failed", ReconciliationError: &errMsg},
	}
	if diff := cmp.Diff(want, spy.calls); diff != "" {
		t.Errorf("UpsertNetworkNodeStatus calls mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyNetworkReportsRejectsUnknownStatus verifies a report whose
// reconciliation_status is outside the [pending,ready,failed] enum is skipped
// (no upsert, WARN-logged) without failing the heartbeat - a buggy/old agent
// must not be able to seed an out-of-enum status the CP would later serve.
func TestApplyNetworkReportsRejectsUnknownStatus(t *testing.T) {
	nodeID := uuid.New()
	goodID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	bogusID := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	reports := []networkReport{
		{ID: bogusID.String(), ReconciliationStatus: "bogus"},
		{ID: goodID.String(), ReconciliationStatus: "ready"},
	}

	spy := &upsertNetworkStatusSpy{}
	h := newQuietHandler()
	if err := h.applyNetworkReports(context.Background(), spy, nodeID, reports); err != nil {
		t.Fatalf("applyNetworkReports returned error: %v", err)
	}

	want := []store.UpsertNetworkNodeStatusParams{
		{NetworkID: goodID, NodeID: nodeID, ReconciliationStatus: "ready"},
	}
	if diff := cmp.Diff(want, spy.calls); diff != "" {
		t.Errorf("UpsertNetworkNodeStatus calls mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyNetworkReportsStoreFailure verifies a genuine store failure
// propagates (so the projection transaction rolls back), unlike a bad id
// which is tolerated.
func TestApplyNetworkReportsStoreFailure(t *testing.T) {
	nodeID := uuid.New()
	spy := &upsertNetworkStatusSpy{failNth: 1}
	h := newQuietHandler()
	reports := []networkReport{{ID: uuid.NewString(), ReconciliationStatus: "ready"}}
	if err := h.applyNetworkReports(context.Background(), spy, nodeID, reports); err == nil {
		t.Errorf("applyNetworkReports = nil, want error on store failure")
	}
}

// VMSoftDeleted answers "not deleted" for every id: these tests do not drive
// the heartbeat teardown path. A test that does asserts on its own answer.
func (s *upsertNetworkStatusSpy) VMSoftDeleted(context.Context, uuid.UUID) (bool, string, error) {
	return false, "", nil
}
