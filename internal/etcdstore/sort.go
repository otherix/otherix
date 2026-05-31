// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// sortByCreatedAtID sorts items in place by (created_at, id) ascending - the
// canonical cursor-pagination order shared by every listable resource. The key
// function projects each item onto its (created_at, id) tuple.
func sortByCreatedAtID[T any](items []T, key func(T) (time.Time, uuid.UUID)) {
	sort.Slice(items, func(i, j int) bool {
		ti, ii := key(items[i])
		tj, ij := key(items[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return ii.String() < ij.String()
	})
}
