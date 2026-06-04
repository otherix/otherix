// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// defaultPoolStoreFake is a hand-rolled DefaultPoolStore for the ensurer unit
// tests. It records every CreateStoragePool call and lets a test inject a
// cluster-settings shape plus a canned create error.
type defaultPoolStoreFake struct {
	settings    store.ClusterSetting
	settingsErr error

	createErr error
	created   []store.CreateStoragePoolParams
}

func (f *defaultPoolStoreFake) ClusterSettings(ctx context.Context) (store.ClusterSetting, error) {
	return f.settings, f.settingsErr
}

func (f *defaultPoolStoreFake) CreateStoragePool(ctx context.Context, arg store.CreateStoragePoolParams) (store.StoragePool, error) {
	f.created = append(f.created, arg)
	if f.createErr != nil {
		return store.StoragePool{}, f.createErr
	}
	return store.StoragePool{ID: arg.ID, NodeID: arg.NodeID, Name: arg.Name}, nil
}

func ptr(s string) *string { return &s }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnsureDefaultPoolsFunc_CreatesForReadyNode(t *testing.T) {
	f := &defaultPoolStoreFake{settings: store.ClusterSetting{DefaultPoolName: ptr("default")}}
	hook := EnsureDefaultPoolsFunc(f, "/opt/otherix/pools/", discardLogger())

	nodeID := uuid.New()
	if err := hook(context.Background(), []store.PromoteHealthyNodesRow{{ID: nodeID, Name: "node-a"}}); err != nil {
		t.Fatalf("hook returned error: %v", err)
	}

	if len(f.created) != 1 {
		t.Fatalf("CreateStoragePool called %d times, want 1", len(f.created))
	}
	got := f.created[0]
	if got.ID == uuid.Nil {
		t.Errorf("CreateStoragePool ID = uuid.Nil, want a fresh UUID")
	}
	want := store.CreateStoragePoolParams{
		NodeID: nodeID,
		Name:   "default",
		Type:   validation.StoragePoolTypeLocalDir,
		Path:   "/opt/otherix/pools/default",
		Config: []byte("{}"),
	}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(store.CreateStoragePoolParams{}, "ID")); diff != "" {
		t.Errorf("CreateStoragePool params mismatch (-want +got):\n%s", diff)
	}
}

func TestEnsureDefaultPoolsFunc_IdempotentOnNameExists(t *testing.T) {
	f := &defaultPoolStoreFake{
		settings:  store.ClusterSetting{DefaultPoolName: ptr("default")},
		createErr: store.ErrStoragePoolNameExists,
	}
	hook := EnsureDefaultPoolsFunc(f, "/opt/otherix/pools/", discardLogger())

	if err := hook(context.Background(), []store.PromoteHealthyNodesRow{{ID: uuid.New(), Name: "node-a"}}); err != nil {
		t.Fatalf("hook returned error on duplicate pool: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("CreateStoragePool called %d times, want 1", len(f.created))
	}
}

func TestEnsureDefaultPoolsFunc_OptOutWhenUnset(t *testing.T) {
	f := &defaultPoolStoreFake{settings: store.ClusterSetting{DefaultPoolName: nil}}
	hook := EnsureDefaultPoolsFunc(f, "/opt/otherix/pools/", discardLogger())

	if err := hook(context.Background(), []store.PromoteHealthyNodesRow{{ID: uuid.New(), Name: "node-a"}}); err != nil {
		t.Fatalf("hook returned error: %v", err)
	}
	if len(f.created) != 0 {
		t.Fatalf("CreateStoragePool called %d times, want 0 (opt-out)", len(f.created))
	}
}

func TestEnsureDefaultPoolsFunc_RejectsPathEscapingName(t *testing.T) {
	// Each name escapes or degenerates the allowlist prefix and must be refused
	// before any create: a separator ("a/b", "../etc") or a traversal segment
	// ("..", ".") that filepath.Clean would resolve out of the prefix.
	for _, name := range []string{"a/b", "..", ".", "../etc"} {
		t.Run(name, func(t *testing.T) {
			f := &defaultPoolStoreFake{settings: store.ClusterSetting{DefaultPoolName: ptr(name)}}
			hook := EnsureDefaultPoolsFunc(f, "/opt/otherix/pools/", discardLogger())

			if err := hook(context.Background(), []store.PromoteHealthyNodesRow{{ID: uuid.New(), Name: "node-a"}}); err != nil {
				t.Fatalf("hook returned error: %v", err)
			}
			if len(f.created) != 0 {
				t.Fatalf("CreateStoragePool called %d times for name %q, want 0 (path-escape guard)", len(f.created), name)
			}
		})
	}
}

func TestEnsureDefaultPoolsFunc_PropagatesSettingsError(t *testing.T) {
	sentinel := errors.New("boom")
	f := &defaultPoolStoreFake{settingsErr: sentinel}
	hook := EnsureDefaultPoolsFunc(f, "/opt/otherix/pools/", discardLogger())

	if err := hook(context.Background(), []store.PromoteHealthyNodesRow{{ID: uuid.New(), Name: "node-a"}}); !errors.Is(err, sentinel) {
		t.Fatalf("hook error = %v, want %v", err, sentinel)
	}
	if len(f.created) != 0 {
		t.Fatalf("CreateStoragePool called %d times, want 0", len(f.created))
	}
}
