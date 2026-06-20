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
	gotCutoff         time.Time
	gotStrandedCutoff time.Time
	deleted           int
	stranded          int
	err               error
	strandedErr       error
}

func (f *retentionStoreFake) DeleteExpiredPullSagas(_ context.Context, cutoff time.Time) (int, error) {
	f.gotCutoff = cutoff
	return f.deleted, f.err
}

func (f *retentionStoreFake) DeleteStrandedPullSagas(_ context.Context, cutoff time.Time) (int, error) {
	f.gotStrandedCutoff = cutoff
	return f.stranded, f.strandedErr
}

func TestSagaRetentionFuncComputesCutoff(t *testing.T) {
	f := &retentionStoreFake{deleted: 3, stranded: 2}
	fn := SagaRetentionFunc(f, 24*time.Hour, 48*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	beforeTerminal := time.Now().Add(-24 * time.Hour)
	beforeStranded := time.Now().Add(-48 * time.Hour)
	if err := fn(context.Background()); err != nil {
		t.Fatalf("retention fn = %v", err)
	}
	afterTerminal := time.Now().Add(-24 * time.Hour)
	afterStranded := time.Now().Add(-48 * time.Hour)
	if f.gotCutoff.Before(beforeTerminal.Add(-time.Minute)) || f.gotCutoff.After(afterTerminal.Add(time.Minute)) {
		t.Errorf("terminal cutoff = %v, want ~now-24h", f.gotCutoff)
	}
	if f.gotStrandedCutoff.Before(beforeStranded.Add(-time.Minute)) || f.gotStrandedCutoff.After(afterStranded.Add(time.Minute)) {
		t.Errorf("stranded cutoff = %v, want ~now-48h", f.gotStrandedCutoff)
	}
}

func TestSagaRetentionFuncPropagatesError(t *testing.T) {
	f := &retentionStoreFake{err: errors.New("boom")}
	fn := SagaRetentionFunc(f, time.Hour, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := fn(context.Background()); err == nil {
		t.Errorf("retention fn = nil, want error propagated")
	}
}

func TestSagaRetentionFuncPropagatesStrandedError(t *testing.T) {
	f := &retentionStoreFake{strandedErr: errors.New("boom")}
	fn := SagaRetentionFunc(f, time.Hour, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := fn(context.Background()); err == nil {
		t.Errorf("retention fn = nil, want stranded error propagated")
	}
}
