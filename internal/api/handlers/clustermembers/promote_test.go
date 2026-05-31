// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package clustermembers

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// fakePromoter is a Promoter test fake that counts TryPromoteLearners calls.
type fakePromoter struct {
	calls atomic.Int32
}

func (f *fakePromoter) TryPromoteLearners(_ context.Context) (int, error) {
	f.calls.Add(1)
	return 0, nil
}

func TestRunPromoteLoopTicks(t *testing.T) {
	p := &fakePromoter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunPromoteLoop(ctx, p, 10*time.Millisecond, log)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// goroutine returned cleanly
	case <-time.After(1 * time.Second):
		t.Fatal("RunPromoteLoop() did not return within 1s after context cancel")
	}

	if got := p.calls.Load(); got == 0 {
		t.Errorf("RunPromoteLoop() calls = %d, want > 0", got)
	}
}
