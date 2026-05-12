// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package health_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/migrationtest"
)

// sharedHarness is a process-global testcontainers Postgres set up by
// TestMain. Tests that don't manipulate the container's lifecycle reuse
// it; tests that intentionally stop the database (TestReady_DatabaseDown)
// allocate their own short-lived harness via migrationtest.MustStart.
var sharedHarness *migrationtest.Harness

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := migrationtest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrationtest.Start: %v\n", err)
		os.Exit(1)
	}
	sharedHarness = h

	code := m.Run()

	stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	h.Stop(stopCtx)

	os.Exit(code)
}
