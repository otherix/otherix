// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/netid"
	"github.com/otherix/otherix/internal/store"
)

// gateway_memberships record that a gateway node covers an overlay network. The
// primary key is gateway-first (gateway_memberships/<gateway_id>/<network_id>),
// which makes the per-gateway scan a direct prefix range; a per-network
// secondary index (index/gateway_memberships/network/<network_id>/<gateway_id>)
// powers the network-scoped lookups the FDB-style projection needs. Each row
// holds a TenantIP and MAC drawn from the SAME per-network address space as VM
// NICs - the IPv4 reservation guard is vmNicIPv4ReservationKey - so a gateway
// and a VM can never collide on an address.

// gatewayMembershipIPRetries bounds the allocate/CAS loop: on a lost IP-claim
// race the allocator re-picks the next free host, so a handful of attempts
// absorbs concurrent binds before giving up.
const gatewayMembershipIPRetries = 8

func gatewayMembershipKey(gatewayID, networkID uuid.UUID) string {
	return etcd.Key("gateway_memberships", gatewayID.String(), networkID.String())
}

func gatewayMembershipGatewayPrefix(gatewayID uuid.UUID) string {
	return etcd.Key("gateway_memberships", gatewayID.String()) + "/"
}

func gatewayMembershipNetworkIndexKey(networkID, gatewayID uuid.UUID) string {
	return etcd.Key("index", "gateway_memberships", "network", networkID.String(), gatewayID.String())
}

func gatewayMembershipNetworkIndexPrefix(networkID uuid.UUID) string {
	return etcd.Key("index", "gateway_memberships", "network", networkID.String()) + "/"
}

// CreateGatewayMembership records that the gateway covers the overlay network,
// allocating a TenantIP and MAC from the same per-network address space as VM
// NICs. The network must be an overlay with an allocated VNI and a subnet
// (ErrNetworkNotGatewayEligible otherwise). One transaction puts the membership
// row, its per-network index, and the shared vmNicIPv4ReservationKey under a
// CreateRevision==0 guard, so the gateway can never claim an address a VM NIC
// (or another gateway) already holds; a lost IP-claim race re-picks the next
// free host. A membership that already exists returns ErrGatewayMembershipExists.
func (s *Store) CreateGatewayMembership(ctx context.Context, gatewayID, networkID uuid.UUID) (store.GatewayMembership, error) {
	net, err := s.NetworkByID(ctx, networkID)
	if err != nil {
		return store.GatewayMembership{}, err
	}
	if net.Type != store.NetworkTypeOverlay || net.VNI == nil || net.Subnet == nil {
		return store.GatewayMembership{}, store.ErrNetworkNotGatewayEligible
	}
	mac, err := netid.GenerateLocalMAC()
	if err != nil {
		return store.GatewayMembership{}, fmt.Errorf("generate gateway mac: %v", err)
	}

	rowKey := gatewayMembershipKey(gatewayID, networkID)
	for attempt := 0; attempt < gatewayMembershipIPRetries; attempt++ {
		// Reuse the VM-NIC allocator so the gateway draws from the shared taken-set;
		// the overlay gateway is link-local (not in-subnet) so net.Gateway is nil,
		// mirroring the VM-NIC bind.
		ip, aerr := s.allocateNICIPv4(ctx, networkID, *net.Subnet, net.Gateway)
		if aerr != nil {
			return store.GatewayMembership{}, aerr
		}
		m := store.GatewayMembership{
			GatewayID: gatewayID,
			NetworkID: networkID,
			VNI:       *net.VNI,
			MAC:       mac,
			TenantIP:  ip,
			CreatedAt: time.Now().UTC(),
		}
		val, merr := etcd.Marshal(m)
		if merr != nil {
			return store.GatewayMembership{}, merr
		}
		resp, terr := s.c.Raw().Txn(ctx).
			If(
				clientv3.Compare(clientv3.CreateRevision(rowKey), "=", 0),
				clientv3.Compare(clientv3.CreateRevision(vmNicIPv4ReservationKey(networkID, ip)), "=", 0),
			).
			Then(
				clientv3.OpPut(rowKey, string(val)),
				clientv3.OpPut(gatewayMembershipNetworkIndexKey(networkID, gatewayID), gatewayID.String()),
				clientv3.OpPut(vmNicIPv4ReservationKey(networkID, ip), gatewayID.String()),
			).
			Commit()
		if terr != nil {
			return store.GatewayMembership{}, terr
		}
		if resp.Succeeded {
			return m, nil
		}
		// A guard lost. If the membership row already exists the gateway is already
		// a member; otherwise a concurrent claim took the IP, so re-pick the next
		// free host on the following attempt.
		var existing store.GatewayMembership
		found, gerr := s.c.GetJSON(ctx, rowKey, &existing)
		if gerr != nil {
			return store.GatewayMembership{}, gerr
		}
		if found {
			return store.GatewayMembership{}, store.ErrGatewayMembershipExists
		}
	}
	return store.GatewayMembership{}, store.ErrSubnetExhausted
}

