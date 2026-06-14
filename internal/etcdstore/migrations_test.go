// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// migrationJobArgsStub is a minimal queue.JobArgs the migration enqueue path can
// marshal onto the backing job.
type migrationJobArgsStub struct {
	TaskID      uuid.UUID
	MigrationID uuid.UUID
}

func (migrationJobArgsStub) Kind() string { return "vm.migrate" }

func migrationParams(vmID, sourceNode, targetNode uuid.UUID) store.CreateMigrationParams {
	src := sourceNode
	tgt := targetNode
	taskID := uuid.New()
	return store.CreateMigrationParams{
		ID:           uuid.New(),
		VmID:         vmID,
		SourceNodeID: &src,
		TargetNodeID: &tgt,
		Reason:       store.MigrationReasonManual,
		Live:         true,
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.migrate",
			Status:       store.TaskStatusPending,
			ResourceType: "migration",
			MaxAttempts:  3,
		},
	}
}

func TestCreateMigration_WritesRowAndGuards(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	vmID := uuid.New()
	p := migrationParams(vmID, sourceNode.ID, targetNode.ID)
	args := migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID}

	m, err := s.CreateMigration(ctx, p, args)
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if m.ID != p.ID {
		t.Errorf("CreateMigration ID = %v, want %v", m.ID, p.ID)
	}
	if m.Phase != store.MigrationPhasePending {
		t.Errorf("CreateMigration Phase = %q, want %q", m.Phase, store.MigrationPhasePending)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Errorf("CreateMigration timestamps not stamped: created=%v updated=%v", m.CreatedAt, m.UpdatedAt)
	}

	// Primary row is readable at /otherix/migrations/<id>.
	var got store.Migration
	found, err := cl.GetJSON(ctx, etcd.Key("migrations", p.ID.String()), &got)
	if err != nil {
		t.Fatalf("GetJSON(migration primary): %v", err)
	}
	if !found {
		t.Fatalf("migration primary row missing at %s", etcd.Key("migrations", p.ID.String()))
	}
	if got.VmID != vmID || got.Phase != store.MigrationPhasePending {
		t.Errorf("stored migration = {vm:%v phase:%q}, want {vm:%v phase:pending}", got.VmID, got.Phase, vmID)
	}

	// Per-node index entry exists under the prefix the existing reader ranges,
	// with the migration id as the value (activeMigrationsOnNode parses the value).
	srcIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "node", sourceNode.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(source node index): %v", err)
	}
	if len(srcIdx) != 1 || string(srcIdx[0].Value) != p.ID.String() {
		t.Errorf("source node index = %+v, want one entry valued %s", srcIdx, p.ID)
	}
	tgtIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "node", targetNode.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(target node index): %v", err)
	}
	if len(tgtIdx) != 1 || string(tgtIdx[0].Value) != p.ID.String() {
		t.Errorf("target node index = %+v, want one entry valued %s", tgtIdx, p.ID)
	}

	// VM index entry exists.
	vmIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "vm", vmID.String())+"/")
	if err != nil {
		t.Fatalf("Range(vm index): %v", err)
	}
	if len(vmIdx) != 1 || string(vmIdx[0].Value) != p.ID.String() {
		t.Errorf("vm index = %+v, want one entry valued %s", vmIdx, p.ID)
	}

	// Backing task row was written.
	if _, err := s.TaskByID(ctx, p.Task.ID); err != nil {
		t.Errorf("TaskByID(backing task) = %v, want nil", err)
	}

	// A second active migration for the same VM is rejected by the guard.
	p2 := migrationParams(vmID, sourceNode.ID, targetNode.ID)
	args2 := migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}
	if _, err := s.CreateMigration(ctx, p2, args2); !errors.Is(err, store.ErrMigrationActiveExists) {
		t.Errorf("second CreateMigration = %v, want store.ErrMigrationActiveExists", err)
	}
}

// seedPinnedVM writes a vms row pinned to node, plus its pinned_node index entry
// (byte-matching buildBindTxn), so a cutover's index move is observable.
func seedPinnedVM(t *testing.T, cli *etcd.Client, node uuid.UUID) store.VM {
	t.Helper()
	ctx := context.Background()
	v := vmRow("mig-" + uuid.NewString()[:8])
	pin := node
	v.PinnedNodeID = &pin
	seedVM(t, cli, v)
	if err := cli.Put(ctx, etcd.Key("index", "vms", "pinned_node", node.String(), v.ID.String()), []byte(v.ID.String())); err != nil {
		t.Fatalf("seed pinned_node index: %v", err)
	}
	return v
}

