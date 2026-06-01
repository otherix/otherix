// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Networks are a bounded, cluster-wide collection addressed by UUID with a
// case-insensitive name uniqueness guard (uq_networks_name, partial on
// deleted_at). The list is a primary-prefix scan rather than a secondary index
// because the collection is small and not on a hot path.

func networkKey(id uuid.UUID) string { return etcd.Key("networks", id.String()) }

func networkPrefix() string { return etcd.Key("networks") + "/" }

func networkNameGuard(name string) string {
	return etcd.Key("uniq", "networks", "name", strings.ToLower(name))
}

// networkNodeStatusPrefix is the root of the per-(network, node)
// reconciliation status keyspace: /otherix/index/network_node_status/<net>/<node>.
const networkNodeStatusPrefix = "/otherix/index/network_node_status/"

func networkNodeStatusKey(net, node uuid.UUID) string {
	return etcd.Key("index", "network_node_status", net.String(), node.String())
}

func networkNodeStatusByNetworkPrefix(net uuid.UUID) string {
	return etcd.Key("index", "network_node_status", net.String()) + "/"
}

// vmNicNetworkIndexPrefix is the prefix under which active vm_nics record their
// network attachment (written by the vms slice). DeleteNetwork counts the keys
// here to block deletion of a referenced network.
func vmNicNetworkIndexPrefix(id uuid.UUID) string {
	return etcd.Key("index", "vm_nics", "network", id.String()) + "/"
}

// NetworkByID returns the non-deleted network with the given id, or
// store.ErrNotFound.
func (s *Store) NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error) {
	var n store.Network
	found, err := s.c.GetJSON(ctx, networkKey(id), &n)
	if err != nil {
		return store.Network{}, err
	}
	if !found || n.DeletedAt != nil {
		return store.Network{}, store.ErrNotFound
	}
	return n, nil
}

