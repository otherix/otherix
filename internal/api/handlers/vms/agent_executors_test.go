// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// fakeVMClient records every PostVMCreate / DeleteVM / PollTask call
// so tests can assert resumption and terminal mapping. Mirrors the
// fakeImportClient pattern from storagepools — same shape, narrower
// surface (no body capture for delete since DeleteVM has no body).
type fakeVMClient struct {
	postCreateCalls atomic.Int32
	deleteCalls     atomic.Int32
	pollCalls       atomic.Int32
	lastPollID      atomic.Value

	postCreateID  uuid.UUID
	postCreateErr error

	deleteID  uuid.UUID
	deleteErr error

	pollResult agentclient.TaskTerminal
	pollErr    error
}

func (f *fakeVMClient) PostVMCreate(
	_ context.Context, _ string, _ string, _ agentclient.VMCreateRequest,
) (uuid.UUID, error) {
	f.postCreateCalls.Add(1)
	return f.postCreateID, f.postCreateErr
}

func (f *fakeVMClient) DeleteVM(
	_ context.Context, _ string, _ string, _ string,
) (uuid.UUID, error) {
	f.deleteCalls.Add(1)
	return f.deleteID, f.deleteErr
}

func (f *fakeVMClient) PollTask(
	_ context.Context, _ string, agentTaskID uuid.UUID,
) (agentclient.TaskTerminal, error) {
	f.pollCalls.Add(1)
	f.lastPollID.Store(agentTaskID)
	return f.pollResult, f.pollErr
}

func fixtureCreateArgs() (CreateArgs, *atomic.Int32, *atomic.Value) {
	var calls atomic.Int32
	var arg atomic.Value
	return CreateArgs{
		TaskID: uuid.New(),
		VM: store.VM{
			ID:        uuid.New(),
			Name:      "demo",
			CpuCores:  2,
			MemoryMib: 2048,
		},
		Disk: store.VMDisk{ID: uuid.New(), VmID: uuid.New()},
		Template: store.Template{
			ID:                  uuid.New(),
			ImageChecksumSha256: []byte{0xab, 0xcd, 0xef},
		},
		Pool: store.StoragePool{ID: uuid.New()},
		Node: store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://node.test"},
		OnAgentTaskID: func(_ context.Context, id uuid.UUID) error {
			calls.Add(1)
			arg.Store(id)
			return nil
		},
	}, &calls, &arg
}

func TestAgentVMCreateExecutor_FirstRunPostsAndPersists(t *testing.T) {
	t.Parallel()

	wantAgentID := uuid.New()
	fc := &fakeVMClient{
		postCreateID: wantAgentID,
		pollResult:   agentclient.TaskTerminal{Status: "success"},
	}
	args, callbackCalls, callbackArg := fixtureCreateArgs()

	exec := NewAgentVMCreateExecutor(fc)
	res, err := exec.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.VMID != args.VM.ID.String() {
		t.Errorf("result.VMID = %q, want %q", res.VMID, args.VM.ID.String())
	}
	if fc.postCreateCalls.Load() != 1 {
		t.Errorf("post calls = %d, want 1", fc.postCreateCalls.Load())
	}
	if fc.pollCalls.Load() != 1 {
		t.Errorf("poll calls = %d, want 1", fc.pollCalls.Load())
	}
	if callbackCalls.Load() != 1 {
		t.Errorf("callback calls = %d, want 1", callbackCalls.Load())
	}
	if got, _ := callbackArg.Load().(uuid.UUID); got != wantAgentID {
		t.Errorf("callback arg = %s, want %s", got, wantAgentID)
	}
	if got, _ := fc.lastPollID.Load().(uuid.UUID); got != wantAgentID {
		t.Errorf("poll task id = %s, want %s", got, wantAgentID)
	}
}

