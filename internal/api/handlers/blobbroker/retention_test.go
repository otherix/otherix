// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobbroker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type retentionStoreFake struct {
	gotCutoff time.Time
	deleted   int
	err       error
}

func (f *retentionStoreFake) DeleteExpiredPullSagas(_ context.Context, cutoff time.Time) (int, error) {
	f.gotCutoff = cutoff
	return f.deleted, f.err
}

func TestSagaRetentionFuncComputesCutoff(t *testing.T) {
	f := &retentionStoreFake{deleted: 3}
	fn := SagaRetentionFunc(f, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := time.Now().Add(-24 * time.Hour)
	if err := fn(context.Background()); err != nil {
		t.Fatalf("retention fn = %v", err)
	}
	after := time.Now().Add(-24 * time.Hour)
	if f.gotCutoff.Before(before.Add(-time.Minute)) || f.gotCutoff.After(after.Add(time.Minute)) {
		t.Errorf("cutoff = %v, want ~now-24h", f.gotCutoff)
	}
}

func TestSagaRetentionFuncPropagatesError(t *testing.T) {
	f := &retentionStoreFake{err: errors.New("boom")}
	fn := SagaRetentionFunc(f, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := fn(context.Background()); err == nil {
		t.Errorf("retention fn = nil, want error propagated")
	}
}