// NetworkByName resolves a non-deleted network through its case-insensitive
// name guard, returning store.ErrNotFound when no live network owns the name.
func (s *Store) NetworkByName(ctx context.Context, name string) (store.Network, error) {
	val, found, err := s.c.Get(ctx, networkNameGuard(name))
	if err != nil {
		return store.Network{}, err
	}
	if !found {
		return store.Network{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(val))
	if err != nil {
		return store.Network{}, fmt.Errorf("corrupt network name guard %q: %v", name, err)
	}
	return s.NetworkByID(ctx, id)
}

// CreateNetwork inserts a network, stamping created_at/updated_at, and writes
// the name guard + primary atomically. A name collision (case-insensitive,
// among non-deleted rows) returns store.ErrNetworkNameExists.
func (s *Store) CreateNetwork(ctx context.Context, arg store.CreateNetworkParams) (store.Network, error) {
	now := time.Now().UTC()
	egress := arg.Egress
	if egress == "" {
		egress = store.NetworkEgressNone
	}
	n := store.Network{
		ID:         arg.ID,
		Name:       arg.Name,
		Type:       arg.Type,
		BridgeName: arg.BridgeName,
		Managed:    arg.Managed,
		Egress:     egress,
		Subnet:     arg.Subnet,
		Gateway:    arg.Gateway,
		VlanTag:    arg.VlanTag,
		Mtu:        arg.Mtu,
		Config:     arg.Config,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.Network{}, err
	}
	guard := networkNameGuard(n.Name)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, n.ID.String()),
			clientv3.OpPut(networkKey(n.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.Network{}, fmt.Errorf("create network txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Network{}, store.ErrNetworkNameExists
	}
	return n, nil
}

// UpdateNetwork rewrites the mutable fields of an existing network (type is
// API-immutable and not touched), bumps updated_at, and moves the name guard
// when the name changes. Returns store.ErrNotFound when the row is missing and
// store.ErrNetworkNameExists when a rename collides with another live network.
func (s *Store) UpdateNetwork(ctx context.Context, arg store.UpdateNetworkParams) (store.Network, error) {
	existing, err := s.NetworkByID(ctx, arg.ID)
	if err != nil {
		return store.Network{}, err
	}
	egress := arg.Egress
	if egress == "" {
		egress = store.NetworkEgressNone
	}
	updated := existing
	updated.Name = arg.Name
	updated.BridgeName = arg.BridgeName
	updated.Managed = arg.Managed
	updated.Egress = egress
	updated.Subnet = arg.Subnet
	updated.Gateway = arg.Gateway
	updated.VlanTag = arg.VlanTag
	updated.Mtu = arg.Mtu
	updated.Config = arg.Config
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.Network{}, err
	}

	oldGuard := networkNameGuard(existing.Name)
	newGuard := networkNameGuard(arg.Name)
	if oldGuard == newGuard {
		// Name unchanged (case-insensitive); the guard stays, only the primary
		// row is rewritten.
		if err := s.c.Put(ctx, networkKey(arg.ID), val); err != nil {
			return store.Network{}, err
		}
		return updated, nil
	}

	// Rename: the new guard must be free; swap guards + rewrite primary atomically.
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0)).
		Then(
			clientv3.OpPut(newGuard, arg.ID.String()),
			clientv3.OpDelete(oldGuard),
			clientv3.OpPut(networkKey(arg.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.Network{}, fmt.Errorf("update network txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Network{}, store.ErrNetworkNameExists
	}
	return updated, nil
}

// ListNetworks returns the non-deleted networks matching the optional type
// filter, ordered by (created_at, id) ascending, after the cursor, capped at
// LimitCount - matching the SQL ListNetworks query. Bounded collection, so a
// primary-prefix scan with in-application filter/sort/paginate is used.
func (s *Store) ListNetworks(ctx context.Context, arg store.ListNetworksParams) ([]store.Network, error) {
	items, err := s.c.Range(ctx, networkPrefix())
	if err != nil {
		return nil, err
	}
	nets := make([]store.Network, 0, len(items))
	for _, kv := range items {
		var n store.Network
		if err := json.Unmarshal(kv.Value, &n); err != nil {
			return nil, fmt.Errorf("unmarshal network %q: %v", kv.Key, err)
		}
		if n.DeletedAt != nil {
			continue
		}
		if arg.Type != nil && n.Type != *arg.Type {
			continue
		}
		if !afterCursor(n.CreatedAt, n.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		nets = append(nets, n)
	}
	sort.Slice(nets, func(i, j int) bool {
		if !nets[i].CreatedAt.Equal(nets[j].CreatedAt) {
			return nets[i].CreatedAt.Before(nets[j].CreatedAt)
		}
		return nets[i].ID.String() < nets[j].ID.String()
	})
	if n := int(arg.LimitCount); n > 0 && len(nets) > n {
		nets = nets[:n]
	}
	return nets, nil
}

// DeleteNetwork soft-deletes the network (sets deleted_at, drops the name guard
// so the name is reusable) after verifying it exists and no active vm_nics
// reference it. Returns store.ErrNotFound when missing or *store.ResourceInUseError
// (key "vm_nics") when referenced.
func (s *Store) DeleteNetwork(ctx context.Context, id uuid.UUID) error {
	existing, err := s.NetworkByID(ctx, id)
	if err != nil {
		return err
	}
	nicCount, err := s.countVMNicsOnNetwork(ctx, id)
	if err != nil {
		return err
	}
	if nicCount > 0 {
		return &store.ResourceInUseError{Resources: map[string]int64{"vm_nics": nicCount}}
	}
	now := time.Now().UTC()
	existing.DeletedAt = &now
	val, err := etcd.Marshal(existing)
	if err != nil {
		return err
	}
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(networkKey(id), string(val)),
			clientv3.OpDelete(networkNameGuard(existing.Name)),
		).
		Commit(); err != nil {
		return fmt.Errorf("delete network txn: %v", err)
	}
	// Best-effort purge of the per-(node, network) status records: they are
	// garbage once the network is gone. A purge failure must not fail the
	// delete, which has already committed.
	s.purgeNetworkNodeStatus(ctx, id)
	return nil
}

// purgeNetworkNodeStatus drops every per-(node, network) status record for the
// network. Best-effort: errors are swallowed because the network's soft-delete
// has already committed and the records are unreferenced garbage.
func (s *Store) purgeNetworkNodeStatus(ctx context.Context, networkID uuid.UUID) {
	items, err := s.c.Range(ctx, networkNodeStatusByNetworkPrefix(networkID))
	if err != nil {
		return
	}
	ops := make([]clientv3.Op, 0, len(items))
	for _, kv := range items {
		ops = append(ops, clientv3.OpDelete(kv.Key))
	}
	if len(ops) == 0 {
		return
	}
	_ = s.commitInChunks(ctx, ops)
}

// countVMNicsOnNetwork counts the active vm_nics referencing the network via
// the maintained index prefix.
func (s *Store) countVMNicsOnNetwork(ctx context.Context, id uuid.UUID) (int64, error) {
	items, err := s.c.Range(ctx, vmNicNetworkIndexPrefix(id))
	if err != nil {
		return 0, err
	}
	return int64(len(items)), nil
}

// UpsertNetworkNodeStatus writes the per-(node, network) reconciliation record,
// stamping last_reconciled_at and updated_at. Last-writer-wins (one writer per
// node), so the call is idempotent.
func (s *Store) UpsertNetworkNodeStatus(ctx context.Context, arg store.UpsertNetworkNodeStatusParams) error {
	now := time.Now().UTC()
	st := store.NetworkNodeStatus{
		NetworkID:            arg.NetworkID,
		NodeID:               arg.NodeID,
		ReconciliationStatus: arg.ReconciliationStatus,
		ReconciliationError:  arg.ReconciliationError,
		LastReconciledAt:     &now,
		UpdatedAt:            now,
	}
	return s.c.PutJSON(ctx, networkNodeStatusKey(arg.NetworkID, arg.NodeID), st)
}

// ListNetworkNodeStatusByNetwork returns every per-node reconciliation record
// for the network, sorted by node id for determinism.
func (s *Store) ListNetworkNodeStatusByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	items, err := s.c.Range(ctx, networkNodeStatusByNetworkPrefix(networkID))
	if err != nil {
		return nil, err
	}
	out := make([]store.NetworkNodeStatus, 0, len(items))
	for _, kv := range items {
		var st store.NetworkNodeStatus
		if err := json.Unmarshal(kv.Value, &st); err != nil {
			return nil, fmt.Errorf("unmarshal network_node_status %q: %v", kv.Key, err)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID.String() < out[j].NodeID.String() })
	return out, nil
}

// ListNetworkNodeStatusByNode returns every per-network reconciliation record
// owned by the node, sorted by network id for determinism. It scans the full
// status keyspace (the records are keyed network-first) and filters by node.
func (s *Store) ListNetworkNodeStatusByNode(ctx context.Context, nodeID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	items, err := s.c.Range(ctx, networkNodeStatusPrefix)
	if err != nil {
		return nil, err
	}
	var out []store.NetworkNodeStatus
	for _, kv := range items {
		var st store.NetworkNodeStatus
		if err := json.Unmarshal(kv.Value, &st); err != nil {
			return nil, fmt.Errorf("unmarshal network_node_status %q: %v", kv.Key, err)
		}
		if st.NodeID == nodeID {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NetworkID.String() < out[j].NetworkID.String() })
	return out, nil
}

// afterCursor reports whether (createdAt, id) sorts strictly after the cursor
// tuple, matching the SQL `(created_at, id) > (cursor_created_at, cursor_id)`.
// A nil cursor (first page) admits every row.
func afterCursor(createdAt time.Time, id uuid.UUID, cursorCreatedAt *time.Time, cursorID *uuid.UUID) bool {
	if cursorCreatedAt == nil || cursorID == nil {
		return true
	}
	if !createdAt.Equal(*cursorCreatedAt) {
		return createdAt.After(*cursorCreatedAt)
	}
	return id.String() > cursorID.String()
}
