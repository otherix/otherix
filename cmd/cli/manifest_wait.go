// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// poolReconcilePoll is the interval for polling pool reconciliation
// status under --wait. A package-level var (not a const) so tests can
// lower it; production code never reassigns it.
var poolReconcilePoll = 2 * time.Second

// waitForCreated blocks until every async resource produced by the
// create fan-out is ready: VM tasks reach a terminal status and pools
// reach reconciliation_status ready/failed. It augments each result's
// note and sets err on failure. Networks are synchronous (no wait).
// Results whose create already failed are skipped.
func waitForCreated(cmd *cobra.Command, c *cpclient.Client, results []docResult, timeout time.Duration) {
	ctx := cmd.Context()
	deadline := time.Now().Add(timeout)
	for i := range results {
		if results[i].err != nil {
			continue
		}
		switch results[i].kind {
		case manifest.KindVM:
			waitVMResult(ctx, c, &results[i], deadline)
		case manifest.KindStoragePool:
			waitPoolResult(ctx, c, &results[i], deadline)
		}
	}
}

// waitVMResult polls the result's task id to terminal status, bounded
// by the shared deadline so every VM and pool in the fan-out competes
// for the same overall --wait budget.
func waitVMResult(ctx context.Context, c *cpclient.Client, r *docResult, deadline time.Time) {
	id, err := uuid.Parse(r.taskID)
	if err != nil {
		r.err = fmt.Errorf("wait: malformed task id %q", r.taskID)
		return
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		r.err = errors.New("wait: timeout budget exhausted")
		return
	}
	task, err := c.WaitTask(ctx, id, cpclient.WaitOptions{Timeout: remaining})
	if err != nil {
		r.err = cpErr(err)
		return
	}
	if task.Status == "success" {
		r.note = "ready"
		return
	}
	env, _ := task.DecodeError()
	if env != nil {
		r.err = fmt.Errorf("%s: %s", env.Code, env.Message)
		return
	}
	r.err = fmt.Errorf("task terminated with status %q", task.Status)
}

// waitPoolResult polls GetPoolByID until reconciliation_status is ready
// or failed (or the deadline passes).
func waitPoolResult(ctx context.Context, c *cpclient.Client, r *docResult, deadline time.Time) {
	id, err := uuid.Parse(r.poolID)
	if err != nil {
		r.err = fmt.Errorf("wait: malformed pool id %q", r.poolID)
		return
	}
	for {
		p, _, err := c.GetPoolByID(ctx, id)
		if err != nil {
			r.err = cpErr(err)
			return
		}
		switch p.ReconciliationStatus {
		case "ready":
			r.note += " ready"
			return
		case "failed":
			r.err = errors.New("reconciliation failed")
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.err = errors.New("reconciliation did not reach ready within timeout")
			return
		}
		sleep := poolReconcilePoll
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			r.err = ctx.Err()
			return
		case <-time.After(sleep):
		}
	}
}
