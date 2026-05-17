// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command otherix-agent is the per-node agent process. Cobra root с
// two subcommands:
//
//   - `serve` (default — preserves bare-invocation back-compat для
//     systemd units): boots the agent runtime, polls для cert
//     material + agent-config.yml, transitions к State B once все
//     four files are present и valid.
//   - `bootstrap`: one-shot operator-driven join flow. Reads token +
//     ca-fingerprint + cp-url + node-name + advertised-endpoint от
//     CLI flags, executes the join protocol, writes cert material +
//     agent-config.yml к disk, exits. Idempotent — а repeat invocation
//     без --force on а bootstrapped host exits 0 с а "already
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
