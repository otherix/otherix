// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package pagination encodes and decodes the opaque cursor used by every
// list endpoint. The cursor wraps a `(created_at, id)` pair in
// base64-encoded JSON; clients must treat it as opaque.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

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

// Encode wraps c into the opaque base64 string clients pass back as the
// next-page cursor. A nil cursor encodes as the empty string and is the
// signal "no more pages".
func Encode(c *Cursor) string {
	if c == nil {
		return ""
	}
	payload := struct {
		CreatedAt time.Time `json:"created_at"`
		ID        uuid.UUID `json:"id"`
	}{c.CreatedAt, c.ID}
	raw, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Decode reverses Encode. An empty string is the canonical "first page"
// signal and returns (nil, nil). A non-empty malformed cursor is a
// client mistake — caller maps it to validation_failed.
func Decode(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode cursor: %v", err)
	}
	var payload struct {
		CreatedAt time.Time `json:"created_at"`
		ID        uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal cursor: %v", err)
	}
	if payload.ID == uuid.Nil {
		return nil, errors.New("cursor missing id")
	}
	return &Cursor{CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
}
