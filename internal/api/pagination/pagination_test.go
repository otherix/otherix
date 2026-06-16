// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pagination_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/pagination"
)

func TestLimitClamps(t *testing.T) {
	tests := []struct {
		in   int
		want int32
	}{
		{in: 0, want: 50},
		{in: -5, want: 50},
		{in: 1, want: 1},
		{in: 200, want: 200},
		{in: 201, want: 200},
		{in: 9_999_999, want: 200},
	}
	for _, tc := range tests {
		if got := pagination.Limit(tc.in); got != tc.want {
			t.Errorf("Limit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	c := &pagination.Cursor{
		CreatedAt: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
		ID:        uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef"),
	}
	encoded := pagination.Encode(c)
	if encoded == "" {
		t.Fatal("Encode(non-nil) returned empty string")
	}
	got, err := pagination.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.CreatedAt.Equal(c.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, c.CreatedAt)
	}
	if got.ID != c.ID {
		t.Errorf("ID = %v, want %v", got.ID, c.ID)
	}
}

// TestEncodeCompactAndRoundTripsMicros pins the compact form (a 24-byte payload
// -> 32 base64url chars, far shorter than the former JSON token) and confirms a
// real microsecond-precision UTC timestamp round-trips to the same instant - the
// precision the keyset comparison relies on.
func TestEncodeCompactAndRoundTripsMicros(t *testing.T) {
	c := &pagination.Cursor{
		CreatedAt: time.Date(2026, 6, 16, 17, 0, 48, 558338000, time.UTC), // micros
		ID:        uuid.MustParse("e5cb61a9-8293-46a7-b68d-7471bba5cd37"),
	}
	enc := pagination.Encode(c)
	if len(enc) != 32 {
		t.Errorf("compact cursor length = %d, want 32 (24-byte payload, base64url no pad): %q", len(enc), enc)
	}
	got, err := pagination.Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.CreatedAt.Equal(c.CreatedAt) {
		t.Errorf("CreatedAt round-trip = %v, want %v", got.CreatedAt, c.CreatedAt)
	}
	if got.ID != c.ID {
		t.Errorf("ID round-trip = %v, want %v", got.ID, c.ID)
	}
}

func TestEncodeNilEmptyString(t *testing.T) {
	if got := pagination.Encode(nil); got != "" {
		t.Errorf("Encode(nil) = %q, want \"\"", got)
	}
}

func TestDecodeEmpty(t *testing.T) {
	got, err := pagination.Decode("")
	if err != nil {
		t.Fatalf("Decode(\"\") err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("Decode(\"\") = %+v, want nil", got)
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{raw: "", want: 0},
		{raw: "abc", want: 0},
		{raw: "-1", want: 0},
		{raw: "0", want: 0},
		{raw: "1", want: 1},
		{raw: "50", want: 50},
		{raw: "12345", want: 12345},
	}
	for _, tc := range cases {
		if got := pagination.ParseLimit(tc.raw); got != tc.want {
			t.Errorf("ParseLimit(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestDecodeMalformed(t *testing.T) {
	cases := []string{
		"@@@not-base64",
		"YWJj",         // valid base64, invalid JSON
		"e30",          // "{}" — missing id
		"eyJpZCI6IiJ9", // {"id":""} — empty / non-uuid id
	}
	for _, s := range cases {
		if _, err := pagination.Decode(s); err == nil {
			t.Errorf("Decode(%q) err = nil, want error", s)
		}
	}
}
