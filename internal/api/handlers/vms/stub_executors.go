// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import "context"

// StubVMCreateExecutor is the test-side CreateExecutor. Returns a
// canned (CreateResult, error) pair - exported so that the
// integration-test scaffold (and the smoke harness) can drive
// VMCreateWorker.Work directly without spinning up an agent.
//
// Mirrors the storagepools.StubImportExecutor surface so the test
// patterns stay symmetric across vertical slices.
type StubVMCreateExecutor struct {
	Result CreateResult
	Err    error
}

// Execute implements CreateExecutor.
func (s *StubVMCreateExecutor) Execute(_ context.Context, _ CreateArgs) (CreateResult, error) {
	return s.Result, s.Err
}

// StubVMDeleteExecutor is the test-side DeleteExecutor.
type StubVMDeleteExecutor struct {
	Result DeleteResult
	Err    error
}

// Execute implements DeleteExecutor.
func (s *StubVMDeleteExecutor) Execute(_ context.Context, _ DeleteArgs) (DeleteResult, error) {
	return s.Result, s.Err
}
