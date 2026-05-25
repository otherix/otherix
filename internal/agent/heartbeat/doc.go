// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package heartbeat is the agent → CP heartbeat sender. Mirrors the
// CP-side receiver under internal/api/handlers/heartbeat: every
// Interval (default 30 s) the agent collects a full snapshot of node
// capabilities + resources + VM runtime and POSTs it to the CP at
// /v1/nodes/{cfg.NodeID}/heartbeat over mTLS.
//
// The package has three pieces:
//
//   - Collector — interface returning the request body. LinuxCollector
//     reads /proc, runs qemu-system-* --version, and asks the vm.Manager
//     for a live VM list. Tests pass a fake Collector to drive Sender
//     deterministically.
//   - Client — mTLS-aware POST client wrapping net/http. Mirrors the
//     CP→agent client under internal/api/agentclient but in reverse:
//     same cert / CA pair on disk, opposite direction on the wire.
//   - Sender — ticker-driven goroutine. First tick fires immediately
//     on start so a fresh agent flips the node to 'ready' without waiting
//     a full interval.
//
// Errors are non-fatal: a failed collection or a failed POST is logged
// and the loop continues. The agent must not crash because the CP is
// temporarily unreachable.
package heartbeat
