// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

// needsCidata reports whether a CreateSpec requires a NoCloud cidata ISO.
// The seed is built when the operator supplied either cloud-init channel
// (user-data or network-config); an all-empty spec boots without a seed.
func needsCidata(spec CreateSpec) bool {
	return len(spec.UserData) > 0 || len(spec.NetworkData) > 0
}
