// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command otherix-agent is the per-node agent process. Cobra root with
// two subcommands:
//
//   - `serve` (default — preserves bare-invocation back-compat for
//     systemd units): boots the agent runtime, polls for cert
//     material + agent.yaml, transitions to State B once all
//     four files are present and valid.
//   - `bootstrap`: one-shot operator-driven join flow. Reads token +
//     ca-fingerprint + cp-url + node-name + advertised-endpoint from
//     CLI flags, executes the join protocol, writes cert material +
//     agent.yaml to disk, exits. Idempotent — a repeat invocation
//     without --force on a bootstrapped host exits 0 with a "already
//     bootstrapped" message.
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
