// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package gateways holds the control-plane logic for VM ingress gateways: the
// periodic coverage reconcile that keeps every ingress-active overlay backed by
// redundant gateway memberships, and the per-VM gateway selection that only ever
// returns a gateway whose overlay reconciliation has converged.
package gateways

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// defaultCoverageTarget is the number of gateway memberships the reconcile keeps
// on each ingress-active overlay so a single gateway loss does not strand
// ingress. Two is the minimum redundant set.
const defaultCoverageTarget = 2

// ReconcileConfig tunes the gateway coverage reconcile.
type ReconcileConfig struct {
	// CoverageTarget is the desired number of gateway memberships per
	// ingress-active overlay network. Values <= 0 fall back to
	// defaultCoverageTarget.
	CoverageTarget int
}

// GatewayReconcileStore is the storage surface the gateway coverage reconcile
// needs. *etcdstore.Store satisfies it.
type GatewayReconcileStore interface {
	ListNetworks(ctx context.Context, arg store.ListNetworksParams) ([]store.Network, error)
	ListVMNicsByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.VMNic, error)
	AllNodes(ctx context.Context) ([]store.Node, error)
	ListGatewayMembershipsForNetwork(ctx context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error)
	CreateGatewayMembership(ctx context.Context, gatewayID, networkID uuid.UUID) (store.GatewayMembership, error)
}

// ReconcileFunc returns the periodic gateway coverage reconcile pass. For every
// overlay network that carries at least one VM NIC (the ingress-active signal),
// it ensures the network is covered by at least CoverageTarget gateway
// memberships, creating the shortfall on live gateway nodes chosen by rendezvous
// hash so coverage spreads across the gateway fleet. The pass is purely additive:
// it never removes a membership (sticky teardown is a later concern), so a single
// pass can only raise coverage. It is idempotent (a network already at target is
// left untouched) and fail-open (a transient list or create error is logged and
// retried next tick, never failing the whole pass). With fewer than CoverageTarget
// live gateways it creates as many as it can and moves on.
func ReconcileFunc(st GatewayReconcileStore, cfg ReconcileConfig, log *slog.Logger) func(context.Context) error {
	target := cfg.CoverageTarget
	if target <= 0 {
		target = defaultCoverageTarget
	}
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
		for _, n := range networks {
			if err := reconcileNetwork(ctx, st, log, n, liveGateways, target); err != nil {
				log.WarnContext(ctx, "gateway coverage reconcile: network pass failed",
					slog.String("network_id", n.ID.String()), slog.Any("error", err))
			}
		}
		return nil
	}
}

// reconcileNetwork raises the gateway coverage of a single overlay network to
// target when the network is ingress-active. Returns an error only on a store
// read failure; create failures are best-effort and do not propagate.
func reconcileNetwork(ctx context.Context, st GatewayReconcileStore, log *slog.Logger, n store.Network, liveGateways []uuid.UUID, target int) error {
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
	need := target - len(members)
	if need <= 0 {
		return nil
	}
	memberSet := make(map[uuid.UUID]bool, len(members))
	for _, m := range members {
		memberSet[m.GatewayID] = true
	}
	candidates := make([]uuid.UUID, 0, len(liveGateways))
	for _, id := range liveGateways {
		if !memberSet[id] {
			candidates = append(candidates, id)
		}
	}
	for _, gw := range spreadTargets(n.ID, candidates, need) {
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

// liveGatewayIDs returns the ids of gateway-kind nodes that may take ingress: not
// soft-deleted and not in a terminal-or-stale status (unreachable/gone).
func liveGatewayIDs(nodes []store.Node) []uuid.UUID {
	var out []uuid.UUID
	for _, n := range nodes {
		if n.Kind != store.NodeKindGateway {
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

// spreadTargets picks up to need gateway ids from candidates by descending
// rendezvous (highest-random-weight) score for the network, ties broken by id.
// Keying on the network spreads coverage across the gateway fleet rather than
// piling every overlay onto the same gateways. Deterministic; input order does
// not affect the result.
func spreadTargets(networkID uuid.UUID, candidates []uuid.UUID, need int) []uuid.UUID {
	if need <= 0 || len(candidates) == 0 {
		return nil
	}
	sorted := make([]uuid.UUID, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		wi, wj := hrwWeight(networkID, sorted[i]), hrwWeight(networkID, sorted[j])
		if c := bytes.Compare(wi[:], wj[:]); c != 0 {
			return c > 0
		}
		return sorted[i].String() < sorted[j].String()
	})
	if need > len(sorted) {
		need = len(sorted)
	}
	return sorted[:need]
}

// hrwWeight is the rendezvous score of covering networkID with gatewayID:
// sha256(networkID || gatewayID). Deterministic and stable across fleet changes.
func hrwWeight(networkID, gatewayID uuid.UUID) [sha256.Size]byte {
	h := sha256.New()
	h.Write(networkID[:])
	h.Write(gatewayID[:])
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}
