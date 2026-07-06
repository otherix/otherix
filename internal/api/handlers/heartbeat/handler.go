// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package heartbeat implements the CP-side receiver for
// `POST /v1/nodes/{id}/heartbeat` (operationId `agents.heartbeat`).
// The route is mounted on the agent-only mTLS listener, behind the
// agentMTLS middleware that resolves a verified client cert
// fingerprint to a registered node id (auth.Agent in context).
//
// On a successful call the handler projects the report into:
//   - `nodes.*` — capability columns, jsonb-merged extras, resources,
//     `last_heartbeat_at`, `agent_version`, optional migration triple;
//   - `vm_runtime` — one upsert per reported VM whose vm_uuid exists
//     in `vms`. Unknown ids are skipped with a WARN log.
//
// The projection steps are a sequence of independent writes, not a single
// transaction. The node capability + `last_heartbeat_at` write is stamped LAST,
// after every observed/declared step has committed, so a heartbeat whose later
// steps persistently fail cannot keep refreshing liveness while its observed
// state never lands.
package heartbeat

import (
	"context"
	"log/slog"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the heartbeat receiver depends on: the
// node-name lookup plus the projection seam. *etcdstore.Store satisfies
// it; depending on the interface rather than the concrete store narrows
// the handler's storage dependency to the methods it uses and lets tests
// substitute a fake. The reconcile drives store.HeartbeatProjection
// inside RunHeartbeatProjection.
type Store interface {
	NodeByName(ctx context.Context, name string) (store.Node, error)
	RunHeartbeatProjection(ctx context.Context, fn func(store.HeartbeatProjection) error) error
	ActiveSessionCA(ctx context.Context) (store.SessionCA, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the heartbeat receiver.
//
// `pressureMemory` / `pressureSystemDisk` carry the operator-configured
// pressure detection knobs. Disabling either via
// `placement.pressure.{memory,system_disk}.enabled: false` is honoured
// by computePressureTransition — the receiver still runs the rest of
// the heartbeat projection unchanged.
type Handler struct {
	store              Store
	log                *slog.Logger
	pressureMemory     config.PressureConditionConfig
	pressureSystemDisk config.PressureConditionConfig
}

// New constructs a Handler.
func New(s Store, log *slog.Logger, pressureMemory, pressureSystemDisk config.PressureConditionConfig) *Handler {
	return &Handler{
		store:              s,
		log:                log,
		pressureMemory:     pressureMemory,
		pressureSystemDisk: pressureSystemDisk,
	}
}
