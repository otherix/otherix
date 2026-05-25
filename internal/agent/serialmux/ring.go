// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import "sync"

// RingBuffer is a fixed-capacity byte buffer that drops the oldest
// bytes on overflow. The multiplexer keeps one of these per VM to
// feed a tail-on-connect history snapshot to console subscribers; the
// 8 KB default in NewRingBuffer easily holds 20 lines at typical
// serial widths.
//
// All methods are safe for concurrent use.
type RingBuffer struct {
	mu       sync.Mutex
	data     []byte
	maxBytes int
}

// NewRingBuffer returns a buffer that retains at most maxBytes bytes
// of recent input. A non-positive maxBytes is treated as 0 - every
// Write is silently discarded; LastNLines / Snapshot return empty.
func NewRingBuffer(maxBytes int) *RingBuffer {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &RingBuffer{maxBytes: maxBytes}
}

// Write appends data to the buffer. When the resulting length would
// exceed the configured maximum, the oldest bytes are dropped so the
// suffix fits exactly.
func (r *RingBuffer) Write(data []byte) {
	if len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxBytes == 0 {
		return
	}
	r.data = append(r.data, data...)
	if len(r.data) > r.maxBytes {
		excess := len(r.data) - r.maxBytes
		r.data = append(r.data[:0], r.data[excess:]...)
	}
}

// Snapshot returns a copy of every byte currently held by the buffer.
// The returned slice is independent of internal storage.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.data) == 0 {
		return []byte{}
	}
	out := make([]byte, len(r.data))
	copy(out, r.data)
	return out
}

// LastNLines returns the trailing n lines of buffered output, where a
// line is bounded by '\n' or the buffer's end. n <= 0 yields an empty
// slice; n greater than the number of buffered lines yields the whole
// buffer. The returned slice is a fresh allocation.
func (r *RingBuffer) LastNLines(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return lastNBytes(r.data, n)
}
