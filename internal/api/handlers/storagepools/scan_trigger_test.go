// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestScanTriggerArgs_Kind(t *testing.T) {
	t.Parallel()
	if got, want := (ScanTriggerArgs{}).Kind(), "storage_pool.scan_trigger"; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
}

func TestScanTriggerConfig_WithDefaults(t *testing.T) {
	t.Parallel()
	got := ScanTriggerConfig{}.withDefaults()
	if got.Interval != defaultScanTriggerInterval {
		t.Errorf("Interval = %s, want %s", got.Interval, defaultScanTriggerInterval)
	}
	if got.Jitter != defaultScanTriggerJitter {
		t.Errorf("Jitter = %s, want %s", got.Jitter, defaultScanTriggerJitter)
	}

	custom := ScanTriggerConfig{Interval: 5 * time.Minute, Jitter: 10 * time.Second}.withDefaults()
	if custom.Interval != 5*time.Minute || custom.Jitter != 10*time.Second {
		t.Errorf("custom values clobbered: %+v", custom)
	}
}

type fakeTriggerStore struct {
	rows []store.ListPoolsNeedingScanRow
	err  error
}

func (f *fakeTriggerStore) ListPoolsNeedingScan(_ context.Context) ([]store.ListPoolsNeedingScanRow, error) {
	return f.rows, f.err
}

type recordingEnqueuer struct {
	calls  []enqueueCall
	failOn map[uuid.UUID]error // poolID -> err to return on that call
}

type enqueueCall struct {
	poolID      uuid.UUID
	scheduledAt time.Time
}

func (r *recordingEnqueuer) EnqueueScan(_ context.Context, poolID uuid.UUID, scheduledAt time.Time) error {
	r.calls = append(r.calls, enqueueCall{poolID: poolID, scheduledAt: scheduledAt})
	if err, ok := r.failOn[poolID]; ok {
		return err
	}
	return nil
}

func newFakeRows(n int) []store.ListPoolsNeedingScanRow {
	out := make([]store.ListPoolsNeedingScanRow, n)
	for i := range out {
		out[i] = store.ListPoolsNeedingScanRow{
			ID:     uuid.New(),
			Name:   "pool-" + string(rune('a'+i)),
			NodeID: uuid.New(),
		}
	}
	return out
}

