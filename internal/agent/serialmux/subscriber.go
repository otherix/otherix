// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"errors"
	"sync"
	"sync/atomic"
)

// errSubscriberClosed is returned by tryDeliver and Write once the
// subscriber has been Close'd. The multiplexer treats this as a signal
// to drop the subscriber from its registry on the next fan-out pass.
var errSubscriberClosed = errors.New("serialmux: subscriber closed")

// errSubscriberFull is returned when the subscriber's outbound channel
// is full. Per L2 the multiplexer never blocks on a slow consumer; the
// caller increments the drop counter and continues.
var errSubscriberFull = errors.New("serialmux: subscriber channel full")

// ConsoleSubscriber is the bidirectional console attachment. Only one
// console subscriber can be active per multiplexer (spec §4.1.2,
// ADR 0028 console_in_use semantic).
//
// Bytes() exposes the receive channel for QEMU serial output; Write()
// forwards operator keystrokes back to QEMU via the multiplexer.
type ConsoleSubscriber struct {
	ch     chan []byte
	done   chan struct{}
	closed atomic.Bool

	closeOnce sync.Once
	writeFn   func([]byte) error
}

// newConsoleSubscriber constructs a console subscriber whose Write
// path forwards through writeFn. capacity bounds the outbound
// channel; the multiplexer uses defaultSubscriberCapacity, tests pin
// smaller values to exercise overflow.
func newConsoleSubscriber(writeFn func([]byte) error, capacity int) *ConsoleSubscriber {
	if capacity <= 0 {
		capacity = 1
	}
	return &ConsoleSubscriber{
		ch:      make(chan []byte, capacity),
		done:    make(chan struct{}),
		writeFn: writeFn,
	}
}

// Bytes returns the channel carrying sanitized serial output from
// QEMU. The channel is never closed by the subscriber - callers
// select on Done() to learn when to stop reading.
func (s *ConsoleSubscriber) Bytes() <-chan []byte { return s.ch }

// Done returns a channel that closes when the subscriber has been
// detached. Read paths use this to unwind in tandem with the write
// pump.
func (s *ConsoleSubscriber) Done() <-chan struct{} { return s.done }

// IsClosed reports whether Close has been called.
func (s *ConsoleSubscriber) IsClosed() bool { return s.closed.Load() }

// Write forwards data to QEMU through the multiplexer-owned write
// callback. Returns errSubscriberClosed if the subscriber has been
// detached.
func (s *ConsoleSubscriber) Write(data []byte) error {
	if s.IsClosed() {
		return errSubscriberClosed
	}
	return s.writeFn(data)
}

// Close detaches the subscriber. Idempotent: subsequent calls are
// no-ops. Future tryDeliver / Write return errSubscriberClosed.
func (s *ConsoleSubscriber) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
	})
	return nil
}

// tryDeliver enqueues data on the outbound channel without blocking.
// Returns errSubscriberClosed if the subscriber is closed, or
// errSubscriberFull if the channel is full.
func (s *ConsoleSubscriber) tryDeliver(data []byte) error {
	if s.IsClosed() {
		return errSubscriberClosed
	}
	select {
	case s.ch <- data:
		return nil
	default:
		return errSubscriberFull
	}
}

// LogsSubscriber is the read-only attachment used by `otherix vm
// logs`. Many of these can be active concurrently against a single
// multiplexer. Drop accounting lives here because the counter must
// survive transient overflow without contending on the multiplexer's
// own state lock.
type LogsSubscriber struct {
	ch     chan []byte
	done   chan struct{}
	ready  chan struct{}
	closed atomic.Bool

	dropped atomic.Uint64
	follow  bool

	closeOnce sync.Once
	readyOnce sync.Once
}

// newLogsSubscriber constructs a logs subscriber. follow controls
// whether the multiplexer keeps the subscriber attached after the
// initial tail has been streamed; capacity bounds the outbound
// channel as for the console subscriber.
func newLogsSubscriber(follow bool, capacity int) *LogsSubscriber {
	if capacity <= 0 {
		capacity = 1
	}
	return &LogsSubscriber{
		ch:     make(chan []byte, capacity),
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
		follow: follow,
	}
}

// Bytes returns the channel carrying sanitized bytes plus any
// out-of-band drop markers injected by the multiplexer.
func (s *LogsSubscriber) Bytes() <-chan []byte { return s.ch }

// Done closes when the subscriber should stop streaming - either
// because Close has been called or because the multiplexer has
// signalled end-of-history for a follow=false attachment.
func (s *LogsSubscriber) Done() <-chan struct{} { return s.done }

// IsClosed reports whether Close has been called.
func (s *LogsSubscriber) IsClosed() bool { return s.closed.Load() }

// Follow reports the follow flag the subscriber was created with.
func (s *LogsSubscriber) Follow() bool { return s.follow }

// Ready returns a channel that closes once the multiplexer has
// finished the initial setup for this subscriber: history has been
// streamed and, for follow=true, the subscriber has been appended to
// the live fan-out. Callers (especially tests) use this to know that
// any further serial bytes from the multiplexer will be delivered to
// this subscriber.
func (s *LogsSubscriber) Ready() <-chan struct{} { return s.ready }

// markReady fires Ready exactly once. Called by the multiplexer's
// history-streaming goroutine as it finishes setup.
func (s *LogsSubscriber) markReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

// AddDropped accumulates dropped bytes. The multiplexer's fan-out
// pass calls this when tryDeliver fails so the next successful
// delivery can emit a marker (see §4.1.4).
func (s *LogsSubscriber) AddDropped(n uint64) { s.dropped.Add(n) }

// DroppedBytes returns the current drop counter without resetting it.
func (s *LogsSubscriber) DroppedBytes() uint64 { return s.dropped.Load() }

// SwapDropped atomically reads-and-resets the drop counter; the
// multiplexer calls this after a successful tryDeliver to compute
// the drop marker payload exactly once.
func (s *LogsSubscriber) SwapDropped() uint64 { return s.dropped.Swap(0) }

// Close detaches the subscriber. Idempotent.
func (s *LogsSubscriber) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
	})
	return nil
}

// tryDeliver enqueues data without blocking. Returns
// errSubscriberClosed if closed, errSubscriberFull if the outbound
// channel is full.
func (s *LogsSubscriber) tryDeliver(data []byte) error {
	if s.IsClosed() {
		return errSubscriberClosed
	}
	select {
	case s.ch <- data:
		return nil
	default:
		return errSubscriberFull
	}
}
