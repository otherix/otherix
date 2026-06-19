// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package replication holds the pure placement-policy helpers for durable blob
// replication: deterministic rendezvous-hash target selection over candidate
// nodes, plus live-node and artifact-pool-membership expansion.
package replication

import (
	"bytes"
	"crypto/sha256"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// isLive reports whether a node may hold or receive a durable replica: not
// soft-deleted and not in a terminal-or-stale status (unreachable/gone).
func isLive(n store.Node) bool {
	return n.DeletedAt == nil &&
		n.Status != store.NodeStatusUnreachable &&
		n.Status != store.NodeStatusGone
}

// liveNodeIDs returns the set of node ids that are live.
func liveNodeIDs(nodes []store.Node) map[uuid.UUID]bool {
	live := make(map[uuid.UUID]bool, len(nodes))
	for _, n := range nodes {
		if isLive(n) {
			live[n.ID] = true
		}
	}
	return live
}

// goneNodeIDs returns the set of node ids in terminal 'gone' status (the prune
// signal; an unreachable node is transient and is never pruned).
func goneNodeIDs(nodes []store.Node) map[uuid.UUID]bool {
	gone := map[uuid.UUID]bool{}
	for _, n := range nodes {
		if n.DeletedAt == nil && n.Status == store.NodeStatusGone {
			gone[n.ID] = true
		}
	}
	return gone
}

// membershipNodeIDs expands an artifact pool membership to the set of LIVE node
// ids backing it: every live node when AllNodes is set, else the live nodes whose
// name (case-insensitive) is listed.
func membershipNodeIDs(m store.ArtifactPoolMembership, nodes []store.Node) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	if m.AllNodes {
		for _, n := range nodes {
			if isLive(n) {
				out[n.ID] = true
			}
		}
		return out
	}
	want := make(map[string]struct{}, len(m.Nodes))
	for _, name := range m.Nodes {
		want[strings.ToLower(name)] = struct{}{}
	}
	for _, n := range nodes {
		if !isLive(n) {
			continue
		}
		if _, ok := want[strings.ToLower(n.Name)]; ok {
			out[n.ID] = true
		}
	}
	return out
}

// hrwWeight is the rendezvous (highest-random-weight) score of placing digest on
// nodeID: sha256(digest || nodeID). Deterministic; stable across membership
// changes (only the entering/leaving node reshuffles).
func hrwWeight(digest string, nodeID uuid.UUID) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(digest))
	h.Write(nodeID[:])
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// selectTargets picks up to need nodes from eligible by descending rendezvous
// weight for digest, ties broken by node id for determinism. Returns fewer than
// need when eligible is smaller. Input order does not affect the result.
func selectTargets(digest string, eligible []uuid.UUID, need int) []uuid.UUID {
	if need <= 0 || len(eligible) == 0 {
		return nil
	}
	sorted := make([]uuid.UUID, len(eligible))
	copy(sorted, eligible)
	sort.Slice(sorted, func(i, j int) bool {
		wi, wj := hrwWeight(digest, sorted[i]), hrwWeight(digest, sorted[j])
		if c := bytes.Compare(wi[:], wj[:]); c != 0 {
			return c > 0
		}
		return sorted[i].String() < sorted[j].String()
	})
	if need > len(sorted) {
		need = len(sorted)
	}
	return sorted[:need]
}
