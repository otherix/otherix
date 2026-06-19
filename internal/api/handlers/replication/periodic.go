// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// ReconcileStore is the storage surface the durability reconcile loop needs.
// *etcdstore.Store satisfies it.
type ReconcileStore interface {
	DurabilityStore
	AllPlacementDigests(ctx context.Context) ([]string, error)
	BlobPlacements(ctx context.Context, digest string) ([]uuid.UUID, error)
	AddBlobPlacement(ctx context.Context, digest string, nodeID uuid.UUID) error
	RemoveBlobPlacement(ctx context.Context, digest string, nodeID uuid.UUID) (bool, error)
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
}

// replicateMaxAttempts is the per-task retry budget stamped on the enqueued row;
// the dispatcher applies its own registered budget. The reconcile loop re-enqueues
// each pass while a replica is short, so this is a bound, not the only retry path.
const replicateMaxAttempts = 25

// ReconcileFunc returns the periodic durability reconcile pass. For every produced
// blob it holds the placement map to the desired replica count K: it prunes
// holders that reached terminal 'gone' (never the last live pointer), adds fresh
// targets by rendezvous hash to reach K, and enqueues an artifact.replicate task
// for every chosen-but-not-yet-holding live member (only when a live holder exists
// to pull from). Purely CP-side; the byte movement is the existing pull saga. The
// pass is non-destructive apart from the narrowly-scoped gone prune.
func ReconcileFunc(st ReconcileStore, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		digests, err := st.AllPlacementDigests(ctx)
		if err != nil {
			return fmt.Errorf("list placement digests: %v", err)
		}
		nodes, err := st.AllNodes(ctx)
		if err != nil {
			return fmt.Errorf("list nodes: %v", err)
		}
		live := liveNodeIDs(nodes)
		gone := goneNodeIDs(nodes)
		for _, digest := range digests {
			if err := reconcileDigest(ctx, st, log, digest, nodes, live, gone); err != nil {
				log.WarnContext(ctx, "durability reconcile: digest pass failed",
					slog.String("digest", digest), slog.Any("error", err))
			}
		}
		return nil
	}
}

func reconcileDigest(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, nodes []store.Node, live, gone map[uuid.UUID]bool) error {
	k, eligible, err := blobPlacementTarget(ctx, st, digest, nodes, live)
	if err != nil {
		return err
	}
	members, err := st.BlobPlacements(ctx, digest)
	if err != nil {
		return err
	}
	holders, err := liveHolders(ctx, st, digest, live)
	if err != nil {
		return err
	}
	holderSet := make(map[uuid.UUID]bool, len(holders))
	for _, h := range holders {
		holderSet[h] = true
	}

	members = pruneGoneMembers(ctx, st, log, digest, members, gone, holderSet)

	liveMembers := 0
	memberSet := make(map[uuid.UUID]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
		if live[m] {
			liveMembers++
		}
	}
	if need := k - liveMembers; need > 0 && len(holders) > 0 {
		members = addTargets(ctx, st, log, digest, eligible, memberSet, members, need)
	}

	if len(holders) == 0 {
		return nil
	}
	enqueueReplicas(ctx, st, log, digest, members, live, holderSet)
	return nil
}

// addTargets selects need fresh rendezvous-hash targets from the eligible set
// (excluding current members) and records each as a new placement member.
// Returns the extended member set; a best-effort add error skips that target.
func addTargets(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, eligible, memberSet map[uuid.UUID]bool, members []uuid.UUID, need int) []uuid.UUID {
	var candidates []uuid.UUID
	for id := range eligible {
		if !memberSet[id] {
			candidates = append(candidates, id)
		}
	}
	for _, t := range selectTargets(digest, candidates, need) {
		if err := st.AddBlobPlacement(ctx, digest, t); err != nil {
			log.WarnContext(ctx, "durability reconcile: add placement failed",
				slog.String("digest", digest), slog.String("node_id", t.String()), slog.Any("error", err))
			continue
		}
		members = append(members, t)
		memberSet[t] = true
	}
	return members
}

// enqueueReplicas enqueues an artifact.replicate task for every live member that
// is not yet holding the digest. The caller guarantees a live holder exists to
// pull from. A best-effort enqueue error is logged and skipped.
func enqueueReplicas(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, members []uuid.UUID, live, holderSet map[uuid.UUID]bool) {
	for _, m := range members {
		if !live[m] || holderSet[m] {
			continue
		}
		taskID := uuid.New()
		if _, err := st.EnqueueTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "artifact.replicate",
			Status:       store.TaskStatusPending,
			ResourceType: "artifact_blob",
			Args:         []byte(`{}`),
			MaxAttempts:  replicateMaxAttempts,
		}, ReplicateArgs{TaskID: taskID, Digest: digest, TargetNodeID: m}); err != nil {
			log.WarnContext(ctx, "durability reconcile: enqueue replicate failed",
				slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
		}
	}
}

// pruneGoneMembers removes placement members in terminal 'gone' status, but only
// while another live holder of the digest remains. Returns the surviving member
// set. A best-effort delete error keeps the member (it is reconsidered next pass).
func pruneGoneMembers(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, members []uuid.UUID, gone, holderSet map[uuid.UUID]bool) []uuid.UUID {
	liveHolderCount := 0
	for _, m := range members {
		if holderSet[m] {
			liveHolderCount++
		}
	}
	kept := make([]uuid.UUID, 0, len(members))
	for _, m := range members {
		if gone[m] && liveHolderCount > 0 {
			if _, err := st.RemoveBlobPlacement(ctx, digest, m); err != nil {
				log.WarnContext(ctx, "durability reconcile: prune gone member failed",
					slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
				kept = append(kept, m)
				continue
			}
			log.InfoContext(ctx, "durability reconcile: pruned gone placement member",
				slog.String("digest", digest), slog.String("node_id", m.String()))
			continue
		}
		kept = append(kept, m)
	}
	return kept
}