// seedActiveMigration creates a migration (source/target) and drives it to the
// active phase so terminal-only paths (cutover, fail) have a non-terminal start.
func seedActiveMigration(t *testing.T, s *etcdstore.Store, vmID, source, target uuid.UUID) store.Migration {
	t.Helper()
	ctx := context.Background()
	p := migrationParams(vmID, source, target)
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	active := store.MigrationPhaseActive
	if err := s.UpdateMigrationProgress(ctx, m.ID, store.MigrationProgressUpdate{Phase: &active}); err != nil {
		t.Fatalf("UpdateMigrationProgress(active): %v", err)
	}
	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID after activate: %v", err)
	}
	return got
}

func TestCommitMigrationCutover_FlipsPin(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}

	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	if err := s.CommitMigrationCutover(ctx, m.ID); err != nil {
		t.Fatalf("CommitMigrationCutover: %v", err)
	}

	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want nodeB %v", gotVM.PinnedNodeID, nodeB.ID)
	}

	gotM, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if gotM.Phase != store.MigrationPhaseCompleted {
		t.Errorf("Phase = %q, want completed", gotM.Phase)
	}
	if gotM.CompletedAt == nil {
		t.Errorf("CompletedAt = nil, want set")
	}
	if gotM.ProgressPercent != 100 {
		t.Errorf("ProgressPercent = %d, want 100", gotM.ProgressPercent)
	}

	// Active-VM guard released: a fresh CreateMigration for the same VM succeeds.
	p2 := migrationParams(vm.ID, nodeA.ID, nodeB.ID)
	if _, err := s.CreateMigration(ctx, p2, migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}); err != nil {
		t.Errorf("CreateMigration after cutover = %v, want nil (guard released)", err)
	}

	// pinned_node index moved A -> B.
	aIdx, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeA.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(A pinned index): %v", err)
	}
	for _, kv := range aIdx {
		if string(kv.Value) == vm.ID.String() {
			t.Errorf("pinned_node index for vm still present under nodeA, want moved")
		}
	}
	bIdx, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeB.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(B pinned index): %v", err)
	}
	foundB := false
	for _, kv := range bIdx {
		if string(kv.Value) == vm.ID.String() {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("pinned_node index for vm not under nodeB, want moved")
	}
}

func TestUpdateMigrationProgress_FailReleasesGuard(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}

	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	failed := store.MigrationPhaseFailed
	msg := "target_unreachable"
	if err := s.UpdateMigrationProgress(ctx, m.ID, store.MigrationProgressUpdate{Phase: &failed, ErrorMessage: &msg}); err != nil {
		t.Fatalf("UpdateMigrationProgress(failed): %v", err)
	}

	gotM, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if gotM.Phase != store.MigrationPhaseFailed {
		t.Errorf("Phase = %q, want failed", gotM.Phase)
	}
	if gotM.ErrorMessage == nil || *gotM.ErrorMessage != msg {
		t.Errorf("ErrorMessage = %v, want %q", gotM.ErrorMessage, msg)
	}
	if gotM.CompletedAt == nil {
		t.Errorf("CompletedAt = nil, want set on terminal failure")
	}

	// Guard released: a fresh CreateMigration for the same VM succeeds.
	p2 := migrationParams(vm.ID, nodeA.ID, nodeB.ID)
	if _, err := s.CreateMigration(ctx, p2, migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}); err != nil {
		t.Errorf("CreateMigration after fail = %v, want nil (guard released)", err)
	}

	// Fail-safe-to-source: PinnedNodeID is unchanged (still nodeA).
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want unchanged nodeA %v", gotVM.PinnedNodeID, nodeA.ID)
	}
}

