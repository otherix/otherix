// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

type reclaimStoreFake struct {
	refs           []uuid.UUID
	holders        []uuid.UUID
	nodes          []store.Node
	finalizeStatus store.TaskStatus
	endReclaimed   bool
}

func (f *reclaimStoreFake) UpdateTaskRunning(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

func (f *reclaimStoreFake) UpdateTaskFinalized(_ context.Context, p store.UpdateTaskFinalizedParams) error {
	f.finalizeStatus = p.Status
	return nil
}

func (f *reclaimStoreFake) EndReclaim(context.Context, string, uuid.UUID) error {
	f.endReclaimed = true
	return nil
}

func (f *reclaimStoreFake) SnapshotsReferencingBlob(context.Context, string) ([]uuid.UUID, error) {
	return f.refs, nil
}

func (f *reclaimStoreFake) SnapshotByID(context.Context, uuid.UUID) (store.Snapshot, error) {
	return store.Snapshot{}, store.ErrNotFound
}

func (f *reclaimStoreFake) ArtifactPoolByName(context.Context, string) (store.ArtifactPool, error) {
	return store.ArtifactPool{}, store.ErrNotFound
}

func (f *reclaimStoreFake) BlobHolders(context.Context, string) ([]uuid.UUID, error) {
	return f.holders, nil
}

func (f *reclaimStoreFake) AllNodes(context.Context) ([]store.Node, error) {
	return f.nodes, nil
}

type reclaimerSpy struct{ calls int }

func (s *reclaimerSpy) Reclaim(context.Context, uuid.UUID, string) error {
	s.calls++
	return nil
}

func liveNodeForReclaim(id uuid.UUID) store.Node {
	return store.Node{ID: id, Status: store.NodeStatusReady}
}

func discardLogReclaim() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const reclaimTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestRunReclaimAbortsWhenReReferenced proves the safety gate: a blob held only
// by the target but newly referenced by a snapshot (desired becomes K>=1) must
// not be deleted, because removing its only holder would drop below desired. The
// worker finalizes SUCCESS without touching the agent.
func TestRunReclaimAbortsWhenReReferenced(t *testing.T) {
	target := uuid.New()
	fake := &reclaimStoreFake{
		refs:    []uuid.UUID{uuid.New()},
		holders: []uuid.UUID{target},
		nodes:   []store.Node{liveNodeForReclaim(target)},
	}
	spy := &reclaimerSpy{}

	if err := runReclaim(context.Background(), fake, spy, discardLogReclaim(),
		ReclaimArgs{TaskID: uuid.New(), Digest: reclaimTestDigest, TargetNodeID: target}); err != nil {
		t.Fatalf("runReclaim() error = %v, want nil", err)
	}
	if spy.calls != 0 {
		t.Errorf("reclaimer called %d times, want 0 (must abort on re-reference)", spy.calls)
	}
	if fake.finalizeStatus != store.TaskStatusSuccess {
		t.Errorf("finalizeStatus = %q, want %q (abort is not an error)", fake.finalizeStatus, store.TaskStatusSuccess)
	}
}

// TestRunReclaimProceedsOnGenuineSurplus proves a referenced blob held by 3 live
// nodes with a desired floor of 1 is reclaimed from the target: removing it
// leaves 2 >= 1.
func TestRunReclaimProceedsOnGenuineSurplus(t *testing.T) {
	target := uuid.New()
	other1, other2 := uuid.New(), uuid.New()
	fake := &reclaimStoreFake{
		refs:    []uuid.UUID{uuid.New()},
		holders: []uuid.UUID{target, other1, other2},
		nodes: []store.Node{
			liveNodeForReclaim(target),
			liveNodeForReclaim(other1),
			liveNodeForReclaim(other2),
		},
	}
	spy := &reclaimerSpy{}

	if err := runReclaim(context.Background(), fake, spy, discardLogReclaim(),
		ReclaimArgs{TaskID: uuid.New(), Digest: reclaimTestDigest, TargetNodeID: target}); err != nil {
		t.Fatalf("runReclaim() error = %v, want nil", err)
	}
	if spy.calls != 1 {
		t.Errorf("reclaimer called %d times, want 1 (genuine surplus)", spy.calls)
	}
}

// TestRunReclaimSkipsWhenTargetNotHolding proves that when the target is absent
// from the live holders, the worker finalizes success without touching the agent.
func TestRunReclaimSkipsWhenTargetNotHolding(t *testing.T) {
	target := uuid.New()
	holder := uuid.New()
	fake := &reclaimStoreFake{
		refs:    []uuid.UUID{uuid.New()},
		holders: []uuid.UUID{holder},
		nodes: []store.Node{
			liveNodeForReclaim(target),
			liveNodeForReclaim(holder),
		},
	}
	spy := &reclaimerSpy{}

	if err := runReclaim(context.Background(), fake, spy, discardLogReclaim(),
		ReclaimArgs{TaskID: uuid.New(), Digest: reclaimTestDigest, TargetNodeID: target}); err != nil {
		t.Fatalf("runReclaim() error = %v, want nil", err)
	}
	if spy.calls != 0 {
		t.Errorf("reclaimer called %d times, want 0 (target not holding)", spy.calls)
	}
}

// TestRunReclaimAbortsWhenSecondHolderUnreachable proves an unreachable second copy
// does not license deleting the last reachable copy: a referenced blob (desired
// floors to K=1) whose holders are {target, other} but where other's node is
// unreachable has only {target} as a live holder. Removing the target would leave 0
// live holders below the desired 1, so the worker aborts without touching the agent.
func TestRunReclaimAbortsWhenSecondHolderUnreachable(t *testing.T) {
	target := uuid.New()
	other := uuid.New()
	fake := &reclaimStoreFake{
		refs:    []uuid.UUID{uuid.New()},
		holders: []uuid.UUID{target, other},
		nodes: []store.Node{
			liveNodeForReclaim(target),
			{ID: other, Status: store.NodeStatusUnreachable},
		},
	}
	spy := &reclaimerSpy{}

	if err := runReclaim(context.Background(), fake, spy, discardLogReclaim(),
		ReclaimArgs{TaskID: uuid.New(), Digest: reclaimTestDigest, TargetNodeID: target}); err != nil {
		t.Fatalf("runReclaim() error = %v, want nil", err)
	}
	if spy.calls != 0 {
		t.Errorf("reclaimer called %d times, want 0 (unreachable copy does not count as live)", spy.calls)
	}
	if fake.finalizeStatus != store.TaskStatusSuccess {
		t.Errorf("finalizeStatus = %q, want %q (abort is not an error)", fake.finalizeStatus, store.TaskStatusSuccess)
	}
}

// TestRunReclaimProceedsWhenOrphaned proves an orphaned blob (zero snapshot refs,
// desired 0) is reclaimed even from its only holder: removing it leaves 0 >= 0.
func TestRunReclaimProceedsWhenOrphaned(t *testing.T) {
	target := uuid.New()
	fake := &reclaimStoreFake{
		refs:    nil,
		holders: []uuid.UUID{target},
		nodes:   []store.Node{liveNodeForReclaim(target)},
	}
	spy := &reclaimerSpy{}

	if err := runReclaim(context.Background(), fake, spy, discardLogReclaim(),
		ReclaimArgs{TaskID: uuid.New(), Digest: reclaimTestDigest, TargetNodeID: target}); err != nil {
		t.Fatalf("runReclaim() error = %v, want nil", err)
	}
	if spy.calls != 1 {
		t.Errorf("reclaimer called %d times, want 1 (orphaned blob)", spy.calls)
	}
}
