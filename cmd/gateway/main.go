// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command otherix-gateway is the VM-less ingress gateway process. It joins
// the cluster, brings up the WireGuard mesh and the declared overlays'
// datapath (bridge + VXLAN + FDB + its own tenant address), and heartbeats -
// but never runs the anycast services plane or any VM/qemu surface. Cobra
// root with two subcommands:
//
//   - `serve` (default — preserves bare-invocation back-compat for systemd
//     units): boots the gateway runtime, polls for cert material +
//     gateway.yaml, transitions to State B once all four files are present
//     and valid.
//   - `bootstrap`: one-shot operator-driven join flow. Reads token +
//     ca-fingerprint + cp-url + node-name + advertised-endpoint from CLI
//     flags, executes the gateway join protocol, writes cert material +
//     gateway.yaml to disk, exits. Idempotent — a repeat invocation without
//     --force on a bootstrapped host exits 0.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
