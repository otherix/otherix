// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// NetworkNodeStatus is the per-(node, network) reconciliation outcome for
// a cluster-wide network materialised node-locally. A
// networks row is one cluster-wide definition; materialisation can
// succeed on some nodes and fail on others, so the status lives here
// keyed by (NetworkID, NodeID) rather than on the Network row.
type NetworkNodeStatus struct {
	NetworkID            uuid.UUID
	NodeID               uuid.UUID
	ReconciliationStatus string // pending | ready | failed
	ReconciliationError  *string
	LastReconciledAt     *time.Time
	UpdatedAt            time.Time
	// ActiveSessions is the gateway's self-reported count of live ingress
	// sessions on this network. It is meaningful only for a gateway node and
	// backs the sticky-membership guard: the coverage reconcile keeps a
	// gateway's overlay membership while this count is above zero so a draining
	// session is never yanked out from under it. Internal observed state; not
	// surfaced through the public NetworkNodeStatus view.
	ActiveSessions int
}

// UpsertNetworkNodeStatusParams updates one per-(node, network)
// reconciliation record from a heartbeat report.
type UpsertNetworkNodeStatusParams struct {
	NetworkID            uuid.UUID
	NodeID               uuid.UUID
	ReconciliationStatus string
	ReconciliationError  *string
	// ActiveSessions carries the gateway's self-reported live-session count for
	// this network (zero for a non-gateway node).
	ActiveSessions int
}
