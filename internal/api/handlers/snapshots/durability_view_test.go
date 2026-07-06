// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// durabilityStoreStub is an in-package double for the snapshot handler Store
// interface, exercising the durability projection on the Get path. Only the
// snapshot-by-id read and the durability read methods are populated; the rest of
// the interface is satisfied with not-found / empty returns since the Get path
// does not call them.
type durabilityStoreStub struct {
	snapshot     store.Snapshot
	referencedBy map[string][]uuid.UUID
	pools        map[string]store.ArtifactPool
	holders      map[string][]uuid.UUID
	nodes        []store.Node

	// listRows backs ListSnapshots (the list paths); vm backs VMByName (the
	// per-VM list path). allNodesCalls counts node-inventory scans so a list
	// test can assert once-per-request, not once-per-row.
	listRows      []store.Snapshot
	vm            store.VM
	allNodesCalls int
}

func (s *durabilityStoreStub) SnapshotByID(_ context.Context, id uuid.UUID) (store.Snapshot, error) {
	if s.snapshot.ID == id {
		return s.snapshot, nil
	}
	return store.Snapshot{}, store.ErrNotFound
}

func (s *durabilityStoreStub) SnapshotsReferencingBlob(_ context.Context, digest string) ([]uuid.UUID, error) {
	return s.referencedBy[digest], nil
}

func (s *durabilityStoreStub) ArtifactPoolByName(_ context.Context, name string) (store.ArtifactPool, error) {
	p, ok := s.pools[name]
	if !ok {
		return store.ArtifactPool{}, store.ErrNotFound
	}
	return p, nil
}

func (s *durabilityStoreStub) BlobHolders(_ context.Context, digest string) ([]uuid.UUID, error) {
	return s.holders[digest], nil
}

func (s *durabilityStoreStub) AllNodes(_ context.Context) ([]store.Node, error) {
	s.allNodesCalls++
	return s.nodes, nil
}

// Remaining Store methods are unused on the Get path.

func (s *durabilityStoreStub) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	return store.StoragePool{}, store.ErrNotFound
}

func (s *durabilityStoreStub) StoragePoolsByName(context.Context, string) ([]store.StoragePool, error) {
	return nil, nil
}

func (s *durabilityStoreStub) ClusterSettings(context.Context) (store.ClusterSetting, error) {
	return store.ClusterSetting{}, store.ErrNotFound
}

func (s *durabilityStoreStub) NodeByName(context.Context, string) (store.Node, error) {
	return store.Node{}, store.ErrNotFound
}

func (s *durabilityStoreStub) VMByName(context.Context, string) (store.VM, error) {
	if s.vm.ID != uuid.Nil {
		return s.vm, nil
	}
	return store.VM{}, store.ErrNotFound
}

func (s *durabilityStoreStub) VMByID(context.Context, uuid.UUID) (store.VM, error) {
	return store.VM{}, store.ErrNotFound
}

func (s *durabilityStoreStub) UserByID(context.Context, uuid.UUID) (store.User, error) {
	return store.User{}, store.ErrNotFound
}

func (s *durabilityStoreStub) VMRuntimeByID(context.Context, uuid.UUID) (store.VMRuntime, error) {
	return store.VMRuntime{}, store.ErrNotFound
}

func (s *durabilityStoreStub) CreateSnapshot(context.Context, store.CreateSnapshotParams, queue.JobArgs) (store.Snapshot, error) {
	return store.Snapshot{}, store.ErrNotFound
}

func (s *durabilityStoreStub) ListSnapshots(context.Context, store.ListSnapshotsParams) ([]store.Snapshot, error) {
	return s.listRows, nil
}

func (s *durabilityStoreStub) DeleteSnapshot(context.Context, uuid.UUID, store.CreateTaskParams, queue.JobArgs) (store.Snapshot, error) {
	return store.Snapshot{}, store.ErrNotFound
}

