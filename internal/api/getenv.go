// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import "os"

// getenv is the production binding of BootstrapAdminEnv. It is a thin
// wrapper around os.Getenv so the bootstrap path stays free of direct
// process-state reads in tests.
func getenv(key string) string {
	return os.Getenv(key)
}
