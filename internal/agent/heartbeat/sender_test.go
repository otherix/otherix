// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type stubCollector struct {
	report Report
	err    error
	count  atomic.Int64
}

func (s *stubCollector) Collect(_ context.Context) (Report, error) {
	s.count.Add(1)
	return s.report, s.err
}

type stubPoster struct {
	status   int
	err      error
	response *Response
	count    atomic.Int64
}

func (s *stubPoster) Send(_ context.Context, _ Report) (int, *Response, error) {
	s.count.Add(1)
	return s.status, s.response, s.err
}

// TestSender_FirstTickFiresImmediately confirms the loop does NOT
// wait a full interval before its first POST. Operators expect node
// promotion to ready inside seconds of agent startup, not minutes.
func TestSender_FirstTickFiresImmediately(t *testing.T) {
	collector := &stubCollector{report: Report{AgentVersion: "test"}, err: nil}
	poster := &stubPoster{status: 200}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := NewSender(collector, poster, nil, SenderConfig{Interval: time.Hour}, logger)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for collector.count.Load() < 1 || poster.count.Load() < 1 {
		select {
		case <-deadline:
			t.Fatalf("first tick did not fire: collect=%d post=%d", collector.count.Load(), poster.count.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	<-done
}

// TestSender_ContinuesAfterPostError confirms the loop does not bail
// when the POST fails. Heartbeat is fire-and-forget from the agent's
// side; transient CP unavailability must not crash the agent.
func TestSender_ContinuesAfterPostError(t *testing.T) {
	collector := &stubCollector{report: Report{AgentVersion: "test"}}
	poster := &stubPoster{err: errors.New("boom")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := NewSender(collector, poster, nil, SenderConfig{Interval: 10 * time.Millisecond}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want context.DeadlineExceeded", err)
	}
	if poster.count.Load() < 2 {
		t.Errorf("expected at least two posts despite errors, got %d", poster.count.Load())
	}
}

// TestSender_SkipsPostOnCollectError confirms a failed collect does
// not trigger a POST with stale data.
func TestSender_SkipsPostOnCollectError(t *testing.T) {
	collector := &stubCollector{err: errors.New("nope")}
	poster := &stubPoster{status: 200}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := NewSender(collector, poster, nil, SenderConfig{Interval: 10 * time.Millisecond}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	_ = s.Run(ctx)
	if poster.count.Load() != 0 {
		t.Errorf("expected zero posts when collect failed, got %d", poster.count.Load())
	}
}
