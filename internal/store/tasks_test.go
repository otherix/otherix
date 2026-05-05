// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

// seedUserForTasks inserts a minimal user row so a tasks.created_by FK is
// satisfied. Email is per-call unique to avoid collisions across tests.
func seedUserForTasks(t *testing.T, ctx context.Context, s *store.Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := "task-" + uuid.NewString()[:8] + "@example.test"
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: "x",
		DisplayName:  "tasks owner",
		Role:         "developer",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func defaultTaskParams(id, createdBy uuid.UUID) store.CreateTaskParams {
	return store.CreateTaskParams{
		ID:           id,
		Type:         "storage_pool.scan",
		Status:       store.TaskStatusPending,
		ResourceType: "storage_pool",
		ResourceID:   nil,
		Args:         []byte(`{}`),
		MaxAttempts:  25,
		CreatedBy:    &createdBy,
	}
}

func TestTasksCreateGetRoundTrip(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	resourceID := uuid.New()

	id := uuid.New()
	params := defaultTaskParams(id, user)
	params.ResourceID = &resourceID

	created, err := s.Queries().CreateTask(ctx, params)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID = %v, want %v", created.ID, id)
	}
	if created.Status != store.TaskStatusPending {
		t.Errorf("created.Status = %v, want pending", created.Status)
	}
	if created.Type != "storage_pool.scan" {
		t.Errorf("created.Type = %q, want storage_pool.scan", created.Type)
	}
	if created.ResourceID == nil || *created.ResourceID != resourceID {
		t.Errorf("created.ResourceID = %v, want %v", created.ResourceID, resourceID)
	}
	if created.CreatedBy == nil || *created.CreatedBy != user {
		t.Errorf("created.CreatedBy = %v, want %v", created.CreatedBy, user)
	}
	if created.Attempts != 0 {
		t.Errorf("created.Attempts = %d, want 0", created.Attempts)
	}
	if created.MaxAttempts != 25 {
		t.Errorf("created.MaxAttempts = %d, want 25", created.MaxAttempts)
	}
	if created.Progress != nil {
		t.Errorf("created.Progress = %v, want nil", created.Progress)
	}
	if created.Result != nil {
		t.Errorf("created.Result = %v, want nil", created.Result)
	}
	if created.Error != nil {
		t.Errorf("created.Error = %v, want nil", created.Error)
	}
	if created.RiverJobID != nil {
		t.Errorf("created.RiverJobID = %v, want nil (stamped via UpdateTaskRiverJobID)", created.RiverJobID)
	}
	if created.StartedAt != nil {
		t.Errorf("created.StartedAt = %v, want nil", created.StartedAt)
	}
	if created.FinishedAt != nil {
		t.Errorf("created.FinishedAt = %v, want nil", created.FinishedAt)
	}
	if created.CreatedAt.IsZero() {
		t.Error("created.CreatedAt is zero, want set")
	}
	if string(created.Args) != "{}" {
		t.Errorf("created.Args = %q, want {}", string(created.Args))
	}

	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != id || got.Type != created.Type {
		t.Errorf("GetTask = %+v, want round-trip of %+v", got, created)
	}
}

func TestTasksGetMissingReturnsErrNoRows(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.Queries().GetTask(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetTask missing err = %v, want pgx.ErrNoRows", err)
	}
}

func TestTasksUpdateRiverJobID(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	jobID := int64(424242)
	if err := s.Queries().UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
		ID:         id,
		RiverJobID: &jobID,
	}); err != nil {
		t.Fatalf("UpdateTaskRiverJobID: %v", err)
	}

	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RiverJobID == nil || *got.RiverJobID != jobID {
		t.Errorf("RiverJobID = %v, want %d", got.RiverJobID, jobID)
	}
}

// TestTasksUpdateAgentTaskID exercises the resumption-pattern column.
// CreateTask leaves AgentTaskID nil; the worker stamps it via
// UpdateTaskAgentTaskID after the agent's 202 response. Subsequent
// calls overwrite (idempotent for the same uuid; explicit overwrite
// if a different one ever lands — operator-error case the worker
// logs).
func TestTasksUpdateAgentTaskID(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask after Create: %v", err)
	}
	if got.AgentTaskID != nil {
		t.Errorf("created.AgentTaskID = %v, want nil", got.AgentTaskID)
	}

	agentTaskID := uuid.New()
	if err := s.Queries().UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
		ID:          id,
		AgentTaskID: &agentTaskID,
	}); err != nil {
		t.Fatalf("UpdateTaskAgentTaskID: %v", err)
	}

	got, err = s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask after Update: %v", err)
	}
	if got.AgentTaskID == nil || *got.AgentTaskID != agentTaskID {
		t.Errorf("AgentTaskID = %v, want %v", got.AgentTaskID, agentTaskID)
	}
}

