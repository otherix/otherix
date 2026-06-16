// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import "sync"

// consoleInboundBuffer is an in-order, byte-bounded FIFO of WebSocket
// message payloads buffered between the operator (downstream) and the
// current agent (upstream) console connection. One producer (the
// session downstream reader) appends; one consumer (the current
// per-attempt drain pump) takes. During a gap (no live upstream) it
// accumulates up to limit bytes; the frame that would exceed limit is
// dropped and the buffer is marked overflowed (the follow loop then
// drops the session rather than replay a large blind buffer). A failed
// upstream write re-buffers its frame at the head via unshift, so no
// keystroke is lost across an upstream swap. signal is a size-1 channel
// pinged on every append/close so a consumer can block in a select and
// unwind on context cancellation without polling.
type consoleInboundBuffer struct {
	mu       sync.Mutex
	frames   [][]byte
	nbytes   int
	limit    int
	overflow bool
	closed   bool
	sig      chan struct{}
}

// newConsoleInboundBuffer returns a buffer bounded to limit bytes.
func newConsoleInboundBuffer(limit int) *consoleInboundBuffer {
	return &consoleInboundBuffer{limit: limit, sig: make(chan struct{}, 1)}
}

// append copies frame into the buffer FIFO. It returns false (and marks
// the buffer overflowed) when frame would push the buffered total past
// limit, or when the buffer is closed - in both cases the caller stops
// producing.
func (b *consoleInboundBuffer) append(frame []byte) bool {
	b.mu.Lock()
	if b.closed || b.overflow {
		b.mu.Unlock()
		return false
	}
	if b.nbytes+len(frame) > b.limit {
		b.overflow = true
		b.mu.Unlock()
		b.ping()
		return false
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	b.frames = append(b.frames, cp)
	b.nbytes += len(cp)
	b.mu.Unlock()
	b.ping()
	return true
}

// take pops the head frame, or returns ok=false when the buffer is
// momentarily empty. Non-blocking; the consumer blocks on signal().
func (b *consoleInboundBuffer) take() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.frames) == 0 {
		return nil, false
	}
	f := b.frames[0]
	b.frames = b.frames[1:]
	b.nbytes -= len(f)
	return f, true
}

// unshift re-inserts frame at the head (an upstream write that failed
// delivered nothing - WS messages are atomic at the coder/websocket
// API - so the frame must be retried on the next upstream). frame must
// be a buffer the caller already owns (e.g. one returned by take, which
// hands back an append-made copy); unshift does not copy. Unlike append
// it ignores closed/overflow and does not enforce limit, so nbytes may
// transiently exceed limit by the single re-buffered frame - limit is a
// soft bound on this retry path, by design (a failed write is retried,
// never dropped).
func (b *consoleInboundBuffer) unshift(frame []byte) {
	b.mu.Lock()
	b.frames = append([][]byte{frame}, b.frames...)
	b.nbytes += len(frame)
	b.mu.Unlock()
	b.ping()
}

// overflowed reports whether a frame was dropped for exceeding limit.
func (b *consoleInboundBuffer) overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overflow
}

// signal returns the size-1 notification channel pinged on append/close.
func (b *consoleInboundBuffer) signal() <-chan struct{} { return b.sig }

// close marks the buffer closed and wakes any waiter.
func (b *consoleInboundBuffer) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.ping()
}

// ping does a non-blocking send on sig (size-1: a pending ping already
// means "there may be work / state changed").
func (b *consoleInboundBuffer) ping() {
	select {
	case b.sig <- struct{}{}:
	default:
	}
}
