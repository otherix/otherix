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
}

// UpsertNetworkNodeStatusParams updates one per-(node, network)
// reconciliation record from a heartbeat report.
type UpsertNetworkNodeStatusParams struct {
	NetworkID            uuid.UUID
	NodeID               uuid.UUID
	ReconciliationStatus string
	ReconciliationError  *string
}
