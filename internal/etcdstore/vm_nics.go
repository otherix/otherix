// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// vm_nics are addressed by UUID with two secondary indexes: a per-VM index that
// powers ListVMNicsByVM, and the per-network index DeleteNetwork counts to block
// deletion of a referenced network (vmNicNetworkIndexPrefix lives in
// networks.go, the slice that consumes it).

func vmNicKey(id uuid.UUID) string { return etcd.Key("vm_nics", id.String()) }

func vmNicVMIndexKey(vmID, nicID uuid.UUID) string {
	return etcd.Key("index", "vm_nics", "vm", vmID.String(), nicID.String())
}

func vmNicVMIndexPrefix(vmID uuid.UUID) string {
	return etcd.Key("index", "vm_nics", "vm", vmID.String()) + "/"
}

func vmNicNetworkIndexKey(networkID, nicID uuid.UUID) string {
	return etcd.Key("index", "vm_nics", "network", networkID.String(), nicID.String())
}

// vmNicFromCreateParams projects CreateVMNicParams onto a store.VMNic, stamping
// timestamps and defaulting generation to 1 (matching the VM/disk create
// projections).
func vmNicFromCreateParams(p store.CreateVMNicParams, now time.Time) store.VMNic {
	return store.VMNic{
		ID:          p.ID,
		VmID:        p.VmID,
		NetworkID:   p.NetworkID,
		DeviceOrder: p.DeviceOrder,
		Model:       p.Model,
		MacAddress:  p.MacAddress,
		Ipv4Address: p.Ipv4Address,
		Ipv6Address: p.Ipv6Address,
		Generation:  1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// vmNicCreateOps returns the primary-row + index writes for a NIC: the row, the
// per-VM index (ListVMNicsByVM), and the per-network index (network
// delete-block). The caller threads these into the VM-create transaction.
func vmNicCreateOps(n store.VMNic) ([]clientv3.Op, error) {
	val, err := etcd.Marshal(n)
	if err != nil {
		return nil, err
	}
	return []clientv3.Op{
		clientv3.OpPut(vmNicKey(n.ID), string(val)),
		clientv3.OpPut(vmNicVMIndexKey(n.VmID, n.ID), n.ID.String()),
		clientv3.OpPut(vmNicNetworkIndexKey(n.NetworkID, n.ID), n.ID.String()),
	}, nil
}

// ListVMNicsByVM returns the non-deleted NICs attached to the VM, ordered by
// device_order then id for determinism.
func (s *Store) ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error) {
	items, err := s.c.Range(ctx, vmNicVMIndexPrefix(vmID))
	if err != nil {
		return nil, err
	}
	out := make([]store.VMNic, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			continue
		}
		var n store.VMNic
		found, gerr := s.c.GetJSON(ctx, vmNicKey(id), &n)
		if gerr != nil {
			return nil, gerr
		}
		if !found || n.DeletedAt != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceOrder != out[j].DeviceOrder {
			return out[i].DeviceOrder < out[j].DeviceOrder
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}
