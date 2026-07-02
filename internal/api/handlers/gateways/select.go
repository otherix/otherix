// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateways

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// ErrIngressUnavailable reports that no gateway can currently carry ingress for
// the VM: either the VM sits on no overlay network, or no gateway covering one of
// its overlays has reported its reconciliation converged.
var ErrIngressUnavailable = errors.New("ingress unavailable")

// GatewaySelectStore is the storage surface gateway selection needs.
// *etcdstore.Store satisfies it.
type GatewaySelectStore interface {
	ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error)
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	ListGatewayMembershipsForNetwork(ctx context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error)
	ListNetworkNodeStatusByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error)
	AllNodes(ctx context.Context) ([]store.Node, error)
}

// SelectGatewayForVM picks a gateway node to carry ingress for the VM. It
// resolves the VM's overlay networks from its NICs, then among the gateways that
// are members of one of those overlays it keeps only the ones whose gateway node
// is live AND has reported that overlay's reconciliation converged ("ready") -
// a membership alone is not enough, because a gateway that has not yet programmed
// its forwarding database would blackhole the forward. It returns the
// lowest-id eligible gateway for a deterministic choice, or ErrIngressUnavailable
// when none qualifies.
func SelectGatewayForVM(ctx context.Context, st GatewaySelectStore, vmID uuid.UUID) (store.Node, error) {
	overlayIDs, err := vmOverlayNetworkIDs(ctx, st, vmID)
	if err != nil {
		return store.Node{}, err
	}
	if len(overlayIDs) == 0 {
		return store.Node{}, ErrIngressUnavailable
	}

	nodes, err := st.AllNodes(ctx)
	if err != nil {
		return store.Node{}, fmt.Errorf("list nodes: %v", err)
	}
	liveGateways := make(map[uuid.UUID]store.Node, len(nodes))
	for _, n := range nodes {
		if n.HasRole(store.NodeRoleGateway) && nodeLive(n) {
			liveGateways[n.ID] = n
		}
	}

	eligible, err := eligibleGateways(ctx, st, overlayIDs, liveGateways)
	if err != nil {
		return store.Node{}, err
	}
	if len(eligible) == 0 {
		return store.Node{}, ErrIngressUnavailable
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID.String() < eligible[j].ID.String() })
	return eligible[0], nil
}

// vmOverlayNetworkIDs returns the VM's distinct overlay network ids, resolved
// from its NICs. A NIC referencing a missing network is skipped (the VM is being
// torn down); a non-overlay NIC is ignored.
func vmOverlayNetworkIDs(ctx context.Context, st GatewaySelectStore, vmID uuid.UUID) ([]uuid.UUID, error) {
	nics, err := st.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("list vm nics: %v", err)
	}
	out := make([]uuid.UUID, 0, len(nics))
	seen := make(map[uuid.UUID]bool, len(nics))
	for _, nic := range nics {
		if seen[nic.NetworkID] {
			continue
		}
		net, err := st.NetworkByID(ctx, nic.NetworkID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("load network %s: %v", nic.NetworkID, err)
		}
		seen[nic.NetworkID] = true
		if net.Type != store.NetworkTypeOverlay {
			continue
		}
		out = append(out, nic.NetworkID)
	}
	return out, nil
}

// eligibleGateways returns the distinct live gateway nodes that are members of at
// least one of the overlay networks and have reported that overlay converged.
func eligibleGateways(ctx context.Context, st GatewaySelectStore, overlayIDs []uuid.UUID, liveGateways map[uuid.UUID]store.Node) ([]store.Node, error) {
	var out []store.Node
	seen := make(map[uuid.UUID]bool)
	for _, netID := range overlayIDs {
		members, err := st.ListGatewayMembershipsForNetwork(ctx, netID)
		if err != nil {
			return nil, fmt.Errorf("list memberships for network %s: %v", netID, err)
		}
		if len(members) == 0 {
			continue
		}
		ready, err := readyGatewaysForNetwork(ctx, st, netID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			gw, ok := liveGateways[m.GatewayID]
			if !ok || seen[m.GatewayID] || !ready[m.GatewayID] {
				continue
			}
			seen[m.GatewayID] = true
			out = append(out, gw)
		}
	}
	return out, nil
}

// readyGatewaysForNetwork returns the set of node ids that have reported the
// network's reconciliation converged ("ready"). This is the load-bearing gate:
// only a gateway that has programmed its forwarding database for the overlay can
// carry the forward without blackholing it.
func readyGatewaysForNetwork(ctx context.Context, st GatewaySelectStore, networkID uuid.UUID) (map[uuid.UUID]bool, error) {
	statuses, err := st.ListNetworkNodeStatusByNetwork(ctx, networkID)
	if err != nil {
		return nil, fmt.Errorf("list network node status for %s: %v", networkID, err)
	}
	ready := make(map[uuid.UUID]bool, len(statuses))
	for _, s := range statuses {
		if s.ReconciliationStatus == "ready" {
			ready[s.NodeID] = true
		}
	}
	return ready, nil
}
