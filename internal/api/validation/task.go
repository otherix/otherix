// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"fmt"

	"github.com/otherix/otherix/internal/store"
)

// ValidateTaskStatus returns nil when s is a recognised
// store.TaskStatus value. The set is anchored to the SQL enum
// (`task_status` in migration 00008_tasks.sql) and mirrors the
// `status` enum in the OpenAPI Task schema. Used by the
// `tasks.list` ?status= filter; adding a new status means
// extending both the migration and this branch.
func ValidateTaskStatus(s string) error {
	switch store.TaskStatus(s) {
	case store.TaskStatusPending,
		store.TaskStatusRunning,
		store.TaskStatusSuccess,
		store.TaskStatusFailed,
		store.TaskStatusCancelled:
		return nil
	}
	return fmt.Errorf("invalid task status %q (must be one of: pending, running, success, failed, cancelled)", s)
}
