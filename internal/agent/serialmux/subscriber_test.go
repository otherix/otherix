// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

func TestConsoleSubscriber_TryDeliverEnqueues(t *testing.T) {
	t.Parallel()
	sub := newConsoleSubscriber(nopConsoleWriter, 4)
	if err := sub.tryDeliver([]byte("hi")); err != nil {
		t.Fatalf("tryDeliver: %v", err)
	}
	select {
	case got := <-sub.Bytes():
		if !bytes.Equal(got, []byte("hi")) {
			t.Errorf("Bytes() = %q, want %q", got, "hi")
		}
	default:
		t.Errorf("Bytes() empty, expected delivery")
	}
}

func TestConsoleSubscriber_TryDeliverReportsOverflow(t *testing.T) {
	t.Parallel()
	sub := newConsoleSubscriber(nopConsoleWriter, 1)
	if err := sub.tryDeliver([]byte("a")); err != nil {
		t.Fatalf("first tryDeliver: %v", err)
	}
	err := sub.tryDeliver([]byte("b"))
	if !errors.Is(err, errSubscriberFull) {
		t.Errorf("second tryDeliver: got %v, want errSubscriberFull", err)
	}
}

func TestConsoleSubscriber_AfterClose(t *testing.T) {
	t.Parallel()
	sub := newConsoleSubscriber(nopConsoleWriter, 4)
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sub.IsClosed() {
		t.Errorf("IsClosed() = false after Close, want true")
	}
	if err := sub.tryDeliver([]byte("x")); !errors.Is(err, errSubscriberClosed) {
		t.Errorf("tryDeliver after Close: got %v, want errSubscriberClosed", err)
	}
	// Close is idempotent.
	if err := sub.Close(); err != nil {
		t.Errorf("double Close: got %v, want nil", err)
	}
}

func TestConsoleSubscriber_DoneFiresOnClose(t *testing.T) {
	t.Parallel()
	sub := newConsoleSubscriber(nopConsoleWriter, 4)
	select {
	case <-sub.Done():
		t.Fatalf("Done() closed before Close()")
	default:
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-sub.Done():
	default:
		t.Errorf("Done() not closed after Close()")
	}
}

func TestConsoleSubscriber_WriteForwardsToBackend(t *testing.T) {
	t.Parallel()
	var captured []byte
	writeFn := func(p []byte) error {
		captured = append(captured, p...)
		return nil
	}
	sub := newConsoleSubscriber(writeFn, 4)
	if err := sub.Write([]byte("op typed")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(captured, []byte("op typed")) {
		t.Errorf("backend captured %q, want %q", captured, "op typed")
	}
}

func TestConsoleSubscriber_WriteAfterClose(t *testing.T) {
	t.Parallel()
	sub := newConsoleSubscriber(nopConsoleWriter, 4)
	_ = sub.Close()
	if err := sub.Write([]byte("late")); !errors.Is(err, errSubscriberClosed) {
		t.Errorf("Write after Close: got %v, want errSubscriberClosed", err)
	}
}

func TestLogsSubscriber_TryDeliverEnqueues(t *testing.T) {
	t.Parallel()
	sub := newLogsSubscriber(true, 4)
	if err := sub.tryDeliver([]byte("hi")); err != nil {
		t.Fatalf("tryDeliver: %v", err)
	}
	select {
	case got := <-sub.Bytes():
		if !bytes.Equal(got, []byte("hi")) {
			t.Errorf("Bytes() = %q, want %q", got, "hi")
		}
	default:
		t.Errorf("Bytes() empty, expected delivery")
	}
}

func TestLogsSubscriber_OverflowReportedAndDropCountAvailable(t *testing.T) {
	t.Parallel()
	sub := newLogsSubscriber(true, 1)
	if err := sub.tryDeliver([]byte("a")); err != nil {
		t.Fatalf("first tryDeliver: %v", err)
	}
	if err := sub.tryDeliver([]byte("bb")); !errors.Is(err, errSubscriberFull) {
		t.Errorf("second tryDeliver: got %v, want errSubscriberFull", err)
	}
	// Drop accounting is the multiplexer's responsibility per spec
	// §4.1.4; the subscriber exposes the counter and a SwapDropped
	// helper so the mux can flush + reset atomically.
	sub.AddDropped(2)
	if got := sub.DroppedBytes(); got != 2 {
		t.Errorf("DroppedBytes() = %d, want 2", got)
	}
	if got := sub.SwapDropped(); got != 2 {
		t.Errorf("SwapDropped() = %d, want 2", got)
	}
	if got := sub.DroppedBytes(); got != 0 {
		t.Errorf("DroppedBytes() after Swap = %d, want 0", got)
	}
}

func TestLogsSubscriber_FollowFlagSurfaced(t *testing.T) {
	t.Parallel()
	if got := newLogsSubscriber(true, 4).Follow(); !got {
		t.Errorf("Follow() = false, want true")
	}
	if got := newLogsSubscriber(false, 4).Follow(); got {
		t.Errorf("Follow() = true, want false")
	}
}

func TestLogsSubscriber_AfterClose(t *testing.T) {
	t.Parallel()
	sub := newLogsSubscriber(true, 4)
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sub.IsClosed() {
		t.Errorf("IsClosed = false after Close")
	}
	if err := sub.tryDeliver([]byte("x")); !errors.Is(err, errSubscriberClosed) {
		t.Errorf("tryDeliver after Close: got %v, want errSubscriberClosed", err)
	}
	select {
	case <-sub.Done():
	default:
		t.Errorf("Done not closed after Close")
	}
}

// TestSubscribers_RaceFree exercises the concurrent-write / drop-
// counter / drain paths the race detector would flag if mutation
// were unguarded. The consumer drains opportunistically - producers
// may report errSubscriberFull when the channel is saturated, and
// the test only pins that no goroutine deadlocks or data-races.
func TestSubscribers_RaceFree(t *testing.T) {
	t.Parallel()
	c := newConsoleSubscriber(nopConsoleWriter, 32)
	l := newLogsSubscriber(true, 32)

	stop := make(chan struct{})
	var producers sync.WaitGroup
	producers.Add(3)
	go func() {
		defer producers.Done()
		for range 200 {
			_ = c.tryDeliver([]byte("x"))
		}
	}()
	go func() {
		defer producers.Done()
		for range 200 {
			_ = l.tryDeliver([]byte("y"))
		}
	}()
	go func() {
		defer producers.Done()
		for range 200 {
			l.AddDropped(1)
			_ = l.DroppedBytes()
		}
	}()

	var consumer sync.WaitGroup
	consumer.Add(1)
	go func() {
		defer consumer.Done()
		for {
			select {
			case <-c.Bytes():
			case <-l.Bytes():
			case <-stop:
				return
			}
		}
	}()

	producers.Wait()
	close(stop)
	consumer.Wait()
	_ = c.Close()
	_ = l.Close()
}

func nopConsoleWriter(_ []byte) error { return nil }
