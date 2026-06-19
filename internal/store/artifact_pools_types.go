// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// ReplicationFactor is an artifact pool's durability target K: either a fixed
// integer count, or the sentinel "all" (every current member, dynamically
// including future joiners). Marshals to a JSON integer or the string "all".
// UnmarshalJSON is lenient on the integer value (it accepts 0 and negatives) so
// the same type decodes both the wire request and already-stored etcd rows; the
// API edge (ValidateReplicationFactor) is what rejects count < 1.
type ReplicationFactor struct {
	All   bool
	Count int32 // meaningful only when All is false
}

// MarshalJSON renders the sentinel as "all", else the integer count.
func (rf ReplicationFactor) MarshalJSON() ([]byte, error) {
	if rf.All {
		return []byte(`"all"`), nil
	}
	return []byte(strconv.Itoa(int(rf.Count))), nil
}

// UnmarshalJSON accepts a JSON integer or the exact string "all". A quoted
// number ("1") or any other string is rejected.
func (rf *ReplicationFactor) UnmarshalJSON(b []byte) error {
	var n int32
	if err := json.Unmarshal(b, &n); err == nil {
		*rf = ReplicationFactor{Count: n}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("replication_factor must be a positive integer or \"all\"")
	}
	if s != "all" {
		return fmt.Errorf("replication_factor string must be \"all\", got %q", s)
	}
	*rf = ReplicationFactor{All: true}
	return nil
}

// ArtifactPoolMembership records which nodes are intended to back the pool.
// Advisory metadata for now (nothing is materialised); future replication
// materialises and reconciles it. AllNodes=true means every current and future node;
// otherwise Nodes is an explicit node-name list.
type ArtifactPoolMembership struct {
	AllNodes bool     `json:"all_nodes"`
	Nodes    []string `json:"nodes,omitempty"`
}

// CreateArtifactPoolParams is the input to Store.CreateArtifactPool.
type CreateArtifactPoolParams struct {
	ID                uuid.UUID
	Name              string
	ReplicationFactor ReplicationFactor
	Membership        ArtifactPoolMembership
}

// UpdateArtifactPoolParams is the input to Store.UpdateArtifactPool. Each field
// is a pointer so a nil leaves the existing row value untouched. The name is
// immutable, so it has no field here.
type UpdateArtifactPoolParams struct {
	ReplicationFactor *ReplicationFactor
	Membership        *ArtifactPoolMembership
}

// ListArtifactPoolsParams is the input to Store.ListArtifactPools (cursor
// pagination over a bounded collection, like networks).
type ListArtifactPoolsParams struct {
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}
