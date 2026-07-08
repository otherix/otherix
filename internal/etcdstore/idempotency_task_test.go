// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestIdempotencyTaskIndexKey_UserBeforeKey(t *testing.T) {
	u := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := idempotencyTaskIndexKey(u, "a/b")
	want := "/otherix/index/idempotency_task/" + u.String() + "/a/b"
	if got != want {
		t.Errorf("idempotencyTaskIndexKey(%v, %q) = %q, want %q", u, "a/b", got, want)
	}
}

func TestIdempotencyTaskIndex_RoundTrip(t *testing.T) {
	in := idempotencyTaskIndex{TaskID: uuid.New(), RequestHash: []byte{0xde, 0xad, 0xbe, 0xef}}
	b, err := marshalIdempotencyTaskIndex(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := unmarshalIdempotencyTaskIndex(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TaskID != in.TaskID || !bytes.Equal(out.RequestHash, in.RequestHash) {
		t.Errorf("round-trip = %+v, want %+v", out, in)
	}
}
