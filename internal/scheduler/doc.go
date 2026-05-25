// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package scheduler implements VM placement decisions. Runs in-process
// inside otherix-api; concurrent replicas serialize placement via
// store.LockKeyPlacement.
package scheduler