func TestCommitMigrationCutover_FailedStaysFailed(t *testing.T) {
	cases := []struct {
		name  string
		phase store.MigrationPhase
	}{
		{"failed", store.MigrationPhaseFailed},
		{"cancelled", store.MigrationPhaseCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cli := startStore(t)
			ctx := context.Background()

			nodeA := nodeParams(uniqueNodeName("a"))
			nodeB := nodeParams(uniqueNodeName("b"))
			if _, err := s.CreateNode(ctx, nodeA); err != nil {
				t.Fatalf("CreateNode(A): %v", err)
			}
			if _, err := s.CreateNode(ctx, nodeB); err != nil {
				t.Fatalf("CreateNode(B): %v", err)
			}

			vm := seedPinnedVM(t, cli, nodeA.ID)
			m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

			// Drive the migration to its terminal phase via the progress path.
			phase := tc.phase
			msg := "target_unreachable"
			if err := s.UpdateMigrationProgress(ctx, m.ID, store.MigrationProgressUpdate{Phase: &phase, ErrorMessage: &msg}); err != nil {
				t.Fatalf("UpdateMigrationProgress(%s): %v", tc.phase, err)
			}

			// Cutover on a terminal-failed/cancelled migration must refuse and never
			// re-pin (fail-safe-to-source, spec D3).
			if err := s.CommitMigrationCutover(ctx, m.ID); !errors.Is(err, store.ErrMigrationTerminal) {
				t.Errorf("CommitMigrationCutover(%s) = %v, want store.ErrMigrationTerminal", tc.phase, err)
			}

			gotVM, err := s.VMByID(ctx, vm.ID)
			if err != nil {
				t.Fatalf("VMByID: %v", err)
			}
			if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
				t.Errorf("PinnedNodeID = %v, want unchanged nodeA %v (failed migration never moves the pin)", gotVM.PinnedNodeID, nodeA.ID)
			}
		})
	}
}

func TestCommitMigrationCutover_IdempotentCompleted(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}

	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	if err := s.CommitMigrationCutover(ctx, m.ID); err != nil {
		t.Fatalf("CommitMigrationCutover(first): %v", err)
	}

	// Second cutover on a now-completed migration is a no-op idempotent reconcile:
	// returns nil, leaves the pin on nodeB, the migration completed, and does not
	// churn the pinned_node index.
	if err := s.CommitMigrationCutover(ctx, m.ID); err != nil {
		t.Errorf("CommitMigrationCutover(second) = %v, want nil (idempotent)", err)
	}

	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want nodeB %v", gotVM.PinnedNodeID, nodeB.ID)
	}

	gotM, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if gotM.Phase != store.MigrationPhaseCompleted {
		t.Errorf("Phase = %q, want completed", gotM.Phase)
	}
	if gotM.ProgressPercent != 100 {
		t.Errorf("ProgressPercent = %d, want 100", gotM.ProgressPercent)
	}

	// pinned_node index did not churn: exactly one entry under nodeB, none under nodeA.
	bIdx, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeB.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(B pinned index): %v", err)
	}
	if len(bIdx) != 1 || string(bIdx[0].Value) != vm.ID.String() {
		t.Errorf("nodeB pinned_node index = %+v, want exactly one entry valued %s", bIdx, vm.ID)
	}
	aIdx, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeA.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(A pinned index): %v", err)
	}
	if len(aIdx) != 0 {
		t.Errorf("nodeA pinned_node index = %+v, want zero entries (moved to nodeB)", aIdx)
	}
}

// TestCommitMigrationCutover_ConcurrentConverges drives two concurrent cutover
// calls at the same active migration and asserts the system converges to a
// single clean re-pin regardless of interleaving. This is a CONVERGENCE test
// rather than a deterministic stale-ModRevision injection: CommitMigrationCutover
// reads the migration and VM revisions internally and commits in one Txn, with no
// production test-seam to wedge a competing write between its read and its commit
// (by design - we will not add one). So we exercise the CAS through real
// concurrency: exactly one caller wins the Txn (PinnedNodeID flips to nodeB,
// migration completes, pinned_node index moves once); the loser either re-reads
// the now-completed row and returns nil (idempotent reconcile) or loses the CAS
// and returns store.ErrConcurrentUpdate. No other error and no panic is allowed.
func TestCommitMigrationCutover_ConcurrentConverges(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}

	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	const workers = 2
	var wg sync.WaitGroup
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			// Never t.Fatal from a worker goroutine: collect into errs and assert
			// after wg.Wait() on the main goroutine (Google style).
			errs[idx] = s.CommitMigrationCutover(ctx, m.ID)
		}(i)
	}
	wg.Wait()

	// Each worker either did the work / observed it done (nil) or lost the CAS
	// (ErrConcurrentUpdate). Any other error means a non-converging interleaving.
	for i, err := range errs {
		if err != nil && !errors.Is(err, store.ErrConcurrentUpdate) {
			t.Errorf("worker %d err = %v, want nil or store.ErrConcurrentUpdate", i, err)
		}
	}

	// VM pinned to nodeB exactly once - never split, never double-claimed.
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want nodeB %v", gotVM.PinnedNodeID, nodeB.ID)
	}

	gotM, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if gotM.Phase != store.MigrationPhaseCompleted {
		t.Errorf("Phase = %q, want completed", gotM.Phase)
	}

	// pinned_node index converged: exactly one entry under nodeB, none under nodeA.
	bIdx, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeB.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(B pinned index): %v", err)
	}
	if len(bIdx) != 1 || string(bIdx[0].Value) != vm.ID.String() {
		t.Errorf("nodeB pinned_node index = %+v, want exactly one entry valued %s (no double-claim)", bIdx, vm.ID)
	}
	aIdx, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeA.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(A pinned index): %v", err)
	}
	if len(aIdx) != 0 {
		t.Errorf("nodeA pinned_node index = %+v, want zero entries (no leak)", aIdx)
	}
}