func TestRunScanTriggerTick_EnqueuesAllPools(t *testing.T) {
	t.Parallel()
	rows := newFakeRows(3)
	src := &fakeTriggerStore{rows: rows}
	enq := &recordingEnqueuer{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if err := runScanTriggerTick(context.Background(), src, enq, 100*time.Millisecond, logger); err != nil {
		t.Fatalf("runScanTriggerTick: %v", err)
	}
	if len(enq.calls) != 3 {
		t.Fatalf("enqueue calls = %d, want 3", len(enq.calls))
	}
	// Order matches store output — trigger does not resort.
	for i, c := range enq.calls {
		if c.poolID != rows[i].ID {
			t.Errorf("call %d poolID = %s, want %s", i, c.poolID, rows[i].ID)
		}
	}
	if got := buf.String(); !strings.Contains(got, "pools_eligible=3") || !strings.Contains(got, "enqueued=3") || !strings.Contains(got, "failed=0") {
		t.Errorf("info log missing expected counts: %q", got)
	}
}

func TestRunScanTriggerTick_NoPools(t *testing.T) {
	t.Parallel()
	src := &fakeTriggerStore{rows: nil}
	enq := &recordingEnqueuer{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if err := runScanTriggerTick(context.Background(), src, enq, time.Second, logger); err != nil {
		t.Fatalf("runScanTriggerTick: %v", err)
	}
	if len(enq.calls) != 0 {
		t.Errorf("expected zero enqueues, got %d", len(enq.calls))
	}
	if got := buf.String(); !strings.Contains(got, "pools_eligible=0") || !strings.Contains(got, "enqueued=0") {
		t.Errorf("info log missing zero counts: %q", got)
	}
}

func TestRunScanTriggerTick_PerPoolErrorContinues(t *testing.T) {
	t.Parallel()
	rows := newFakeRows(3)
	src := &fakeTriggerStore{rows: rows}
	boom := errors.New("river queue full")
	enq := &recordingEnqueuer{failOn: map[uuid.UUID]error{rows[1].ID: boom}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if err := runScanTriggerTick(context.Background(), src, enq, time.Second, logger); err != nil {
		t.Fatalf("runScanTriggerTick: %v", err)
	}
	if len(enq.calls) != 3 {
		t.Fatalf("expected all 3 calls (per-pool errors are logged + continue), got %d", len(enq.calls))
	}
	out := buf.String()
	if !strings.Contains(out, "scan_trigger enqueue failed") {
		t.Errorf("warn log not emitted: %q", out)
	}
	if !strings.Contains(out, rows[1].Name) {
		t.Errorf("warn log missing failing pool name %q: %q", rows[1].Name, out)
	}
	if !strings.Contains(out, "enqueued=2") || !strings.Contains(out, "failed=1") {
		t.Errorf("info log counts wrong: %q", out)
	}
}

func TestRunScanTriggerTick_ListPoolsErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection refused")
	src := &fakeTriggerStore{err: boom}
	enq := &recordingEnqueuer{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := runScanTriggerTick(context.Background(), src, enq, time.Second, logger)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, boom) && !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not wrap underlying cause", err)
	}
	if len(enq.calls) != 0 {
		t.Errorf("expected no enqueues on list error, got %d", len(enq.calls))
	}
}

func TestRunScanTriggerTick_JitterBounds(t *testing.T) {
	t.Parallel()
	const (
		numPools = 200
		jitter   = 250 * time.Millisecond
	)
	rows := newFakeRows(numPools)
	src := &fakeTriggerStore{rows: rows}
	enq := &recordingEnqueuer{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	start := time.Now()
	if err := runScanTriggerTick(context.Background(), src, enq, jitter, logger); err != nil {
		t.Fatalf("runScanTriggerTick: %v", err)
	}
	end := time.Now()

	// jitterDelay reads time.Now() once for the whole tick (captured
	// inside runScanTriggerTick), so every ScheduledAt falls into
	// [start, end + jitter].
	min := start
	max := end.Add(jitter)
	for i, c := range enq.calls {
		if c.scheduledAt.Before(min) || c.scheduledAt.After(max) {
			t.Errorf("call %d scheduledAt %s outside [%s, %s]", i, c.scheduledAt, min, max)
		}
	}

	// Sanity: with 200 pools and 250 ms jitter, at least one ScheduledAt
	// should differ from the others by more than 1 ms. Catches a silent
	// "jitter=0" regression. Spread is max(scheduledAt) - min(scheduledAt)
	// across all enqueues — a one-sided diff from calls[0] would silently
	// return 0 when calls[0] happens to draw the largest jitter (1/200
	// probability, the prior bug).
	minAt, maxAt := enq.calls[0].scheduledAt, enq.calls[0].scheduledAt
	for _, c := range enq.calls {
		if c.scheduledAt.Before(minAt) {
			minAt = c.scheduledAt
		}
		if c.scheduledAt.After(maxAt) {
			maxAt = c.scheduledAt
		}
	}
	spread := maxAt.Sub(minAt)
	if spread < time.Millisecond {
		t.Errorf("jitter spread = %s, expected > 1ms across %d calls", spread, numPools)
	}
}

func TestJitterDelay_ZeroDisablesJitter(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		if got := jitterDelay(0); got != 0 {
			t.Fatalf("jitterDelay(0) = %s, want 0", got)
		}
		if got := jitterDelay(-time.Second); got != 0 {
			t.Fatalf("jitterDelay(-1s) = %s, want 0", got)
		}
	}
}

func TestJitterDelay_InRange(t *testing.T) {
	t.Parallel()
	const max = 100 * time.Millisecond
	for i := 0; i < 1000; i++ {
		d := jitterDelay(max)
		if d < 0 || d >= max {
			t.Fatalf("jitterDelay(%s) = %s, want [0, %s)", max, d, max)
		}
	}
}
