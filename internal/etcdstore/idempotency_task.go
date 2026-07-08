// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
)

// idempotencyTaskIndex is the value stored at an idempotency-task index key: the
// task an in-request enqueue produced for a given (user, key), plus the request
// body hash that produced it. The hash lets a reclaim-re-run distinguish a true
// idempotent replay (same body) from a same-key/different-body reuse.
type idempotencyTaskIndex struct {
	TaskID      uuid.UUID `json:"task_id"`
	RequestHash []byte    `json:"request_hash"`
}

// idempotencyTaskIndexKey is the durable index recording which task a (user, key)
// enqueue produced. user_id precedes the raw client key so a key containing '/'
// stays confined to the caller's subtree.
func idempotencyTaskIndexKey(userID uuid.UUID, key string) string {
	return etcd.Key("index", "idempotency_task", userID.String(), key)
}

// idempotencyTaskIndexPrefix is the range prefix for the index (cleanup sweeps).
func idempotencyTaskIndexPrefix() string {
	return etcd.Key("index", "idempotency_task") + "/"
}

func marshalIdempotencyTaskIndex(v idempotencyTaskIndex) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal idempotency task index: %v", err)
	}
	return b, nil
}

func unmarshalIdempotencyTaskIndex(b []byte) (idempotencyTaskIndex, error) {
	var v idempotencyTaskIndex
	if err := json.Unmarshal(b, &v); err != nil {
		return idempotencyTaskIndex{}, fmt.Errorf("unmarshal idempotency task index: %v", err)
	}
	return v, nil
}
