// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// ReconcileStore is the storage surface the durability reconcile loop needs.
// *etcdstore.Store satisfies it.
type ReconcileStore interface {
	DurabilityStore
	AllPlacementDigests(ctx context.Context) ([]string, error)
	AllNodeBlobDigests(ctx context.Context) ([]string, error)
	BlobPlacements(ctx context.Context, digest string) ([]uuid.UUID, error)
	AddBlobPlacement(ctx context.Context, digest string, nodeID uuid.UUID) error
	RemoveBlobPlacement(ctx context.Context, digest string, nodeID uuid.UUID) (bool, error)
	UpsertNodeBlobInventory(ctx context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
	TryBeginReplicate(ctx context.Context, digest string, nodeID uuid.UUID, ttl time.Duration) (bool, error)
	EndReplicate(ctx context.Context, digest string, nodeID uuid.UUID) error
	TryBeginReclaim(ctx context.Context, digest string, nodeID uuid.UUID, ttl time.Duration) (bool, error)
	EndReclaim(ctx context.Context, digest string, nodeID uuid.UUID) error
}

// replicateMaxAttempts is the per-task retry budget stamped on the enqueued row;
// the dispatcher applies its own registered budget. The reconcile loop re-enqueues
// each pass while a replica is short, so this is a bound, not the only retry path.
const replicateMaxAttempts = 25

// replicateInflightTTL bounds how long an in-flight replicate suppresses a
// duplicate enqueue if the worker dies without clearing the marker. Comfortably
// larger than a normal pull, smaller than an operator's patience.
const replicateInflightTTL = 5 * time.Minute

// ReconcileFunc returns the periodic durability reconcile pass. For every produced
// blob it holds the placement map to the desired replica count K: it prunes
// holders that are rebalance-eligible (never the last live pointer), adds fresh
// targets by rendezvous hash to reach K, and enqueues an artifact.replicate task
// for every chosen-but-not-yet-holding live member (only when a live holder exists
// to pull from). It also drives the reclaim side: a blob no longer referenced by
// any snapshot has every live holder reclaimed, and a referenced blob with more
// live holders than K has the rendezvous-surplus holders reclaimed back down to K.
// Reclaim victims are always chosen from observed live holders, never from the
// placement map, so a failed reclaim self-heals off observed reality next pass.
// It reconciles the union of placement digests (the durability intent) and the
// digests observed across node_blobs inventories (observed reality), so a blob a
// node still holds but whose placement key was already pruned is revisited and, if
// unreferenced, collected through the reclaim path. Purely CP-side; the byte
// movement is the existing pull/reclaim sagas.
func ReconcileFunc(st ReconcileStore, grace time.Duration, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		placed, err := st.AllPlacementDigests(ctx)
		if err != nil {
			return fmt.Errorf("list placement digests: %v", err)
		}
		observed, err := st.AllNodeBlobDigests(ctx)
		if err != nil {
			return fmt.Errorf("list observed digests: %v", err)
		}
		digests := unionDigests(placed, observed)
		nodes, err := st.AllNodes(ctx)
		if err != nil {
			return fmt.Errorf("list nodes: %v", err)
		}
		now := time.Now().UTC()
		live := liveNodeIDs(nodes)
		eligible := rebalanceEligibleNodeIDs(nodes, grace, now)
		for id := range eligible {
			if err := st.UpsertNodeBlobInventory(ctx, id, nil); err != nil {
				log.WarnContext(ctx, "durability reconcile: prune rebalance-eligible node inventory failed",
					slog.String("node_id", id.String()), slog.Any("error", err))
			}
		}
		for _, digest := range digests {
			if err := reconcileDigest(ctx, st, log, digest, nodes, live, eligible); err != nil {
				log.WarnContext(ctx, "durability reconcile: digest pass failed",
					slog.String("digest", digest), slog.Any("error", err))
			}
		}
		return nil
	}
}

