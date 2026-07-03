// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package gateways holds the control-plane logic for VM ingress gateways: the
// periodic coverage reconcile that places a gateway membership on every live
// gateway node for every ingress-active overlay, and the per-VM gateway
// selection that only ever returns a gateway whose overlay reconciliation has
// converged.
package gateways

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// ReconcileConfig tunes the gateway coverage reconcile. It carries no fields
// today; it is retained as the config seam for future tunables.
type ReconcileConfig struct{}

// GatewayReconcileStore is the storage surface the gateway coverage reconcile
// needs. *etcdstore.Store satisfies it.
type GatewayReconcileStore interface {
	ListNetworks(ctx context.Context, arg store.ListNetworksParams) ([]store.Network, error)
	ListVMNicsByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.VMNic, error)
	AllNodes(ctx context.Context) ([]store.Node, error)
	ListGatewayMembershipsForNetwork(ctx context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error)
	CreateGatewayMembership(ctx context.Context, gatewayID, networkID uuid.UUID) (store.GatewayMembership, error)
	ListNetworkNodeStatusByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error)
	DeleteGatewayMembership(ctx context.Context, gatewayID, networkID uuid.UUID) error
}

// ReconcileFunc returns the periodic gateway coverage reconcile pass. It runs two
// passes over every overlay network. The additive pass: for a network that
// carries at least one VM NIC (the ingress-active signal) it places a gateway
// membership on every live gateway node that does not already have one - a plain
// cross-product of live gateway nodes and ingress-active overlays, no redundancy
// target and no spread. The reaping pass: it removes a membership that has become
// unnecessary - the network has gone ingress-inactive (its last VM NIC removed)
// or the gateway node has died - guarded so a membership a live session still
// depends on is never reaped (see reapNetwork). Both passes are idempotent (a
// network already covered by every live gateway with no stale memberships is left
// untouched) and fail-open (a transient list, create, or delete error is logged
// and retried next tick, never failing the whole pass).
func ReconcileFunc(st GatewayReconcileStore, _ ReconcileConfig, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		overlay := store.NetworkTypeOverlay
		networks, err := st.ListNetworks(ctx, store.ListNetworksParams{Type: &overlay})
		if err != nil {
			return fmt.Errorf("list overlay networks: %v", err)
		}
		nodes, err := st.AllNodes(ctx)
		if err != nil {
			return fmt.Errorf("list nodes: %v", err)
		}
		liveGateways := liveGatewayIDs(nodes)
		nodeByID := make(map[uuid.UUID]store.Node, len(nodes))
		for _, n := range nodes {
			nodeByID[n.ID] = n
		}
		for _, n := range networks {
			if err := reconcileNetwork(ctx, st, log, n, liveGateways); err != nil {
				log.WarnContext(ctx, "gateway coverage reconcile: network pass failed",
					slog.String("network_id", n.ID.String()), slog.Any("error", err))
			}
			if err := reapNetwork(ctx, st, log, n, nodeByID); err != nil {
				log.WarnContext(ctx, "gateway coverage reconcile: reap pass failed",
					slog.String("network_id", n.ID.String()), slog.Any("error", err))
			}
		}
		return nil
	}
}

// reconcileNetwork places a gateway membership on every live gateway node for a
// single overlay network when the network is ingress-active. It is the CP half of
// a plain cross-product (live gateway nodes x ingress-active overlays): there is
// no redundancy target and no spread, so a node holding the gateway role covers
// every overlay that carries a VM.
//
// IP-consumption coupling: each membership allocates a tenant IP from the same
// per-network /24 as VM NICs (CreateGatewayMembership -> allocateNICIPv4 ->
// vmNicIPv4ReservationKey), so the cross-product consumes one IP per gateway-role
// node on every ingress-active overlay, competing with VM-NIC allocation on a
// small subnet. There is deliberately no per-node membership cap here. This fails
// toward inaction: a full subnet makes CreateGatewayMembership return
// store.ErrSubnetExhausted, which is logged and skipped (like any create failure)
// so a single exhausted network never wedges the pass or blocks other networks.
//
// Returns an error only on a store read failure; create failures are best-effort
// and do not propagate.
func reconcileNetwork(ctx context.Context, st GatewayReconcileStore, log *slog.Logger, n store.Network, liveGateways []uuid.UUID) error {
	nics, err := st.ListVMNicsByNetwork(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("list nics: %v", err)
	}
	if len(nics) == 0 {
		// No VM sits on this overlay, so it is not ingress-active yet.
		return nil
	}
	members, err := st.ListGatewayMembershipsForNetwork(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("list memberships: %v", err)
	}
	memberSet := make(map[uuid.UUID]bool, len(members))
	for _, m := range members {
		memberSet[m.GatewayID] = true
	}
	for _, gw := range liveGateways {
		if memberSet[gw] {
			continue
		}
		if _, err := st.CreateGatewayMembership(ctx, gw, n.ID); err != nil {
			if errors.Is(err, store.ErrGatewayMembershipExists) {
				continue
			}
			log.WarnContext(ctx, "gateway coverage reconcile: create membership failed",
				slog.String("network_id", n.ID.String()),
				slog.String("gateway_id", gw.String()), slog.Any("error", err))
			continue
		}
		log.InfoContext(ctx, "gateway coverage reconcile: added gateway membership",
			slog.String("network_id", n.ID.String()), slog.String("gateway_id", gw.String()))
	}
	return nil
}

