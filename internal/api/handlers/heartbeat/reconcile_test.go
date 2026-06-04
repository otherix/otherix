// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// reconcileStoreFake is a hand-rolled ReconcileStore: it promotes a fixed set
// of rows and returns empty unreachable/gone sets so the test can assert the
// promotion -> onReady seam in isolation.
type reconcileStoreFake struct {
	promoted []store.PromoteHealthyNodesRow
}

func (f *reconcileStoreFake) PromoteHealthyNodes(ctx context.Context, freshAfter time.Time) ([]store.PromoteHealthyNodesRow, error) {
	return f.promoted, nil
}

func (f *reconcileStoreFake) MarkNodesUnreachable(ctx context.Context, staleBefore time.Time) ([]store.MarkNodesUnreachableRow, error) {
	return nil, nil
}

func (f *reconcileStoreFake) MarkNodesGone(ctx context.Context, goneBefore time.Time) ([]store.MarkNodesGoneRow, error) {
	return nil, nil
}

func reconcileTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReconcileFunc_InvokesOnReadyWithPromoted(t *testing.T) {
	row := store.PromoteHealthyNodesRow{ID: uuid.New(), Name: "node-a"}
	st := &reconcileStoreFake{promoted: []store.PromoteHealthyNodesRow{row}}

	var seen []store.PromoteHealthyNodesRow
	spy := func(ctx context.Context, ready []store.PromoteHealthyNodesRow) error {
		seen = ready
		return nil
	}

	fn := ReconcileFunc(st, ReconcileConfig{}, reconcileTestLogger(), spy)
	if err := fn(context.Background()); err != nil {
		t.Fatalf("ReconcileFunc returned error: %v", err)
	}

	if diff := cmp.Diff([]store.PromoteHealthyNodesRow{row}, seen); diff != "" {
		t.Errorf("onReady received wrong rows (-want +got):\n%s", diff)
	}
}

func TestReconcileFunc_OnReadyErrorDoesNotFailReconcile(t *testing.T) {
	row := store.PromoteHealthyNodesRow{ID: uuid.New(), Name: "node-a"}
	st := &reconcileStoreFake{promoted: []store.PromoteHealthyNodesRow{row}}

	failing := func(ctx context.Context, ready []store.PromoteHealthyNodesRow) error {
		return errors.New("provisioning hiccup")
	}

	fn := ReconcileFunc(st, ReconcileConfig{}, reconcileTestLogger(), failing)
	if err := fn(context.Background()); err != nil {
		t.Fatalf("ReconcileFunc returned error from best-effort hook: %v", err)
	}
}

func TestReconcileFunc_NilOnReadyIsValid(t *testing.T) {
	st := &reconcileStoreFake{promoted: []store.PromoteHealthyNodesRow{{ID: uuid.New(), Name: "node-a"}}}

	fn := ReconcileFunc(st, ReconcileConfig{}, reconcileTestLogger(), nil)
	if err := fn(context.Background()); err != nil {
		t.Fatalf("ReconcileFunc with nil hook returned error: %v", err)
	}
}