func TestCommitMigrationCutover_NoTarget(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("a"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}

	vm := seedPinnedVM(t, cli, nodeA.ID)

	// Migration with a nil target: pending, source-only.
	src := nodeA.ID
	taskID := uuid.New()
	p := store.CreateMigrationParams{
		ID:           uuid.New(),
		VmID:         vm.ID,
		SourceNodeID: &src,
		Reason:       store.MigrationReasonManual,
		Live:         true,
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.migrate",
			Status:       store.TaskStatusPending,
			ResourceType: "migration",
			MaxAttempts:  3,
		},
	}
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: taskID, MigrationID: p.ID})
	if err != nil {
		t.Fatalf("CreateMigration(no target): %v", err)
	}

	if err := s.CommitMigrationCutover(ctx, m.ID); err == nil {
		t.Errorf("CommitMigrationCutover(no target) = nil, want error")
	}
}

func TestMigrationByID_NotFound(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if _, err := s.MigrationByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("MigrationByID(unknown) = %v, want store.ErrNotFound", err)
	}
}

func TestMigrationByID_ReturnsCreated(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	p := migrationParams(uuid.New(), sourceNode.ID, targetNode.ID)
	args := migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID}
	if _, err := s.CreateMigration(ctx, p, args); err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	got, err := s.MigrationByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("MigrationByID(%v) = %v, want nil", p.ID, err)
	}
	if got.ID != p.ID || got.VmID != p.VmID || got.Phase != store.MigrationPhasePending {
		t.Errorf("MigrationByID = {id:%v vm:%v phase:%q}, want {id:%v vm:%v phase:pending}", got.ID, got.VmID, got.Phase, p.ID, p.VmID)
	}
}

func TestCreateMigration_PersistsOptions(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	p := migrationParams(uuid.New(), sourceNode.ID, targetNode.ID)
	p.Live = false
	p.AllowPostcopy = true
	p.TargetPoolName = "pool-a"
	args := migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID}
	if _, err := s.CreateMigration(ctx, p, args); err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	got, err := s.MigrationByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("MigrationByID(%v) = %v, want nil", p.ID, err)
	}
	if got.Live != false {
		t.Errorf("MigrationByID Live = %v, want false", got.Live)
	}
	if got.AllowPostcopy != true {
		t.Errorf("MigrationByID AllowPostcopy = %v, want true", got.AllowPostcopy)
	}
	if got.TargetPoolName != "pool-a" {
		t.Errorf("MigrationByID TargetPoolName = %q, want %q", got.TargetPoolName, "pool-a")
	}
}

func TestListMigrations_VMFilter(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	src := nodeParams(uniqueNodeName("src"))
	tgt := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, src); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, tgt); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	vmA := uuid.New()
	vmB := uuid.New()
	mA := migrationParams(vmA, src.ID, tgt.ID)
	mB := migrationParams(vmB, src.ID, tgt.ID)
	if _, err := s.CreateMigration(ctx, mA, migrationJobArgsStub{TaskID: mA.Task.ID, MigrationID: mA.ID}); err != nil {
		t.Fatalf("CreateMigration(A): %v", err)
	}
	if _, err := s.CreateMigration(ctx, mB, migrationJobArgsStub{TaskID: mB.Task.ID, MigrationID: mB.ID}); err != nil {
		t.Fatalf("CreateMigration(B): %v", err)
	}

	got, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 50, VmID: &vmA})
	if err != nil {
		t.Fatalf("ListMigrations(VmID=A): %v", err)
	}
	if len(got) != 1 || got[0].ID != mA.ID {
		t.Errorf("ListMigrations(VmID=A) = %+v, want one entry %v", got, mA.ID)
	}
}