func TestTasksUpdateRunningTransition(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := s.Queries().UpdateTaskRunning(ctx, id); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusRunning {
		t.Errorf("Status = %v, want running", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if got.StartedAt == nil {
		t.Fatal("StartedAt = nil, want set after first running transition")
	}
	firstStartedAt := *got.StartedAt

	// Second invocation: attempts++ but started_at must NOT shift (COALESCE
	// guard). Sleep a beat so a misbehaving overwrite would be observable.
	time.Sleep(10 * time.Millisecond)
	if err := s.Queries().UpdateTaskRunning(ctx, id); err != nil {
		t.Fatalf("UpdateTaskRunning #2: %v", err)
	}
	got2, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask #2: %v", err)
	}
	if got2.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", got2.Attempts)
	}
	if got2.StartedAt == nil || !got2.StartedAt.Equal(firstStartedAt) {
		t.Errorf("StartedAt = %v, want unchanged at %v", got2.StartedAt, firstStartedAt)
	}
}

func TestTasksUpdateFinalizedSuccess(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	result := []byte(`{"capacity_bytes":1099511627776,"available_bytes":549755813888}`)
	if err := s.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     id,
		Status: store.TaskStatusSuccess,
		Result: result,
		Error:  nil,
	}); err != nil {
		t.Fatalf("UpdateTaskFinalized: %v", err)
	}

	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusSuccess {
		t.Errorf("Status = %v, want success", got.Status)
	}
	if got.Result == nil {
		t.Errorf("Result = nil, want JSON payload")
	}
	if got.Error != nil {
		t.Errorf("Error = %v, want nil on success", got.Error)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set")
	}
}

func TestTasksUpdateFinalizedFailed(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	errBody := []byte(`{"code":"agent_unreachable","message":"dial timed out"}`)
	if err := s.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     id,
		Status: store.TaskStatusFailed,
		Result: nil,
		Error:  errBody,
	}); err != nil {
		t.Fatalf("UpdateTaskFinalized: %v", err)
	}

	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusFailed {
		t.Errorf("Status = %v, want failed", got.Status)
	}
	if got.Result != nil {
		t.Errorf("Result = %v, want nil on failure", got.Result)
	}
	if got.Error == nil {
		t.Errorf("Error = nil, want envelope")
	}
}

func TestTasksCancelIfPendingHappy(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cancelled, err := s.Queries().CancelTaskIfPending(ctx, id)
	if err != nil {
		t.Fatalf("CancelTaskIfPending: %v", err)
	}
	if cancelled.Status != store.TaskStatusCancelled {
		t.Errorf("Status = %v, want cancelled", cancelled.Status)
	}
	if cancelled.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set")
	}
}

func TestTasksCancelIfPendingRunningReturnsNoRows(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.Queries().UpdateTaskRunning(ctx, id); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}

	if _, err := s.Queries().CancelTaskIfPending(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("CancelTaskIfPending(running) err = %v, want pgx.ErrNoRows", err)
	}

	got, err := s.Queries().GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusRunning {
		t.Errorf("Status = %v, want running (unchanged)", got.Status)
	}
}

func TestTasksCancelIfPendingTerminalReturnsNoRows(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	for _, terminal := range []store.TaskStatus{
		store.TaskStatusSuccess,
		store.TaskStatusFailed,
		store.TaskStatusCancelled,
	} {
		id := uuid.New()
		if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if err := s.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
			ID:     id,
			Status: terminal,
		}); err != nil {
			t.Fatalf("UpdateTaskFinalized(%v): %v", terminal, err)
		}
		if _, err := s.Queries().CancelTaskIfPending(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("CancelTaskIfPending(%v) err = %v, want pgx.ErrNoRows", terminal, err)
		}
	}
}

