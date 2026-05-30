// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"testing"

	"github.com/otherix/otherix/internal/queue"
)

func TestInTxEnqueueWithoutBinderErrors(t *testing.T) {
	var s Store // no pool, no binder
	err := s.InTxEnqueue(context.Background(), func(*Queries, queue.Enqueuer) error {
		t.Fatal("fn must not run when no queue binder is configured")
		return nil
	})
	if err == nil {
		t.Fatal("InTxEnqueue with no binder = nil error, want non-nil")
	}
}
