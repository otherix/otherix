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
//   - `node_firmwares` — one upsert per firmware in the report whose
//     (name, architecture, type) matches a registered firmwares row.
//     Unmatched entries are skipped with a WARN log;
//   - `vm_runtime` — one upsert per reported VM whose vm_uuid exists
//     in `vms`. Unknown ids are skipped with a WARN log.
//
// The full projection runs in a single transaction so a partially
// applied heartbeat cannot reach the database.
package heartbeat

import (
	"context"
	"log/slog"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the heartbeat receiver depends on: the
// out-of-transaction node-name lookup plus the projection transaction
// seam. *store.Store satisfies it; depending on the interface rather
// than the concrete store is the Phase 2 seam that lets a second backend
// (Phase 3) be substituted under the same handler tests. The
// transactional projection drives store.HeartbeatTx inside InHeartbeatTx.
type Store interface {
	NodeByName(ctx context.Context, name string) (store.Node, error)
	InHeartbeatTx(ctx context.Context, fn func(store.HeartbeatTx) error) error
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
