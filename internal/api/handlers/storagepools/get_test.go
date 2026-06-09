// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// getStoreFake is a minimal Store double covering the GET-by-UUID render
// path: resolver.Pool (StoragePoolByID), projectView (PoolEffectiveByID,
// NodeByID, ClusterSettings) and the new PoolImageInventory seam.
type getStoreFake struct {
	pool      store.StoragePool
	effective store.PoolEffectiveCapacity
	node      store.Node
	settings  store.ClusterSetting
	inventory []store.PoolImage
}

func (f *getStoreFake) StoragePoolByID(_ context.Context, id uuid.UUID) (store.StoragePool, error) {
	if id == f.pool.ID {
		return f.pool, nil
	}
	return store.StoragePool{}, store.ErrNotFound
}

func (f *getStoreFake) StoragePoolsByName(context.Context, string) ([]store.StoragePool, error) {
	return nil, store.ErrNotFound
}

func (f *getStoreFake) ClusterSettings(context.Context) (store.ClusterSetting, error) {
	return f.settings, nil
}

func (f *getStoreFake) NodeByName(context.Context, string) (store.Node, error) {
	return store.Node{}, store.ErrNotFound
}

func (f *getStoreFake) VMByName(context.Context, string) (store.VM, error) {
	return store.VM{}, store.ErrNotFound
}

func (f *getStoreFake) CreateStoragePool(context.Context, store.CreateStoragePoolParams) (store.StoragePool, error) {
	return store.StoragePool{}, nil
}

func (f *getStoreFake) UpdateStoragePool(context.Context, store.UpdateStoragePoolParams) (store.StoragePool, error) {
	return store.StoragePool{}, nil
}

func (f *getStoreFake) PoolEffectiveByID(_ context.Context, id uuid.UUID) (store.PoolEffectiveCapacity, error) {
	if id == f.effective.ID {
		return f.effective, nil
	}
	return store.PoolEffectiveCapacity{}, store.ErrNotFound
}

func (f *getStoreFake) ListPoolsEffective(context.Context, store.ListPoolsEffectiveParams) ([]store.PoolEffectiveCapacity, error) {
	return nil, nil
}

func (f *getStoreFake) ListPoolsEffectiveByName(context.Context, string) ([]store.PoolEffectiveCapacity, error) {
	return nil, nil
}

func (f *getStoreFake) DeleteStoragePool(context.Context, uuid.UUID) error { return nil }

func (f *getStoreFake) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	if id == f.node.ID {
		return f.node, nil
	}
	return store.Node{}, store.ErrNotFound
}

func (f *getStoreFake) PoolImageInventory(_ context.Context, poolID uuid.UUID) ([]store.PoolImage, error) {
	if poolID == f.pool.ID {
		return f.inventory, nil
	}
	return nil, nil
}

func (f *getStoreFake) EnqueueTask(context.Context, store.CreateTaskParams, queue.JobArgs) (uuid.UUID, error) {
	return uuid.Nil, nil
}

// TestGetByID_SurfacesImageInventory locks in that GET /v1/storage-pools/{uuid}
// embeds the agent-reported image inventory under `images[]`.
func TestGetByID_SurfacesImageInventory(t *testing.T) {
	t.Parallel()

	poolID := uuid.New()
	nodeID := uuid.New()
	importedAt := time.Date(2026, 5, 7, 12, 34, 56, 789000000, time.UTC)

	fake := &getStoreFake{
		pool: store.StoragePool{ID: poolID, NodeID: nodeID, Name: "fast", Type: "dir", Path: "/var/lib/otherix/pools/fast"},
		effective: store.PoolEffectiveCapacity{
			ID:                   poolID,
			NodeID:               nodeID,
			Name:                 "fast",
			Type:                 "dir",
			Path:                 "/var/lib/otherix/pools/fast",
			ReconciliationStatus: "ready",
			CreatedAt:            importedAt,
			UpdatedAt:            importedAt,
		},
		node:     store.Node{ID: nodeID, Name: "node-a", Status: store.NodeStatusReady},
		settings: store.ClusterSetting{},
		inventory: []store.PoolImage{{
			Basename:         "noble.qcow2",
			ChecksumSha256:   "abc123",
			SizeBytes:        1024,
			VirtualSizeBytes: 4096,
			Format:           "qcow2",
			ImportedAt:       importedAt,
		}},
	}

	h := New(fake, config.StoragePoolsConfig{}, slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/storage-pools/"+poolID.String(), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", poolID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Get status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Images []poolImageView `json:"images"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Images) != 1 {
		t.Fatalf("images len = %d, want 1; body = %s", len(got.Images), rec.Body.String())
	}
	want := poolImageView{
		Name:             "noble.qcow2",
		SHA256:           "abc123",
		SizeBytes:        1024,
		VirtualSizeBytes: 4096,
		Format:           "qcow2",
		ImportedAt:       importedAt.Format(time.RFC3339Nano),
	}
	if got.Images[0] != want {
		t.Errorf("images[0] = %+v, want %+v", got.Images[0], want)
	}
}
