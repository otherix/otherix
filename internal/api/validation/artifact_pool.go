// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/otherix/otherix/internal/store"
)

// ArtifactPoolNameMaxLength bounds an artifact pool name (runes), matching the
// storage-pool name bound.
const ArtifactPoolNameMaxLength = 255

var artifactPoolNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateArtifactPoolName returns nil when name is a syntactically valid
// artifact pool name: 1..255 runes, leading alphanumeric, alphabet
// [A-Za-z0-9._-], no '/' or whitespace (the name forms the etcd name-guard key
// path and the future on-node dir).
func ValidateArtifactPoolName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if utf8.RuneCountInString(name) > ArtifactPoolNameMaxLength {
		return fmt.Errorf("name is too long (max %d)", ArtifactPoolNameMaxLength)
	}
	if !artifactPoolNameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q (must start alphanumeric; allowed [A-Za-z0-9._-])", name)
	}
	return nil
}

// ValidateReplicationFactor returns nil for the "all" sentinel or an integer
// count >= 1. Raw 0 (and negatives) are rejected: "everywhere" is reachable only
// via the explicit "all" sentinel, never a stray zero.
func ValidateReplicationFactor(rf store.ReplicationFactor) error {
	if rf.All {
		return nil
	}
	if rf.Count < 1 {
		return fmt.Errorf("replication_factor must be >= 1 or \"all\" (got %d)", rf.Count)
	}
	return nil
}
