// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import "github.com/otherix/otherix/internal/store"

// machineTypeFor returns the qemu machine type the CP picks for a
// fresh VM whose architecture is arch. The schema requires a non-null
// `vms.machine_type`; VMs do not carry a per-VM machine_type
// override yet. The mapping mirrors the agent's defaults:
//
//   - amd64 → q35  (modern Intel chipset, KVM-friendly defaults)
//   - arm64 → virt (the canonical ARM virtual machine type)
//
// Unknown architectures fall back to "q35" rather than failing —
// schema-level NOT NULL leaves no zero-value option, and the agent
// will surface an arch / machine mismatch on launch with an explicit
// `qemu_spawn_failed` envelope. The helper derives the machine type
// from the architecture alone.
func machineTypeFor(arch store.CPUArch) string {
	switch arch {
	case store.CpuArchArm64:
		return "virt"
	case store.CpuArchAmd64:
		return "q35"
	}
	return "q35"
}
