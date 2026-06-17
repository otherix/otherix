// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

// needsCidata reports whether a CreateSpec requires a NoCloud cidata ISO.
// Every VM gets a minimal seed unless cloud-init was explicitly disabled, so a
// name-only VM (no user-data, no network-config) still gets instance-id +
// local-hostname = its name and the guest hostname reliably follows the VM
// name. CloudInitDisabled is the single, explicit opt-out (the operator's
// --no-cloud-init). This is a deliberate behavior change from the prior
// "seed only when user-data or network-config was supplied" rule.
func needsCidata(spec CreateSpec) bool {
	return !spec.CloudInitDisabled
}