func TestListMigrations_NodeFilterMatchesSourceOrTarget(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	a := nodeParams(uniqueNodeName("a"))
	b := nodeParams(uniqueNodeName("b"))
	c := nodeParams(uniqueNodeName("c"))
	for _, n := range []store.CreateNodeParams{a, b, c} {
		if _, err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("CreateNode(%s): %v", n.Name, err)
		}
	}

	// m1: a -> b (b is target). m2: b -> c (b is source).
	m1 := migrationParams(uuid.New(), a.ID, b.ID)
	m2 := migrationParams(uuid.New(), b.ID, c.ID)
	if _, err := s.CreateMigration(ctx, m1, migrationJobArgsStub{TaskID: m1.Task.ID, MigrationID: m1.ID}); err != nil {
		t.Fatalf("CreateMigration(m1): %v", err)
	}
	if _, err := s.CreateMigration(ctx, m2, migrationJobArgsStub{TaskID: m2.Task.ID, MigrationID: m2.ID}); err != nil {
		t.Fatalf("CreateMigration(m2): %v", err)
	}

	got, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 50, NodeID: &b.ID})
	if err != nil {
		t.Fatalf("ListMigrations(NodeID=b): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListMigrations(NodeID=b) returned %d, want 2 (source-or-target)", len(got))
	}
	seen := map[uuid.UUID]bool{}
	for _, m := range got {
		seen[m.ID] = true
	}
	if !seen[m1.ID] || !seen[m2.ID] {
		t.Errorf("ListMigrations(NodeID=b) = %+v, want both m1=%v and m2=%v", got, m1.ID, m2.ID)
	}
}

func TestListMigrations_OrderingAndLimit(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	src := nodeParams(uniqueNodeName("src"))
	tgt := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, src); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, tgt); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	// Seed three migrations across distinct VMs so the per-VM active guard does
	// not reject them.
	var created []store.Migration
	for i := 0; i < 3; i++ {
		p := migrationParams(uuid.New(), src.ID, tgt.ID)
		m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
		if err != nil {
			t.Fatalf("CreateMigration(%d): %v", i, err)
		}
		created = append(created, m)
		time.Sleep(2 * time.Millisecond)
	}

	all, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 50})
	if err != nil {
		t.Fatalf("ListMigrations(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListMigrations(all) returned %d, want 3", len(all))
	}
	// Newest-first: descending by CreatedAt, tie-break by ID descending.
	for i := 0; i+1 < len(all); i++ {
		if all[i].CreatedAt.Before(all[i+1].CreatedAt) {
			t.Errorf("ListMigrations not newest-first at %d: %v before %v", i, all[i].CreatedAt, all[i+1].CreatedAt)
		}
	}

	limited, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 2})
	if err != nil {
		t.Fatalf("ListMigrations(limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("ListMigrations(limit=2) returned %d, want 2", len(limited))
	}
	if len(all) >= 2 && (limited[0].ID != all[0].ID || limited[1].ID != all[1].ID) {
		t.Errorf("ListMigrations(limit=2) = %v,%v, want first two of all %v,%v", limited[0].ID, limited[1].ID, all[0].ID, all[1].ID)
	}
}

// cancelStartNodes is a tiny helper that creates the two nodes a cancel test
// needs and returns their params.
func cancelStartNodes(t *testing.T, s *etcdstore.Store) (store.CreateNodeParams, store.CreateNodeParams) {
	t.Helper()
	ctx := context.Background()
	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}
	return nodeA, nodeB
}

func TestCancelMigration_PendingReleasesGuard(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeA, nodeB := cancelStartNodes(t, s)

	vm := seedPinnedVM(t, cli, nodeA.ID)
	// Seed a pending migration directly (no progress-update to active).
	p := migrationParams(vm.ID, nodeA.ID, nodeB.ID)
	created, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if created.Phase != store.MigrationPhasePending {
		t.Fatalf("seed Phase = %q, want pending", created.Phase)
	}

	const reason = "operator_cancel"
	got, err := s.CancelMigration(ctx, created.ID, reason)
	if err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("Phase = %q, want cancelled", got.Phase)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != reason {
		t.Errorf("ErrorMessage = %v, want %q", got.ErrorMessage, reason)
	}
	if got.CompletedAt == nil {
		t.Errorf("CompletedAt = nil, want set")
	}

	// Persisted row matches the returned row.
	persisted, err := s.MigrationByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if persisted.Phase != store.MigrationPhaseCancelled {
		t.Errorf("persisted Phase = %q, want cancelled", persisted.Phase)
	}

	// Active-VM guard released: a fresh CreateMigration for the same VM succeeds.
	p2 := migrationParams(vm.ID, nodeA.ID, nodeB.ID)
	if _, err := s.CreateMigration(ctx, p2, migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}); err != nil {
		t.Errorf("CreateMigration after cancel = %v, want nil (guard released)", err)
	}

	// Fail-safe: PinnedNodeID is unchanged (VM stays on source nodeA).
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want unchanged nodeA %v", gotVM.PinnedNodeID, nodeA.ID)
	}
}

