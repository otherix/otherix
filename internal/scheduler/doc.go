// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package scheduler implements VM placement decisions. Runs in-process
// inside otherix-api, on the replica that holds the worker election.
// store.LockKeyPlacement is process-local: it serializes placement inside the
// worker leader; cross-replica exclusion comes from leader-only workers and
// the leader fence on the bind txn.
package scheduler