func TestTasksListAnyFiltersAndPagination(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	resourceA := uuid.New()
	resourceB := uuid.New()

	// Mix of types / resource_types / resource_ids — filter assertions key
	// off these.
	type seed struct {
		typ          string
		resourceType string
		resourceID   *uuid.UUID
		status       store.TaskStatus
	}
	seeds := []seed{
		{"storage_pool.scan", "storage_pool", &resourceA, store.TaskStatusPending},
		{"storage_pool.scan", "storage_pool", &resourceA, store.TaskStatusPending},
		{"storage_pool.scan", "storage_pool", &resourceB, store.TaskStatusPending},
		{"template.import", "template", nil, store.TaskStatusPending},
		{"vm.start", "vm", &resourceA, store.TaskStatusPending},
	}

	created := make([]uuid.UUID, 0, len(seeds))
	for _, sd := range seeds {
		id := uuid.New()
		params := defaultTaskParams(id, user)
		params.Type = sd.typ
		params.ResourceType = sd.resourceType
		params.ResourceID = sd.resourceID
		params.Status = sd.status
		if _, err := s.Queries().CreateTask(ctx, params); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		created = append(created, id)
		// Spread CreatedAt so cursor pagination has a deterministic
		// ordering keyed on the timestamp axis.
		time.Sleep(2 * time.Millisecond)
	}

	// Mark one task running so we can also exercise the status filter.
	if err := s.Queries().UpdateTaskRunning(ctx, created[0]); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}

	containsAll := func(rows []store.Task, want []uuid.UUID) bool {
		got := map[uuid.UUID]bool{}
		for _, r := range rows {
			got[r.ID] = true
		}
		for _, w := range want {
			if !got[w] {
				return false
			}
		}
		return true
	}
	onlyOurs := func(rows []store.Task) []store.Task {
		ours := map[uuid.UUID]bool{}
		for _, id := range created {
			ours[id] = true
		}
		out := rows[:0:0]
		for _, r := range rows {
			if ours[r.ID] {
				out = append(out, r)
			}
		}
		return out
	}

	// No filters → at least our 5 + everything seeded by other tests in
	// the shared harness. Limit is intentionally large.
	all, err := s.Queries().ListTasksAny(ctx, store.ListTasksAnyParams{LimitCount: 1000})
	if err != nil {
		t.Fatalf("ListTasksAny no filters: %v", err)
	}
	if !containsAll(all, created) {
		t.Errorf("ListTasksAny no filters missing seeded ids; got %d rows", len(all))
	}

	// status=running → at least the one we transitioned. Other concurrent
	// tests may have pending rows but should not have running ones with
	// our resource ids; we validate by id intersection rather than by
	// total count.
	running := "running"
	runningRows, err := s.Queries().ListTasksAny(ctx, store.ListTasksAnyParams{
		StatusFilter: &running,
		LimitCount:   1000,
	})
	if err != nil {
		t.Fatalf("ListTasksAny status=running: %v", err)
	}
	mine := onlyOurs(runningRows)
	if len(mine) != 1 || mine[0].ID != created[0] {
		t.Errorf("running filter returned mine = %v, want [%v]", taskIDs(mine), created[0])
	}

	// type=template.import → only seeds[3].
	importType := "template.import"
	imported, err := s.Queries().ListTasksAny(ctx, store.ListTasksAnyParams{
		TypeFilter: &importType,
		LimitCount: 1000,
	})
	if err != nil {
		t.Fatalf("ListTasksAny type=template.import: %v", err)
	}
	mine = onlyOurs(imported)
	if len(mine) != 1 || mine[0].ID != created[3] {
		t.Errorf("type filter returned mine = %v, want [%v]", taskIDs(mine), created[3])
	}

	// resource_type=storage_pool AND resource_id=resourceA → seeds[0..1].
	resourceType := "storage_pool"
	scoped, err := s.Queries().ListTasksAny(ctx, store.ListTasksAnyParams{
		ResourceTypeFilter: &resourceType,
		ResourceIDFilter:   &resourceA,
		LimitCount:         1000,
	})
	if err != nil {
		t.Fatalf("ListTasksAny resource scoped: %v", err)
	}
	mine = onlyOurs(scoped)
	wantSet := map[uuid.UUID]bool{created[0]: true, created[1]: true}
	if len(mine) != 2 {
		t.Errorf("resource-scoped len = %d, want 2", len(mine))
	}
	for _, r := range mine {
		if !wantSet[r.ID] {
			t.Errorf("resource-scoped includes %v, not in want set", r.ID)
		}
	}

	// Cursor pagination: scan our 5 in (created_at desc, id desc) order
	// in two pages of 3 + 3, asserting all five ids show up exactly once
	// AND no row from the second page is more recent than the cursor.
	pageOne, err := s.Queries().ListTasksAny(ctx, store.ListTasksAnyParams{
		ResourceTypeFilter: &resourceType,
		LimitCount:         3,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !sortedDesc(pageOne) {
		t.Errorf("page1 not sorted by (created_at desc, id desc)")
	}
	last := pageOne[len(pageOne)-1]

	pageTwo, err := s.Queries().ListTasksAny(ctx, store.ListTasksAnyParams{
		ResourceTypeFilter: &resourceType,
		CursorCreatedAt:    &last.CreatedAt,
		CursorID:           &last.ID,
		LimitCount:         100,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	for _, r := range pageTwo {
		if r.CreatedAt.After(last.CreatedAt) {
			t.Errorf("page2 row CreatedAt %v is after cursor %v", r.CreatedAt, last.CreatedAt)
		}
		if r.CreatedAt.Equal(last.CreatedAt) && idGTE(r.ID, last.ID) {
			t.Errorf("page2 row id %v not strictly less than cursor id %v at equal CreatedAt",
				r.ID, last.ID)
		}
	}
}

func TestTasksListOwnScope(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userA := seedUserForTasks(t, ctx, s)
	userB := seedUserForTasks(t, ctx, s)

	idA := uuid.New()
	idB := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(idA, userA)); err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(idB, userB)); err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}

	rowsA, err := s.Queries().ListTasksOwn(ctx, store.ListTasksOwnParams{
		CreatedBy:  &userA,
		LimitCount: 1000,
	})
	if err != nil {
		t.Fatalf("ListTasksOwn(A): %v", err)
	}
	for _, r := range rowsA {
		if r.CreatedBy == nil || *r.CreatedBy != userA {
			t.Errorf("ListTasksOwn(A) returned task with CreatedBy=%v, want %v", r.CreatedBy, userA)
		}
		if r.ID == idB {
			t.Error("ListTasksOwn(A) leaked task created by user B")
		}
	}
	if !containsID(rowsA, idA) {
		t.Errorf("ListTasksOwn(A) missing own task %v", idA)
	}
}

