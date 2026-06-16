// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package pagination encodes and decodes the opaque cursor used by every
// list endpoint. The cursor packs a `(created_at, id)` pair into a fixed
// 24-byte payload - 8 bytes of created_at as big-endian Unix nanoseconds and
// the 16 raw UUID bytes - base64url-encoded (no padding) to ~32 chars. Clients
// must treat it as opaque. The compact binary form (vs the former JSON) keeps
// the copy-pasted token short; cursors are ephemeral (consumed on the next
// page, never durably stored), so the format carries no compatibility burden.
package pagination

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// cursorPayloadLen is the fixed decoded cursor size: an 8-byte big-endian
// Unix-nano created_at followed by the 16-byte UUID.
const cursorPayloadLen = 24

// Limit clamps a user-supplied limit into the [1, 200] range, defaulting
// to 50 when raw is zero. Mirrors the OpenAPI parameter constraints.
func Limit(raw int) int32 {
	const (
		def = 50
		max = 200
	)
	if raw <= 0 {
		return def
	}
	if raw > max {
		return max
	}
	return int32(raw)
}

// ParseLimit accepts an unsigned integer string from a `?limit=` query
// param; anything else (empty, malformed, negative) degrades to 0,
// which Limit then interprets as "use default". The compose
// `Limit(ParseLimit(q.Get("limit")))` is the canonical decode path
// for every list endpoint.
func ParseLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Cursor carries the cursor's payload after decoding.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Encode packs c into the opaque base64url token clients pass back as the
// next-page cursor: 8 bytes of created_at (big-endian Unix nanoseconds) then the
// 16 raw UUID bytes. A nil cursor encodes as the empty string and is the signal
// "no more pages".
func Encode(c *Cursor) string {
	if c == nil {
		return ""
	}
	var buf [cursorPayloadLen]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(c.CreatedAt.UnixNano()))
	id := c.ID
	copy(buf[8:], id[:])
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

// Decode reverses Encode. An empty string is the canonical "first page"
// signal and returns (nil, nil). A non-empty malformed cursor (bad base64,
// wrong length, or a nil UUID) is a client mistake — caller maps it to
// validation_failed. The created_at round-trips to its original instant in UTC
// (Unix-nano precision), which is what the keyset comparison needs.
func Decode(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode cursor: %v", err)
	}
	if len(raw) != cursorPayloadLen {
		return nil, fmt.Errorf("decode cursor: unexpected length %d, want %d", len(raw), cursorPayloadLen)
	}
	// Bit-preserving round-trip: Encode stored an int64 UnixNano as uint64; cast
	// back. Not a lossy narrowing - the full uint64 maps bijectively to int64.
	nanos := int64(binary.BigEndian.Uint64(raw[0:8])) //nolint:gosec // G115: intentional int64<->uint64 round-trip of UnixNano
	var id uuid.UUID
	copy(id[:], raw[8:])
	if id == uuid.Nil {
		return nil, errors.New("cursor missing id")
	}
	return &Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}