// unionDigests returns the deduplicated union of two digest lists. Placement is
// the durability intent; node-blob digests are observed reality. Reconciling the
// union lets the pass collect a blob a node still holds but that has no placement
// entry (an orphaned blob whose placement key was already pruned), closing the
// residual leak where a pruned placement key would otherwise hide a held blob.
func unionDigests(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, d := range list {
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
		}
	}
	return out
}

func reconcileDigest(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, nodes []store.Node, live, rebalanceEligible map[uuid.UUID]bool) error {
	refs, err := st.SnapshotsReferencingBlob(ctx, digest)
	if err != nil {
		return err
	}
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

	members = pruneDeadMembers(ctx, st, log, digest, members, rebalanceEligible, holderSet)

	// Orphaned: no snapshot references this blob, so the cluster wants zero copies.
	// Reclaim every live holder and prune the placement key of any member observed
	// to no longer hold the blob. A member that still holds keeps its key until
	// absence is observed, so the digest stays in the placement scan until the
	// bytes are confirmed gone everywhere.
	if len(refs) == 0 {
		reclaimSurplus(ctx, st, log, digest, holders, nil)
		pruneObservedAbsentMembers(ctx, st, log, digest, members, live, holderSet)
		return nil
	}

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

	// Over-replicated: more live holders than K. Keep the K rendezvous-preferred
	// holders, reclaim the surplus, and trim every non-keeper placement member
	// (the K keepers keep the digest scannable, so a failed reclaim self-heals off
	// observed holders next pass).
	if len(holders) > k {
		keepers := selectTargets(digest, holders, k)
		keeperSet := make(map[uuid.UUID]bool, len(keepers))
		for _, kp := range keepers {
			keeperSet[kp] = true
		}
		reclaimSurplus(ctx, st, log, digest, holders, keeperSet)
		trimNonKeeperMembers(ctx, st, log, digest, members, keeperSet)
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
		began, err := st.TryBeginReplicate(ctx, digest, m, replicateInflightTTL)
		if err != nil {
			log.WarnContext(ctx, "durability reconcile: replicate dedup check failed",
				slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
			continue
		}
		if !began {
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
			_ = st.EndReplicate(ctx, digest, m)
			log.WarnContext(ctx, "durability reconcile: enqueue replicate failed",
				slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
		}
	}
}

// reclaimMaxAttempts is the per-task retry budget stamped on an enqueued reclaim
// row; the dispatcher applies its own registered budget. The reconcile loop
// re-enqueues each pass while a copy is surplus, so this is a bound, not the only
// retry path.
const reclaimMaxAttempts = 25

// reclaimInflightTTL bounds how long an in-flight reclaim suppresses a duplicate
// enqueue if the worker dies without clearing the marker.
const reclaimInflightTTL = 5 * time.Minute

// reclaimSurplus enqueues an artifact.reclaim task for every live holder not in
// keepers (nil keepers => every holder is surplus, the orphaned case), guarded by
// the cross-replica reclaim marker. Best-effort: an error is logged and skipped.
func reclaimSurplus(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, holders []uuid.UUID, keepers map[uuid.UUID]bool) {
	for _, h := range holders {
		if keepers[h] {
			continue
		}
		began, err := st.TryBeginReclaim(ctx, digest, h, reclaimInflightTTL)
		if err != nil {
			log.WarnContext(ctx, "durability reconcile: reclaim dedup check failed",
				slog.String("digest", digest), slog.String("node_id", h.String()), slog.Any("error", err))
			continue
		}
		if !began {
			continue
		}
		taskID := uuid.New()
		if _, err := st.EnqueueTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "artifact.reclaim",
			Status:       store.TaskStatusPending,
			ResourceType: "artifact_blob",
			Args:         []byte(`{}`),
			MaxAttempts:  reclaimMaxAttempts,
		}, ReclaimArgs{TaskID: taskID, Digest: digest, TargetNodeID: h}); err != nil {
			_ = st.EndReclaim(ctx, digest, h)
			log.WarnContext(ctx, "durability reconcile: enqueue reclaim failed",
				slog.String("digest", digest), slog.String("node_id", h.String()), slog.Any("error", err))
		}
	}
}