func TestAgentVMCreateExecutor_ResumeSkipsPost(t *testing.T) {
	t.Parallel()

	resumedID := uuid.New()
	fc := &fakeVMClient{
		pollResult: agentclient.TaskTerminal{Status: "success"},
	}
	args, callbackCalls, _ := fixtureCreateArgs()
	args.AgentTaskID = &resumedID

	exec := NewAgentVMCreateExecutor(fc)
	if _, err := exec.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fc.postCreateCalls.Load() != 0 {
		t.Errorf("post calls = %d, want 0 (resumption must skip POST)", fc.postCreateCalls.Load())
	}
	if got, _ := fc.lastPollID.Load().(uuid.UUID); got != resumedID {
		t.Errorf("poll task id = %s, want %s (resumed)", got, resumedID)
	}
	if callbackCalls.Load() != 0 {
		t.Errorf("callback calls = %d, want 0 (resumption must not re-persist)", callbackCalls.Load())
	}
}

func TestAgentVMCreateExecutor_TerminalFailedSurfacesAgentError(t *testing.T) {
	t.Parallel()

	fc := &fakeVMClient{
		postCreateID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "failed",
			Error:  &agentclient.AgentError{Status: 500, Code: "qemu_spawn_failed"},
		},
	}
	args, _, _ := fixtureCreateArgs()
	exec := NewAgentVMCreateExecutor(fc)

	_, err := exec.Execute(context.Background(), args)
	if err == nil {
		t.Fatalf("Execute err = nil, want AgentError")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Code != "qemu_spawn_failed" {
		t.Errorf("err = %v, want AgentError code=qemu_spawn_failed", err)
	}
}

func TestAgentVMDeleteExecutor_HappyPath(t *testing.T) {
	t.Parallel()

	wantAgentID := uuid.New()
	fc := &fakeVMClient{
		deleteID:   wantAgentID,
		pollResult: agentclient.TaskTerminal{Status: "success"},
	}
	vmID := uuid.New()
	var callbackCalls atomic.Int32
	args := DeleteArgs{
		TaskID: uuid.New(),
		VMID:   vmID,
		Node:   store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://node.test"},
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error {
			callbackCalls.Add(1)
			return nil
		},
	}

	exec := NewAgentVMDeleteExecutor(fc)
	res, err := exec.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.VMID != vmID.String() {
		t.Errorf("result.VMID = %q, want %q", res.VMID, vmID.String())
	}
	if callbackCalls.Load() != 1 {
		t.Errorf("callback calls = %d, want 1", callbackCalls.Load())
	}
}

func TestAgentVMDeleteExecutor_404IsIdempotentSuccess(t *testing.T) {
	t.Parallel()

	fc := &fakeVMClient{
		deleteErr: &agentclient.AgentError{Status: http.StatusNotFound, Code: "not_found"},
	}
	vmID := uuid.New()
	args := DeleteArgs{
		VMID:          vmID,
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}

	exec := NewAgentVMDeleteExecutor(fc)
	res, err := exec.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v (404 must collapse to success)", err)
	}
	if res.VMID != vmID.String() {
		t.Errorf("result.VMID = %q, want %q", res.VMID, vmID.String())
	}
	if fc.pollCalls.Load() != 0 {
		t.Errorf("poll calls = %d, want 0 (404 short-circuit must skip polling)", fc.pollCalls.Load())
	}
}

func TestAgentVMDeleteExecutor_PollTerminalFailedNon404(t *testing.T) {
	t.Parallel()

	fc := &fakeVMClient{
		deleteID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "failed",
			Error:  &agentclient.AgentError{Status: 500, Code: "internal"},
		},
	}
	vmID := uuid.New()
	args := DeleteArgs{
		VMID:          vmID,
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}

	exec := NewAgentVMDeleteExecutor(fc)
	_, err := exec.Execute(context.Background(), args)
	if err == nil {
		t.Fatalf("Execute err = nil, want non-404 failure surfacing as error")
	}
}