func TestCancelMigration_ActiveReleasesGuard(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeA, nodeB := cancelStartNodes(t, s)

	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	const reason = "operator_cancel"
	got, err := s.CancelMigration(ctx, m.ID, reason)
	if err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("Phase = %q, want cancelled", got.Phase)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != reason {
		t.Errorf("ErrorMessage = %v, want %q", got.ErrorMessage, reason)
	}
	if got.CompletedAt == nil {
		t.Errorf("CompletedAt = nil, want set")
	}

	// Guard released: a fresh CreateMigration for the same VM succeeds.
	p2 := migrationParams(vm.ID, nodeA.ID, nodeB.ID)
	if _, err := s.CreateMigration(ctx, p2, migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}); err != nil {
		t.Errorf("CreateMigration after cancel = %v, want nil (guard released)", err)
	}

	// Fail-safe: PinnedNodeID is unchanged (VM stays on source nodeA).
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want unchanged nodeA %v", gotVM.PinnedNodeID, nodeA.ID)
	}
}

// TestNodeForceDelete_ReleasesMigrationGuard proves the A2.2 seam fix: the
// node-force-delete cascade that cancels active migrations must release the
// per-VM active guard the same way every other terminal transition does.
// Without the fix the guard dangles and the VM is permanently un-migratable
// (every future CreateMigration CAS hits the guard and returns
// ErrMigrationActiveExists), with no API recovery since the row is already
// terminal. The proof of release is that a fresh CreateMigration for the same
// VM SUCCEEDS after the force-delete.
func TestNodeForceDelete_ReleasesMigrationGuard(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeA, nodeB := cancelStartNodes(t, s)

	vm := seedPinnedVM(t, cli, nodeA.ID)
	// An ACTIVE migration with source=nodeA, target=nodeB holds the guard.
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	out, err := s.DeleteNode(ctx, nodeA.ID, true, uuid.New())
	if err != nil {
		t.Fatalf("DeleteNode(force): %v", err)
	}
	if out.MigrationsCancelled != 1 {
		t.Errorf("MigrationsCancelled = %d, want 1", out.MigrationsCancelled)
	}

	// The migration row is now terminal-cancelled (cancel cascade still writes it).
	persisted, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if persisted.Phase != store.MigrationPhaseCancelled {
		t.Errorf("migration Phase = %q, want cancelled", persisted.Phase)
	}

	// The active-VM guard MUST be released: a fresh CreateMigration for the same
	// VM succeeds. Before the fix this returns ErrMigrationActiveExists (the guard
	// leaked) and the VM is permanently un-migratable.
	p2 := migrationParams(vm.ID, nodeB.ID, nodeA.ID)
	if _, err := s.CreateMigration(ctx, p2, migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}); err != nil {
		t.Errorf("CreateMigration after node force-delete = %v, want nil (guard released)", err)
	}
}

func TestCancelMigration_TerminalRejected(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeA, nodeB := cancelStartNodes(t, s)

	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	// Drive to completed via cutover.
	if err := s.CommitMigrationCutover(ctx, m.ID); err != nil {
		t.Fatalf("CommitMigrationCutover: %v", err)
	}

	got, err := s.CancelMigration(ctx, m.ID, "too_late")
	if !errors.Is(err, store.ErrMigrationNotCancelable) {
		t.Errorf("CancelMigration(completed) err = %v, want store.ErrMigrationNotCancelable", err)
	}
	if got.Phase != store.MigrationPhaseCompleted {
		t.Errorf("returned Phase = %q, want unchanged completed", got.Phase)
	}

	// Persisted row is unchanged - still completed.
	persisted, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if persisted.Phase != store.MigrationPhaseCompleted {
		t.Errorf("persisted Phase = %q, want unchanged completed", persisted.Phase)
	}
}