// pruneObservedAbsentMembers removes the placement key of any live member not
// currently holding the digest (observed absence = the reclaim landed, or it never
// held it). Members that still hold, are unreachable (transient), or gone (handled
// earlier) are left in place. Used for the orphaned case, where the key must
// survive until the bytes are confirmed gone.
//
// Pruning on observed absence trusts the node's reported inventory. A node that
// reports a complete inventory which, because of an enumeration fault, omits a blob
// it still holds would have its placement key pruned here, dropping the digest from
// the placement scan; if the bytes are in fact still present they then leak, since
// the placement map is the only cluster-wide rediscovery handle. This is bounded: a
// node either reports its full inventory or, when enumeration fails, the prior
// inventory is preserved, so observed absence closely tracks true absence in normal
// operation. A future content-addressed sweep over each node's store is the backstop
// for this residual leak.
func pruneObservedAbsentMembers(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, members []uuid.UUID, live, holderSet map[uuid.UUID]bool) {
	for _, m := range members {
		if live[m] && !holderSet[m] {
			if _, err := st.RemoveBlobPlacement(ctx, digest, m); err != nil {
				log.WarnContext(ctx, "durability reconcile: prune observed-absent member failed",
					slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
				continue
			}
			log.InfoContext(ctx, "durability reconcile: pruned observed-absent placement member",
				slog.String("digest", digest), slog.String("node_id", m.String()))
		}
	}
}

// trimNonKeeperMembers removes the placement key of every member not in keepers.
// Used for the over-replication case: the cluster keeps exactly the k keeper
// holders, so surplus members leave the intent index immediately. Safe because the
// k keepers keep the digest in the placement scan, so a reclaim that fails is
// re-detected off observed holders next pass. This is safe only while every observed
// holder is also a placement member (true today: the snapshot seed and the reconcile
// add-targets step both record a member key before a node can hold the blob); a
// future out-of-band holder source that lets a node hold a blob without a member key
// would need to preserve this invariant, or the digest could drop from the placement
// scan while a surplus copy remains.
func trimNonKeeperMembers(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, members []uuid.UUID, keepers map[uuid.UUID]bool) {
	for _, m := range members {
		if keepers[m] {
			continue
		}
		if _, err := st.RemoveBlobPlacement(ctx, digest, m); err != nil {
			log.WarnContext(ctx, "durability reconcile: trim surplus member failed",
				slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
			continue
		}
		log.InfoContext(ctx, "durability reconcile: trimmed surplus placement member",
			slog.String("digest", digest), slog.String("node_id", m.String()))
	}
}

// pruneDeadMembers removes placement members that are rebalance-eligible (a dead
// holder), but only while another live holder of the digest remains. Returns the
// surviving member set. A best-effort delete error keeps the member (it is
// reconsidered next pass).
func pruneDeadMembers(ctx context.Context, st ReconcileStore, log *slog.Logger, digest string, members []uuid.UUID, eligible, holderSet map[uuid.UUID]bool) []uuid.UUID {
	liveHolderCount := 0
	for _, m := range members {
		if holderSet[m] {
			liveHolderCount++
		}
	}
	kept := make([]uuid.UUID, 0, len(members))
	for _, m := range members {
		if eligible[m] && liveHolderCount > 0 {
			if _, err := st.RemoveBlobPlacement(ctx, digest, m); err != nil {
				log.WarnContext(ctx, "durability reconcile: prune dead member failed",
					slog.String("digest", digest), slog.String("node_id", m.String()), slog.Any("error", err))
				kept = append(kept, m)
				continue
			}
			log.InfoContext(ctx, "durability reconcile: pruned dead placement member",
				slog.String("digest", digest), slog.String("node_id", m.String()))
			continue
		}
		kept = append(kept, m)
	}
	return kept
}
