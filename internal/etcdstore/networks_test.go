// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	networkshandlers "github.com/otherix/otherix/internal/api/handlers/networks"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd-backed store satisfies the networks handler's storage contract -
// the Phase 2 seam a second backend slots behind.
var _ networkshandlers.Store = (*etcdstore.Store)(nil)

// startStore spins up a single-node embedded member, a KV client, and an
// etcd-backed Store over it, registering cleanup. Returns the Store and the raw
// client so tests can seed index keys directly.
func startStore(t *testing.T) (*etcdstore.Store, *etcd.Client) {
	t.Helper()
	cfg := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n1",
		DataDir:      filepath.Join(t.TempDir(), "member"),
		PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClusterToken: "otherix-test",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("etcd.Start: %v", err)
	}
	cli := etcd.NewClient(r)
	t.Cleanup(func() {
		_ = cli.Close()
		r.Stop(10 * time.Second)
	})
	return etcdstore.New(cli), cli
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func netParams(name string) store.CreateNetworkParams {
	return store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       name,
		Type:       store.NetworkTypeBridge,
		BridgeName: "br0",
		Mtu:        1500,
		Config:     []byte(`{}`),
	}
}

func uniqueNetName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestNetworkCreateAndGet(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	p := netParams(uniqueNetName("net"))
	created, err := s.CreateNetwork(ctx, p)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if created.ID != p.ID || created.Name != p.Name || created.CreatedAt.IsZero() {
		t.Errorf("CreateNetwork returned %+v, want id/name set + created_at stamped", created)
	}
	got, err := s.NetworkByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("NetworkByID: %v", err)
	}
	if got.ID != p.ID || got.Name != p.Name {
		t.Errorf("NetworkByID = %+v, want id=%v name=%q", got, p.ID, p.Name)
	}
}

func TestNetworkByIDNotFound(t *testing.T) {
	s, _ := startStore(t)
	if _, err := s.NetworkByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByID(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestNetworkCreateDuplicateName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueNetName("dup")
	if _, err := s.CreateNetwork(ctx, netParams(name)); err != nil {
		t.Fatalf("first CreateNetwork: %v", err)
	}
	// Different casing must still collide (case-insensitive guard).
	clash := netParams(strings.ToUpper(name))
	_, err := s.CreateNetwork(ctx, clash)
	if !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("duplicate name err = %v, want store.ErrNetworkNameExists", err)
	}
}

func TestNetworkUpdateRenameCollision(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	taken := uniqueNetName("taken")
	if _, err := s.CreateNetwork(ctx, netParams(taken)); err != nil {
		t.Fatalf("seed taken: %v", err)
	}
	mover := netParams(uniqueNetName("mover"))
	if _, err := s.CreateNetwork(ctx, mover); err != nil {
		t.Fatalf("seed mover: %v", err)
	}
	_, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID: mover.ID, Name: taken, BridgeName: "br1", Mtu: 1500, Config: []byte(`{}`),
	})
	if !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("rename collision err = %v, want store.ErrNetworkNameExists", err)
	}
}

func TestNetworkUpdateSucceeds(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("upd"))
	created, err := s.CreateNetwork(ctx, p)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	newName := uniqueNetName("upd-renamed")
	updated, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID: p.ID, Name: newName, BridgeName: "br9", Mtu: 9000, Config: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if updated.Name != newName || updated.BridgeName != "br9" || updated.Mtu != 9000 {
		t.Errorf("UpdateNetwork = %+v, want renamed/br9/9000", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at not bumped: created %v updated %v", created.UpdatedAt, updated.UpdatedAt)
	}
	// Old name is free again; the new name now collides.
	if _, err := s.CreateNetwork(ctx, netParams(p.Name)); err != nil {
		t.Errorf("old name not reusable after rename: %v", err)
	}
	if _, err := s.CreateNetwork(ctx, netParams(newName)); !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("new name should be taken, got %v", err)
	}
}

func TestNetworkListOrderingPaginationAndDeletedExcluded(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	ids := make([]uuid.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		p := netParams(uniqueNetName(fmt.Sprintf("list%d", i)))
		if _, err := s.CreateNetwork(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, p.ID)
		time.Sleep(2 * time.Millisecond) // keep created_at strictly increasing
	}

	all, err := s.ListNetworks(ctx, store.ListNetworksParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListNetworks all: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("ListNetworks returned %d, want >= 3", len(all))
	}
	// Ascending (created_at, id).
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Errorf("list not ascending at %d: %v before %v", i, all[i].CreatedAt, all[i-1].CreatedAt)
		}
	}

	// Pagination: limit 1 from the first row's cursor yields the next row.
	first := all[0]
	page, err := s.ListNetworks(ctx, store.ListNetworksParams{
		CursorCreatedAt: &first.CreatedAt, CursorID: &first.ID, LimitCount: 1,
	})
	if err != nil {
		t.Fatalf("ListNetworks paged: %v", err)
	}
	if len(page) != 1 || page[0].ID != all[1].ID {
		t.Errorf("paged after first = %v, want [%v]", idsOf(page), all[1].ID)
	}

	// Soft-deleted rows drop out of the list.
	if err := s.DeleteNetwork(ctx, ids[0]); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	after, err := s.ListNetworks(ctx, store.ListNetworksParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListNetworks after delete: %v", err)
	}
	for _, n := range after {
		if n.ID == ids[0] {
			t.Errorf("deleted network %v still listed", ids[0])
		}
	}
}

func TestNetworkDeleteAndNameReuse(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueNetName("del")
	p := netParams(name)
	if _, err := s.CreateNetwork(ctx, p); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if err := s.DeleteNetwork(ctx, p.ID); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, err := s.NetworkByID(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByID after delete = %v, want store.ErrNotFound", err)
	}
	// Name reusable after soft-delete (guard dropped).
	if _, err := s.CreateNetwork(ctx, netParams(name)); err != nil {
		t.Errorf("name not reusable after delete: %v", err)
	}
}

func TestNetworkDeleteBlockedByNic(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("blk"))
	if _, err := s.CreateNetwork(ctx, p); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	// Seed an active vm_nic index entry referencing the network.
	nicKey := etcd.Key("index", "vm_nics", "network", p.ID.String(), uuid.NewString())
	if err := cli.Put(ctx, nicKey, []byte("nic")); err != nil {
		t.Fatalf("seed nic index: %v", err)
	}

	err := s.DeleteNetwork(ctx, p.ID)
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		t.Fatalf("DeleteNetwork err = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vm_nics"] != 1 {
		t.Errorf("blocking vm_nics = %d, want 1", blocking.Resources["vm_nics"])
	}
}

func idsOf(nets []store.Network) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.ID)
	}
	return out
}