func TestCancelMigration_NotFound(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if _, err := s.CancelMigration(ctx, uuid.New(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CancelMigration(unknown) err = %v, want store.ErrNotFound", err)
	}
}

// migrationParamsNodeless builds CreateMigrationParams with no target bound (the
// node-less `vm migrate` shape): the worker resolves the target via the
// scheduler and binds it with BindMigrationTarget.
func migrationParamsNodeless(vmID, sourceNode uuid.UUID) store.CreateMigrationParams {
	p := migrationParams(vmID, sourceNode, uuid.Nil)
	p.TargetNodeID = nil
	return p
}

func seedNodelessMigration(t *testing.T, s *etcdstore.Store, vmID, source uuid.UUID) store.Migration {
	t.Helper()
	ctx := context.Background()
	p := migrationParamsNodeless(vmID, source)
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
	if err != nil {
		t.Fatalf("CreateMigration(nodeless): %v", err)
	}
	return m
}

// TestBindMigrationTarget_BindsAndIndexes pins 12a: binding a node-less
// migration's target sets TargetNodeID + TargetPoolName, clears SchedulingReason,
// and writes the per-node index entry the reservation / heartbeat gate range, all
// under one CAS.
func TestBindMigrationTarget_BindsAndIndexes(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	vmID := uuid.New()
	m := seedNodelessMigration(t, s, vmID, sourceNode.ID)

	// Stamp a scheduling reason so we can prove the bind clears it.
	reason := "no_eligible_target"
	if err := s.UpdateMigrationProgress(ctx, m.ID, store.MigrationProgressUpdate{SchedulingReason: &reason}); err != nil {
		t.Fatalf("UpdateMigrationProgress(reason): %v", err)
	}

	if err := s.BindMigrationTarget(ctx, m.ID, targetNode.ID, "fast-ssd"); err != nil {
		t.Fatalf("BindMigrationTarget: %v", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.TargetNodeID == nil || *got.TargetNodeID != targetNode.ID {
		t.Errorf("TargetNodeID = %v, want %v", got.TargetNodeID, targetNode.ID)
	}
	if got.TargetPoolName != "fast-ssd" {
		t.Errorf("TargetPoolName = %q, want %q", got.TargetPoolName, "fast-ssd")
	}
	if got.SchedulingReason != nil {
		t.Errorf("SchedulingReason = %v, want nil after bind", got.SchedulingReason)
	}
	if got.Phase != store.MigrationPhasePending {
		t.Errorf("Phase = %q, want pending (bind does not advance phase)", got.Phase)
	}

	tgtIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "node", targetNode.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(target node index): %v", err)
	}
	if len(tgtIdx) != 1 || string(tgtIdx[0].Value) != m.ID.String() {
		t.Errorf("target node index = %+v, want one entry valued %s", tgtIdx, m.ID)
	}
}

// TestBindMigrationTarget_RejectsTerminal pins fail-safe: a terminal migration
// can never have a target bound (it would resurrect a dead saga).
func TestBindMigrationTarget_RejectsTerminal(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	vmID := uuid.New()
	m := seedNodelessMigration(t, s, vmID, sourceNode.ID)
	if _, err := s.CancelMigration(ctx, m.ID, "operator cancel"); err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}

	if err := s.BindMigrationTarget(ctx, m.ID, targetNode.ID, ""); !errors.Is(err, store.ErrMigrationTerminal) {
		t.Errorf("BindMigrationTarget(terminal) = %v, want store.ErrMigrationTerminal", err)
	}
}

// TestBindMigrationTarget_RejectsDifferentTarget pins the conflict guard: binding
// a different node onto a migration that already has a target is rejected, while
// re-binding the SAME target is idempotent.
func TestBindMigrationTarget_RejectsDifferentTarget(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	otherNode := nodeParams(uniqueNodeName("oth"))
	for _, n := range []store.CreateNodeParams{sourceNode, targetNode, otherNode} {
		if _, err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("CreateNode(%s): %v", n.Name, err)
		}
	}

	vmID := uuid.New()
	// Migration already bound to targetNode at create time.
	p := migrationParams(vmID, sourceNode.ID, targetNode.ID)
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	if err := s.BindMigrationTarget(ctx, m.ID, otherNode.ID, ""); !errors.Is(err, store.ErrMigrationTargetConflict) {
		t.Errorf("BindMigrationTarget(different target) = %v, want store.ErrMigrationTargetConflict", err)
	}
	// Re-binding the same target is a no-op success (idempotent reconcile).
	if err := s.BindMigrationTarget(ctx, m.ID, targetNode.ID, ""); err != nil {
		t.Errorf("BindMigrationTarget(same target) = %v, want nil (idempotent)", err)
	}
}

