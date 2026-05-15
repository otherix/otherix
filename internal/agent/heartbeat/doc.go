// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package heartbeat is the agent → CP heartbeat sender. Mirrors the
// CP-side receiver under internal/api/handlers/heartbeat: every
// Interval (default 30 s) the agent collects а full snapshot of node
// capabilities + resources + VM runtime and POSTs it к the CP at
// /v1/nodes/{cfg.NodeID}/heartbeat over mTLS.
//
// The package has three pieces:
//
//   - Collector — interface returning the request body. LinuxCollector
//     reads /proc, runs qemu-system-* --version, and asks the vm.Manager
//     for а live VM list. Tests pass а fake Collector to drive Sender
//     deterministically.
//   - Client — mTLS-aware POST client wrapping net/http. Mirrors the
//     CP→agent client under internal/api/agentclient but in reverse:
//     same cert / CA pair on disk, opposite direction on the wire.
//   - Sender — ticker-driven goroutine. First tick fires immediately
//     on start so а fresh agent flips the node к 'ready' without waiting
//     а full interval.
//
// Errors are non-fatal: а failed collection или а failed POST is logged
// и the loop continues. The agent must not crash because the CP is
// temporarily unreachable.
package heartbeat
