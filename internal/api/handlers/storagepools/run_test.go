// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// scanWorkerStoreStub is a minimal WorkerStore double for the runScan abort test.
// taskTerminal drives UpdateTaskRunning's alreadyTerminal return; projectedScan
// records whether the success projection was reached.
type scanWorkerStoreStub struct {
	taskTerminal  bool
	projectedScan bool
}

func (s *scanWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) (bool, error) {
	return s.taskTerminal, nil
}

func (s *scanWorkerStoreStub) UpdateTaskFinalized(context.Context, store.UpdateTaskFinalizedParams) error {
	return nil
}

func (s *scanWorkerStoreStub) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	return store.StoragePool{}, nil
}

func (s *scanWorkerStoreStub) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	return store.Node{}, nil
}

func (s *scanWorkerStoreStub) ProjectStoragePoolScan(context.Context, store.UpsertStoragePoolUsageParams, store.UpdatePoolDiskPressureParams, store.UpdateTaskFinalizedParams) error {
	s.projectedScan = true
	return nil
}

// spyScanExecutor records whether the agent scan was attempted.
type spyScanExecutor struct{ called bool }

func (e *spyScanExecutor) Execute(context.Context, ScanArgs) (ScanResult, error) {
	e.called = true
	return ScanResult{}, nil
}

// TestRunScanAbortsOnTerminalTask pins R2-M3 for the scan path: a redelivery whose
// task is already committed-terminal (UpdateTaskRunning reports alreadyTerminal=true)
// completes WITHOUT contacting the agent - the executor is never called and no
// projection runs. The dispatcher then CompleteJob-deletes the job.
func TestRunScanAbortsOnTerminalTask(t *testing.T) {
	st := &scanWorkerStoreStub{taskTerminal: true}
	exec := &spyScanExecutor{}

	err := runScan(context.Background(), st, exec, config.PressureConditionConfig{}, discardLogger(),
		StoragePoolScanArgs{TaskID: uuid.New(), PoolID: uuid.New()})
	if err != nil {
		t.Fatalf("runScan(terminal task) = %v, want nil", err)
	}
	if exec.called {
		t.Errorf("agent scan attempted on a committed-terminal task; must abort without contacting the agent")
	}
	if st.projectedScan {
		t.Errorf("ProjectStoragePoolScan called on a committed-terminal task; the abort path must not project")
	}
}
