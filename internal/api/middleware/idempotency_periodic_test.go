// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"errors"
	"testing"
)

// stubCleaner counts calls to each sweep and returns the configured results, so
// a test can assert both sweeps run and that a first-sweep error does not skip
// the second.
type stubCleaner struct {
	keysCalls, indexCalls int
	keysN, indexN         int64
	keysErr, indexErr     error
}

func (s *stubCleaner) DeleteExpiredIdempotencyKeys(context.Context) (int64, error) {
	s.keysCalls++
	return s.keysN, s.keysErr
}

func (s *stubCleaner) DeleteExpiredIdempotencyTaskIndex(context.Context) (int64, error) {
	s.indexCalls++
	return s.indexN, s.indexErr
}

func TestIdempotencyCleanupFunc_RunsBothSweeps(t *testing.T) {
	c := &stubCleaner{keysN: 2, indexN: 3}
	if err := IdempotencyCleanupFunc(c, discardLog())(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if c.keysCalls != 1 {
		t.Errorf("DeleteExpiredIdempotencyKeys calls = %d, want 1", c.keysCalls)
	}
	if c.indexCalls != 1 {
		t.Errorf("DeleteExpiredIdempotencyTaskIndex calls = %d, want 1", c.indexCalls)
	}
}

func TestIdempotencyCleanupFunc_FirstErrorStillRunsSecond(t *testing.T) {
	keysErr := errors.New("keys sweep failed")
	indexErr := errors.New("index sweep failed")
	c := &stubCleaner{keysErr: keysErr, indexErr: indexErr}

	err := IdempotencyCleanupFunc(c, discardLog())(context.Background())
	if c.keysCalls != 1 || c.indexCalls != 1 {
		t.Fatalf("calls = (keys %d, index %d), want (1, 1): a failing first sweep must not skip the second", c.keysCalls, c.indexCalls)
	}
	if !errors.Is(err, keysErr) {
		t.Errorf("err does not carry the keys-sweep error: %v", err)
	}
	if !errors.Is(err, indexErr) {
		t.Errorf("err does not carry the index-sweep error: %v", err)
	}
}
