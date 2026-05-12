// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"encoding/json"
	"fmt"

	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// validationFailure carries a single field-shaped reason out of
// validateRequest.
type validationFailure struct {
	field   string
	message string
}

// validateRequest enforces the OpenAPI HeartbeatRequest constraints
// the schema can express (required fields, enum values, basic
// ranges). It is intentionally not a full JSON-schema validator;
// shape mismatches that pass here surface as DB errors and become
// 500 internal — acceptable for an authenticated agent surface.
func validateRequest(b *requestBody) *validationFailure {
	if b.AgentVersion == "" {
		return &validationFailure{field: "agent_version", message: "agent_version is required"}
	}
	if err := validation.ValidateArchitecture(b.Architecture); err != nil {
		return &validationFailure{field: "architecture", message: err.Error()}
	}
	if v := validateCapabilities(&b.Capabilities); v != nil {
		return v
	}
	if v := validateResources(&b.Resources); v != nil {
		return v
	}
	if v := validateFirmwares(b.Capabilities.Firmwares); v != nil {
		return v
	}
	if v := validateVMs(b.VMs); v != nil {
		return v
	}
	if v := validateMigration(b.Migration); v != nil {
		return v
	}
	return nil
}

func validateCapabilities(c *nodeCapabilitiesReport) *validationFailure {
	if c.CPUModel == "" {
		return &validationFailure{field: "capabilities.cpu_model", message: "cpu_model is required"}
	}
	if c.CPUCoresTotal < 1 {
		return &validationFailure{field: "capabilities.cpu_cores_total", message: "cpu_cores_total must be >= 1"}
	}
	if c.MemoryTotalMib < 0 {
		return &validationFailure{field: "capabilities.memory_total_mib", message: "memory_total_mib must be >= 0"}
	}
	if c.KernelVersion == "" {
		return &validationFailure{field: "capabilities.kernel_version", message: "kernel_version is required"}
	}
	if c.QemuVersion == "" {
		return &validationFailure{field: "capabilities.qemu_version", message: "qemu_version is required"}
	}
	return nil
}

func validateResources(r *nodeResourcesReport) *validationFailure {
	if r.CPUCoresAvailable < 0 {
		return &validationFailure{field: "resources.cpu_cores_available", message: "cpu_cores_available must be >= 0"}
	}
	if r.MemoryAvailableMib < 0 {
		return &validationFailure{field: "resources.memory_available_mib", message: "memory_available_mib must be >= 0"}
	}
	if r.SystemDiskTotalBytes != nil && *r.SystemDiskTotalBytes < 0 {
		return &validationFailure{field: "resources.system_disk_total_bytes", message: "system_disk_total_bytes must be >= 0"}
	}
	if r.SystemDiskAvailableBytes != nil && *r.SystemDiskAvailableBytes < 0 {
		return &validationFailure{field: "resources.system_disk_available_bytes", message: "system_disk_available_bytes must be >= 0"}
	}
	return nil
}

func validateFirmwares(reports []firmwareReport) *validationFailure {
	for i, fr := range reports {
		if fr.Name == "" {
			return &validationFailure{field: fmt.Sprintf("capabilities.firmwares[%d].name", i), message: "name is required"}
		}
		if err := validation.ValidateArchitecture(fr.Architecture); err != nil {
			return &validationFailure{field: fmt.Sprintf("capabilities.firmwares[%d].architecture", i), message: err.Error()}
		}
		if err := validation.ValidateFirmwareType(fr.Type); err != nil {
			return &validationFailure{field: fmt.Sprintf("capabilities.firmwares[%d].type", i), message: err.Error()}
		}
		if fr.CodePath == "" {
			return &validationFailure{field: fmt.Sprintf("capabilities.firmwares[%d].code_path", i), message: "code_path is required"}
		}
	}
	return nil
}

func validateVMs(reports []vmReport) *validationFailure {
	for i, vr := range reports {
		if !validVMPhase(vr.Phase) {
			return &validationFailure{field: fmt.Sprintf("vms[%d].phase", i), message: fmt.Sprintf("invalid phase %q", vr.Phase)}
		}
	}
	return nil
}

func validateMigration(m *migrationCapability) *validationFailure {
	if m == nil {
		return nil
	}
	if m.Host == "" {
		return &validationFailure{field: "migration.host", message: "host is required when migration is set"}
	}
	if m.PortRangeStart < 1024 || m.PortRangeStart > 65535 {
		return &validationFailure{field: "migration.port_range_start", message: "port_range_start must be in [1024, 65535]"}
	}
	if m.PortRangeEnd < 1024 || m.PortRangeEnd > 65535 {
		return &validationFailure{field: "migration.port_range_end", message: "port_range_end must be in [1024, 65535]"}
	}
	if m.PortRangeEnd < m.PortRangeStart {
		return &validationFailure{field: "migration.port_range_end", message: "port_range_end must be >= port_range_start"}
	}
	return nil
}

func validVMPhase(p string) bool {
	switch store.VmPhase(p) {
	case store.VmPhasePending, store.VmPhaseRunning, store.VmPhasePaused,
		store.VmPhaseStopped, store.VmPhaseError, store.VmPhaseGone:
		return true
	}
	return false
}

// buildCapabilitiesJSON projects the heartbeat-reported capability
// fields that have no dedicated nodes column into the
// nodes.capabilities jsonb. kvm_available, nested_virt, and
// qemu_binaries land here per the NodeCapabilitiesReport mapping.
func buildCapabilitiesJSON(c nodeCapabilitiesReport) ([]byte, error) {
	binaries := c.QemuBinaries
	if binaries == nil {
		binaries = map[string]string{}
	}
	payload := map[string]any{
		"kvm_available": c.KvmAvailable,
		"nested_virt":   c.NestedVirt,
		"qemu_binaries": binaries,
	}
	return json.Marshal(payload)
}

// buildNumaJSON marshals the optional numa_topology object back to
// jsonb. nil topology lands as SQL NULL via the empty []byte path.
func buildNumaJSON(topology map[string]any) ([]byte, error) {
	if topology == nil {
		return nil, nil
	}
	return json.Marshal(topology)
}
