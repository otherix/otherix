// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"

	"github.com/otherix/otherix/internal/etcd"
)

// ClusterMembership is the CP-mediated etcd membership surface the cluster-join
// handler, the cluster-members admin handler, and the promote loop depend on.
// *etcd.MembershipClient satisfies it; tests substitute a fake. It is carried on
// RouterDeps as one object so the same loopback seam serves all three callers.
type ClusterMembership interface {
	RegisterLearner(ctx context.Context, peerURL string) ([]etcd.Member, error)
	ListMembers(ctx context.Context) ([]etcd.Member, error)
	RemoveMember(ctx context.Context, id uint64) error
	TryPromoteLearners(ctx context.Context) (int, error)
}