func TestTasksDeleteExpiredStateAware(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	user := seedUserForTasks(t, ctx, s)
	now := time.Now()
	completedCutoff := now.Add(-7 * 24 * time.Hour)
	failedCutoff := now.Add(-30 * 24 * time.Hour)

	type fixture struct {
		status     store.TaskStatus
		finishedAt time.Time
		expectKept bool
	}
	fixtures := []fixture{
		// Success: kept inside the 7d window, deleted past it.
		{store.TaskStatusSuccess, now.Add(-1 * time.Hour), true},
		{store.TaskStatusSuccess, now.Add(-8 * 24 * time.Hour), false},
		// Failed: kept inside the 30d window, deleted past it.
		{store.TaskStatusFailed, now.Add(-15 * 24 * time.Hour), true},
		{store.TaskStatusFailed, now.Add(-31 * 24 * time.Hour), false},
		// Cancelled: shares the failed cutoff.
		{store.TaskStatusCancelled, now.Add(-1 * time.Hour), true},
		{store.TaskStatusCancelled, now.Add(-31 * 24 * time.Hour), false},
		// A success row outside the 30d window is gone — the 7d window
		// matters for success, not the longer one.
		{store.TaskStatusSuccess, now.Add(-31 * 24 * time.Hour), false},
	}

	ids := make(map[uuid.UUID]fixture, len(fixtures))
	for _, f := range fixtures {
		id := uuid.New()
		if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(id, user)); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		// Backdate finished_at directly: UpdateTaskFinalized stamps now(),
		// but the cleanup test needs precise historical timestamps.
		const stamp = `update tasks set status = $1, finished_at = $2 where id = $3`
		if _, err := s.Pool().Exec(ctx, stamp, f.status, f.finishedAt, id); err != nil {
			t.Fatalf("backdate task: %v", err)
		}
		ids[id] = f
	}

	// A pending row must survive any cleanup — finished_at is null.
	pendingID := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, defaultTaskParams(pendingID, user)); err != nil {
		t.Fatalf("CreateTask pending: %v", err)
	}

	deleted, err := s.Queries().DeleteExpiredTasks(ctx, store.DeleteExpiredTasksParams{
		CompletedCutoff: completedCutoff,
		FailedCutoff:    failedCutoff,
	})
	if err != nil {
		t.Fatalf("DeleteExpiredTasks: %v", err)
	}
	wantDeleted := int64(0)
	for _, f := range fixtures {
		if !f.expectKept {
			wantDeleted++
		}
	}
	if deleted < wantDeleted {
		t.Errorf("deleted = %d, want >= %d (cross-test rows allowed)", deleted, wantDeleted)
	}

	for id, f := range ids {
		_, err := s.Queries().GetTask(ctx, id)
		if f.expectKept {
			if err != nil {
				t.Errorf("expected-kept task %v (status=%v age=%v) lost: %v",
					id, f.status, now.Sub(f.finishedAt), err)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("expected-deleted task %v (status=%v age=%v) survived: err=%v",
				id, f.status, now.Sub(f.finishedAt), err)
		}
	}

	if _, err := s.Queries().GetTask(ctx, pendingID); err != nil {
		t.Errorf("pending task deleted by cleanup: %v", err)
	}
}

// taskIDs extracts ids for terse error messages.
func taskIDs(rows []store.Task) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// containsID reports whether rows contains a row with the given id.
func containsID(rows []store.Task, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// sortedDesc reports whether rows are in (created_at desc, id desc) order.
func sortedDesc(rows []store.Task) bool {
	return sort.SliceIsSorted(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return idGT(rows[i].ID, rows[j].ID)
	})
}

// idGT reports a > b on uuid bytes.
func idGT(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// idGTE reports a >= b on uuid bytes.
func idGTE(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}
