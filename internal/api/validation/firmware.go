// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/otherix/otherix/internal/store"
)

// FirmwareNameMaxLength bounds firmwares.name and matches
// `Firmware.name.maxLength` in api/openapi/control-plane.yaml. The SQL
// column is unbounded text; the cap is API-edge only.
const FirmwareNameMaxLength = 255

// FirmwareVersionMaxLength bounds firmwares.version when present.
// version is nullable in the schema, but when supplied it shares the
// same 255-rune cap as other identifier-shaped strings on the API
// surface.
const FirmwareVersionMaxLength = 255

// ValidateArchitecture returns nil when s is a recognised
// store.CPUArch enum value (`amd64` or `arm64`). The validator is
// shared across resources whose API surface carries the architecture
// (firmwares, vms) so a single source of truth
// controls the wire-level enum.
func ValidateArchitecture(s string) error {
	switch store.CPUArch(s) {
	case store.CpuArchAmd64, store.CpuArchArm64:
		return nil
	}
	return fmt.Errorf("invalid architecture %q (must be one of: amd64, arm64)", s)
}

// ValidateFirmwareType returns nil when s is a recognised
// store.FirmwareType enum value (`bios` or `uefi`). Anchored to the
// SQL enum; adding a new firmware family means extending both the
// migration and this branch.
func ValidateFirmwareType(s string) error {
	switch store.FirmwareType(s) {
	case store.FirmwareTypeBios, store.FirmwareTypeUefi:
		return nil
	}
	return fmt.Errorf("invalid firmware type %q (must be one of: bios, uefi)", s)
}

// ValidateFirmwareName returns nil when s is a syntactically valid
// firmware name: 1..255 runes, no leading or trailing whitespace.
// The schema enforces (name, architecture) global uniqueness via
// uq_firmwares_name_arch; this helper only catches malformed input.
func ValidateFirmwareName(s string) error {
	if s == "" {
		return errors.New("name is required")
	}
	if s != strings.TrimSpace(s) {
		return errors.New("name must not have leading or trailing whitespace")
	}
	if utf8.RuneCountInString(s) > FirmwareNameMaxLength {
		return fmt.Errorf("name is too long (max %d runes)", FirmwareNameMaxLength)
	}
	return nil
}

// ValidateFirmwareVersion returns nil when s is acceptable as the
// firmwares.version column value. The empty string is allowed and
// represents "version not specified" — callers convert it to NULL
// before persisting. Non-empty strings must fit the rune cap and
// must not contain ASCII control characters (which break logs and
// most rendering paths and have no plausible meaning in a version
// label).
func ValidateFirmwareVersion(s string) error {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > FirmwareVersionMaxLength {
		return fmt.Errorf("version is too long (max %d runes)", FirmwareVersionMaxLength)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("version must not contain control characters")
		}
	}
	return nil
}