// reapNetwork removes gateway memberships that have become unnecessary on a
// single overlay network, while keeping any membership a live ingress session
// still depends on. A membership (gw, net) is a reap candidate when the network
// is no longer ingress-active (no VM NIC), its gateway node is not live
// (unreachable/gone/soft-deleted/absent), or its gateway node is live but no
// longer holds the gateway role (an operator ran gateway disable). The reap is
// destructive, so it fails toward inaction and enumerates the taxonomy
// explicitly:
//
//   - live + role on + network active        -> keep (this is the coverage the
//     additive pass maintains; not a reap candidate)
//   - live + (role off OR inactive) + >0 sess -> keep (a session is draining; the
//     sticky guard refuses to yank its coverage until the count reaches zero)
//   - live + (role off OR inactive) + 0 sess  -> reap (idle membership leak, or a
//     drained node whose role was turned off)
//   - gateway not live                        -> reap regardless of the
//     last-reported count (a dead gateway cannot hold a live session, and a
//     stale count must not wedge the reaper forever)
//
// The sticky guard keys on the gateway's own self-reported active-session count
// (network_node_status.active_sessions), a fail-closed signal: a live gateway is
// reaped only when it is no longer a coverage candidate (role off or the network
// inactive) AND it reports zero sessions. Best-effort throughout - a delete
// failure is logged and retried next tick; only a store read failure propagates
// so the caller can log it.
func reapNetwork(ctx context.Context, st GatewayReconcileStore, log *slog.Logger, n store.Network, nodeByID map[uuid.UUID]store.Node) error {
	members, err := st.ListGatewayMembershipsForNetwork(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("list memberships: %v", err)
	}
	if len(members) == 0 {
		return nil
	}
	nics, err := st.ListVMNicsByNetwork(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("list nics: %v", err)
	}
	networkActive := len(nics) > 0

	// Lazily fetched: the per-gateway session counts are needed only to decide a
	// live gateway's membership that is no longer a coverage candidate (role off
	// or the network inactive), never for the active-coverage or dead-gateway
	// cases.
	var sessions map[uuid.UUID]int
	for _, m := range members {
		gw, found := nodeByID[m.GatewayID]
		keep, live := memberDisposition(gw, found, networkActive)
		if keep {
			continue // keep: live gateway, role on, active overlay
		}
		// Reap candidate (inactive overlay, dead node, or role off). Never cut a
		// live session: a live gateway still draining is kept until its
		// self-reported active-session count reaches zero.
		if live {
			if sessions == nil {
				sessions, err = sessionCountsByGateway(ctx, st, n.ID)
				if err != nil {
					return fmt.Errorf("list network node status: %v", err)
				}
			}
			if sessions[m.GatewayID] > 0 {
				continue
			}
		}
		if err := st.DeleteGatewayMembership(ctx, m.GatewayID, n.ID); err != nil {
			log.WarnContext(ctx, "gateway coverage reconcile: reap membership failed",
				slog.String("network_id", n.ID.String()),
				slog.String("gateway_id", m.GatewayID.String()), slog.Any("error", err))
			continue
		}
		log.InfoContext(ctx, "gateway coverage reconcile: reaped gateway membership",
			slog.String("network_id", n.ID.String()),
			slog.String("gateway_id", m.GatewayID.String()),
			slog.Bool("network_active", networkActive),
			slog.Bool("gateway_live", live))
	}
	return nil
}

// memberDisposition classifies one gateway membership for the coverage reaper.
// keep is true only for active coverage - a live gateway whose role is on while
// the overlay is active - and such a membership is never a reap candidate. When
// keep is false the membership is a reap candidate; live reports whether the
// gateway is still live, in which case the caller applies the sticky
// active-session guard before deleting. It is the branch-free classifier of the
// keep/reap table documented on reapNetwork; the session lazy-fetch stays in the
// caller so it runs only for a live reap candidate.
func memberDisposition(gw store.Node, found, networkActive bool) (keep, live bool) {
	live = found && nodeLive(gw)
	roleOn := found && gw.HasRole(store.NodeRoleGateway)
	keep = live && roleOn && networkActive
	return keep, live
}

// sessionCountsByGateway returns the gateway-self-reported live ingress session
// count per node for the network, drawn from the per-(node, network) status
// records. A gateway with no record is absent from the map and reads as zero.
func sessionCountsByGateway(ctx context.Context, st GatewayReconcileStore, networkID uuid.UUID) (map[uuid.UUID]int, error) {
	statuses, err := st.ListNetworkNodeStatusByNetwork(ctx, networkID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(statuses))
	for _, s := range statuses {
		out[s.NodeID] = s.ActiveSessions
	}
	return out, nil
}

// liveGatewayIDs returns the ids of gateway-role nodes that may take ingress: not
// soft-deleted and not in a terminal-or-stale status (unreachable/gone).
func liveGatewayIDs(nodes []store.Node) []uuid.UUID {
	var out []uuid.UUID
	for _, n := range nodes {
		if !n.HasRole(store.NodeRoleGateway) {
			continue
		}
		if !nodeLive(n) {
			continue
		}
		out = append(out, n.ID)
	}
	return out
}

// nodeLive reports whether a node may carry traffic: not soft-deleted and not in
// a terminal-or-stale status.
func nodeLive(n store.Node) bool {
	return n.DeletedAt == nil &&
		n.Status != store.NodeStatusUnreachable &&
		n.Status != store.NodeStatusGone
}