// TestActiveMigrationForVM_ReturnsNonTerminal locks T16a: ActiveMigrationForVM
// returns the VM's single non-terminal migration; once that migration reaches a
// terminal phase (or no migration exists) it reports (zero, false, nil).
func TestActiveMigrationForVM_ReturnsNonTerminal(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	for _, n := range []store.CreateNodeParams{sourceNode, targetNode} {
		if _, err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("CreateNode(%s): %v", n.Name, err)
		}
	}

	vmID := uuid.New()

	// No migration yet -> (zero, false, nil).
	if got, ok, err := s.ActiveMigrationForVM(ctx, vmID); err != nil || ok || got.ID != uuid.Nil {
		t.Fatalf("ActiveMigrationForVM(no migration) = (%v, %v, %v), want (zero, false, nil)", got.ID, ok, err)
	}

	p := migrationParams(vmID, sourceNode.ID, targetNode.ID)
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	// Active (pending) migration -> returned with ok=true.
	got, ok, err := s.ActiveMigrationForVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ActiveMigrationForVM(active): %v", err)
	}
	if !ok {
		t.Fatalf("ActiveMigrationForVM(active) ok = false, want true")
	}
	if got.ID != m.ID {
		t.Errorf("ActiveMigrationForVM(active) ID = %v, want %v", got.ID, m.ID)
	}
	if got.Phase != store.MigrationPhasePending {
		t.Errorf("ActiveMigrationForVM(active) Phase = %q, want pending", got.Phase)
	}

	// Cancel the migration -> terminal -> no active migration.
	if _, err := s.CancelMigration(ctx, m.ID, "test cancel"); err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}
	got, ok, err = s.ActiveMigrationForVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ActiveMigrationForVM(after cancel): %v", err)
	}
	if ok || got.ID != uuid.Nil {
		t.Errorf("ActiveMigrationForVM(after cancel) = (%v, %v), want (zero, false)", got.ID, ok)
	}
}

// mustUUID parses a UUID string in a test, failing on error.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

// createMigrationWithID seeds a migration with an explicit id (the prefix
// resolver keys off the id), reusing migrationParams for the rest of the shape.
func createMigrationWithID(t *testing.T, s *etcdstore.Store, id, vmID, source, target uuid.UUID) {
	t.Helper()
	p := migrationParams(vmID, source, target)
	p.ID = id
	args := migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID}
	if _, err := s.CreateMigration(context.Background(), p, args); err != nil {
		t.Fatalf("CreateMigration(%s): %v", id, err)
	}
}

func TestMigrationsByIDPrefix(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	src := nodeParams(uniqueNodeName("src"))
	tgt := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, src); err != nil {
		t.Fatalf("CreateNode(src): %v", err)
	}
	if _, err := s.CreateNode(ctx, tgt); err != nil {
		t.Fatalf("CreateNode(tgt): %v", err)
	}

	// Two migrations sharing the leading "aaaaaaaa" prefix, one with a distinct
	// "bbbbbbbb" prefix. Each migration is on its own VM (one active per VM).
	idA1 := mustUUID(t, "aaaaaaaa-0000-4000-8000-000000000001")
	idA2 := mustUUID(t, "aaaaaaaa-0000-4000-8000-000000000002")
	idB := mustUUID(t, "bbbbbbbb-0000-4000-8000-000000000003")
	createMigrationWithID(t, s, idA1, uuid.New(), src.ID, tgt.ID)
	createMigrationWithID(t, s, idA2, uuid.New(), src.ID, tgt.ID)
	createMigrationWithID(t, s, idB, uuid.New(), src.ID, tgt.ID)

	// A prefix unique to idB -> exactly that one.
	got, err := s.MigrationsByIDPrefix(ctx, "bbbbbbbb")
	if err != nil {
		t.Fatalf("MigrationsByIDPrefix(unique): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("unique prefix matches = %d, want 1", len(got))
	}
	if got[0].ID != idB {
		t.Errorf("unique prefix id = %v, want %v", got[0].ID, idB)
	}

	// A prefix shared by the two "aaaaaaaa" rows -> multiple (capped at 2).
	got, err = s.MigrationsByIDPrefix(ctx, "aaaaaaaa")
	if err != nil {
		t.Fatalf("MigrationsByIDPrefix(shared): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("shared prefix matches = %d, want 2", len(got))
	}

	// A fully-disambiguating prefix into one of the shared pair -> exactly one.
	got, err = s.MigrationsByIDPrefix(ctx, "aaaaaaaa-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("MigrationsByIDPrefix(full): %v", err)
	}
	if len(got) != 1 || got[0].ID != idA1 {
		t.Fatalf("full prefix = %v, want exactly idA1", got)
	}

	// An absent prefix -> empty.
	got, err = s.MigrationsByIDPrefix(ctx, "cccccccc")
	if err != nil {
		t.Fatalf("MigrationsByIDPrefix(absent): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("absent prefix matches = %d, want 0", len(got))
	}
}