// DeleteGatewayMembership removes the gateway's coverage of the network: one
// transaction drops the membership row, its per-network index, and the shared
// IPv4 reservation, so the address frees for reuse exactly when the membership
// is torn down. Deleting an absent membership is a no-op.
func (s *Store) DeleteGatewayMembership(ctx context.Context, gatewayID, networkID uuid.UUID) error {
	rowKey := gatewayMembershipKey(gatewayID, networkID)
	var m store.GatewayMembership
	found, err := s.c.GetJSON(ctx, rowKey, &m)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpDelete(rowKey),
			clientv3.OpDelete(gatewayMembershipNetworkIndexKey(networkID, gatewayID)),
			clientv3.OpDelete(vmNicIPv4ReservationKey(networkID, m.TenantIP)),
		).
		Commit()
	return err
}

// ListGatewayMembershipsForGateway returns every membership the gateway holds,
// a direct range over the gateway-first primary prefix. Ordered by network id
// for determinism.
func (s *Store) ListGatewayMembershipsForGateway(ctx context.Context, gatewayID uuid.UUID) ([]store.GatewayMembership, error) {
	items, err := s.c.Range(ctx, gatewayMembershipGatewayPrefix(gatewayID))
	if err != nil {
		return nil, err
	}
	out := make([]store.GatewayMembership, 0, len(items))
	for _, kv := range items {
		var m store.GatewayMembership
		if uerr := json.Unmarshal(kv.Value, &m); uerr != nil {
			return nil, uerr
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NetworkID.String() < out[j].NetworkID.String() })
	return out, nil
}

// ListGatewayMembershipsForNetwork returns every gateway covering the network,
// resolved through the per-network secondary index. Ordered by gateway id for
// determinism.
func (s *Store) ListGatewayMembershipsForNetwork(ctx context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error) {
	return s.listGatewayMembershipsForNetworkAtRev(ctx, networkID, 0)
}

// ListGatewayMembershipsForNetworkAtRev is ListGatewayMembershipsForNetwork
// pinned to a prior MVCC revision, so a projection joining gateway memberships
// with other state reads one consistent snapshot. Pass rev==0 to read the
// latest revision.
func (s *Store) ListGatewayMembershipsForNetworkAtRev(ctx context.Context, networkID uuid.UUID, rev int64) ([]store.GatewayMembership, error) {
	return s.listGatewayMembershipsForNetworkAtRev(ctx, networkID, rev)
}

func (s *Store) listGatewayMembershipsForNetworkAtRev(ctx context.Context, networkID uuid.UUID, rev int64) ([]store.GatewayMembership, error) {
	idxItems, idxRev, err := s.c.RangeRev(ctx, gatewayMembershipNetworkIndexPrefix(networkID), rev)
	if err != nil {
		return nil, err
	}
	out := make([]store.GatewayMembership, 0, len(idxItems))
	for _, kv := range idxItems {
		gatewayID, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			continue
		}
		var m store.GatewayMembership
		found, gerr := s.c.GetJSONAtRev(ctx, gatewayMembershipKey(gatewayID, networkID), idxRev, &m)
		if gerr != nil {
			return nil, gerr
		}
		if !found {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GatewayID.String() < out[j].GatewayID.String() })
	return out, nil
}
