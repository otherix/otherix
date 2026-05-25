// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"log/slog"
	"time"
)

// Poster is the narrow Client surface the Sender depends on. The
// production *Client satisfies it; tests pass a fake to drive the
// loop without spinning up a real CP server.
type Poster interface {
	Send(ctx context.Context, report Report) (int, *Response, error)
}

// ResponseHandler is the optional sink for heartbeat responses. The
// pool reconciler implements it and receives the desired-pool inventory
// every tick. nil handler means the loop runs without downstream
// notification (used by collector-only test paths).
type ResponseHandler interface {
	HandleHeartbeatResponse(ctx context.Context, resp *Response)
}

// MultiResponseHandler fans the heartbeat response to multiple
// reconcilers (e.g. pool + VM). Nil entries are skipped so callers
// can wire optional reconcilers without a nil-check dance.
type MultiResponseHandler []ResponseHandler

// HandleHeartbeatResponse implements ResponseHandler by dispatching to
// every non-nil delegate. Errors are not aggregated — each delegate
// reports its own (or swallows) through its own logger.
func (m MultiResponseHandler) HandleHeartbeatResponse(ctx context.Context, resp *Response) {
	for _, h := range m {
		if h != nil {
			h.HandleHeartbeatResponse(ctx, resp)
		}
	}
}

// SenderConfig pins the ticker cadence. Zero falls back to 30 s.
type SenderConfig struct {
	Interval time.Duration
}

// Sender runs the heartbeat loop. One instance per agent process;
// the agent runtime owns the goroutine lifecycle and cancels the
// context to stop the loop.
type Sender struct {
	collector       Collector
	poster          Poster
	responseHandler ResponseHandler
	interval        time.Duration
	logger          *slog.Logger
}

// NewSender constructs a Sender. Collector / poster / logger are
// required; responseHandler is optional (nil disables response
// dispatch). Zero Interval defaults to 30 s.
func NewSender(collector Collector, poster Poster, handler ResponseHandler, cfg SenderConfig, logger *slog.Logger) *Sender {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Sender{
		collector:       collector,
		poster:          poster,
		responseHandler: handler,
		interval:        interval,
		logger:          logger,
	}
}

// Run blocks until ctx is cancelled. First tick fires immediately so
// a fresh agent flips the node to 'ready' without waiting a full
// interval. Subsequent ticks fire every Interval.
//
// Errors (collect or post) are logged at WARN and the loop continues;
// the heartbeat protocol is fire-and-forget per design (the CP
// reconciler closes the loop by marking stale nodes unreachable).
func (s *Sender) Run(ctx context.Context) error {
	s.tick(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick runs one collect+post cycle. Return value is dropped — the
// caller (Run) only cares about ctx cancellation, not per-tick
// failures.
func (s *Sender) tick(ctx context.Context) {
	report, err := s.collector.Collect(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "heartbeat collect failed", slog.String("error", err.Error()))
		return
	}
	status, response, err := s.poster.Send(ctx, report)
	if err != nil {
		// ctx-cancelled errors during shutdown are noise; suppress them.
		if ctx.Err() != nil {
			return
		}
		s.logger.WarnContext(ctx, "heartbeat post failed",
			slog.String("error", err.Error()),
			slog.Int("status", status),
		)
		return
	}
	if s.responseHandler != nil && response != nil {
		s.responseHandler.HandleHeartbeatResponse(ctx, response)
	}
	declaredPools := 0
	declaredVMs := 0
	if response != nil {
		declaredPools = len(response.DeclaredPools)
		declaredVMs = len(response.DeclaredVMs)
	}
	s.logger.DebugContext(ctx, "heartbeat sent",
		slog.Int("status", status),
		slog.Int("vms", len(report.VMs)),
		slog.Int("pools_reported", len(report.Pools)),
		slog.Int("pools_declared", declaredPools),
		slog.Int("vms_declared", declaredVMs),
		slog.Int("cpu_cores_available", int(report.Resources.CPUCoresAvailable)),
		slog.Int64("memory_available_mib", report.Resources.MemoryAvailableMib),
	)
}