func TestGetSurfacesDurability(t *testing.T) {
	owner := uuid.New()
	snapID := uuid.New()
	pool := "gold"
	digest := "d0"
	n1, n2 := uuid.New(), uuid.New()

	st := &durabilityStoreStub{
		snapshot: store.Snapshot{
			ID:               snapID,
			VmID:             uuid.New(),
			OwnerID:          owner,
			Name:             "daily",
			Status:           store.SnapshotStatusReady,
			ArtifactPoolName: &pool,
			Disks: []store.SnapshotDisk{
				{Index: 0, Device: "virtio0", SHA256: digest, SizeBytes: 10},
			},
		},
		referencedBy: map[string][]uuid.UUID{digest: {snapID}},
		pools: map[string]store.ArtifactPool{
			pool: {
				Name:              pool,
				ReplicationFactor: store.ReplicationFactor{Count: 2},
				Membership:        store.ArtifactPoolMembership{AllNodes: true},
			},
		},
		holders: map[string][]uuid.UUID{digest: {n1, n2}},
		nodes: []store.Node{
			{ID: n1, Name: "n1", Status: store.NodeStatusReady},
			{ID: n2, Name: "n2", Status: store.NodeStatusReady},
		},
	}

	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}
	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/"+snapID.String(), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", snapID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Get status = %d, body %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Durability       string `json:"durability"`
		DesiredReplicas  int    `json:"desired_replicas"`
		ObservedReplicas int    `json:"observed_replicas"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Durability != "durable" || body.DesiredReplicas != 2 || body.ObservedReplicas != 2 {
		t.Errorf("durability view = %+v, want durable 2 2", body)
	}
}

// listPageStub builds a stub whose page has rowN snapshots, all sharing one
// digest / pool, owned by owner and captured from vmID.
func listPageStub(owner, vmID uuid.UUID, rowN int) (*durabilityStoreStub, string) {
	pool := "gold"
	digest := "d0"
	n1, n2 := uuid.New(), uuid.New()

	rows := make([]store.Snapshot, 0, rowN)
	for i := 0; i < rowN; i++ {
		rows = append(rows, store.Snapshot{
			ID:               uuid.New(),
			VmID:             vmID,
			OwnerID:          owner,
			Name:             "daily",
			Status:           store.SnapshotStatusReady,
			ArtifactPoolName: &pool,
			Disks:            []store.SnapshotDisk{{Index: 0, Device: "virtio0", SHA256: digest, SizeBytes: 10}},
		})
	}

	st := &durabilityStoreStub{
		// snapshot backs SnapshotByID: the digest's referencing snapshot must
		// resolve to a row carrying the pool so K derives to 2.
		snapshot:     rows[0],
		referencedBy: map[string][]uuid.UUID{digest: {rows[0].ID}},
		pools: map[string]store.ArtifactPool{
			pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}},
		},
		holders: map[string][]uuid.UUID{digest: {n1, n2}},
		nodes: []store.Node{
			{ID: n1, Name: "n1", Status: store.NodeStatusReady},
			{ID: n2, Name: "n2", Status: store.NodeStatusReady},
		},
		listRows: rows,
	}
	return st, pool
}

// TestListAllScansNodesOncePerRequest drives GET /v1/snapshots over a multi-row
// page and asserts the node inventory is scanned once per request (page-scoped
// resolver), not once per row, while every rendered row still reports the correct
// durability.
func TestListAllScansNodesOncePerRequest(t *testing.T) {
	owner := uuid.New()
	const rowN = 4
	st, _ := listPageStub(owner, uuid.New(), rowN)

	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}
	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots", nil)
	req = req.WithContext(auth.WithUser(req.Context(), caller))

	rr := httptest.NewRecorder()
	h.ListAll(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("ListAll status = %d, body %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Data []struct {
			Durability       string `json:"durability"`
			DesiredReplicas  int    `json:"desired_replicas"`
			ObservedReplicas int    `json:"observed_replicas"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != rowN {
		t.Fatalf("rows = %d, want %d", len(body.Data), rowN)
	}
	for i, row := range body.Data {
		if row.Durability != "durable" || row.DesiredReplicas != 2 || row.ObservedReplicas != 2 {
			t.Errorf("row %d = %+v, want durable 2 2", i, row)
		}
	}
	if st.allNodesCalls != 1 {
		t.Errorf("AllNodes called %d times over a %d-row page, want 1", st.allNodesCalls, rowN)
	}
}

// TestPerVMListScansNodesOncePerRequest is the per-VM counterpart over
// GET /v1/vms/{id}/snapshots.
func TestPerVMListScansNodesOncePerRequest(t *testing.T) {
	owner := uuid.New()
	vmID := uuid.New()
	const rowN = 3
	st, _ := listPageStub(owner, vmID, rowN)
	st.vm = store.VM{ID: vmID, OwnerID: owner, Name: "web"}

	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}
	req := httptest.NewRequest(http.MethodGet, "/v1/vms/web/snapshots", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "web")
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.List(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("List status = %d, body %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Data []struct {
			Durability string `json:"durability"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != rowN {
		t.Fatalf("rows = %d, want %d", len(body.Data), rowN)
	}
	for i, row := range body.Data {
		if row.Durability != "durable" {
			t.Errorf("row %d durability = %s, want durable", i, row.Durability)
		}
	}
	if st.allNodesCalls != 1 {
		t.Errorf("AllNodes called %d times over a %d-row page, want 1", st.allNodesCalls, rowN)
	}
}
