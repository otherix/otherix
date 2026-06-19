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

	lastCreateReq atomic.Value
	lastIdemKey   atomic.Value

	postCreateID  uuid.UUID
	postCreateErr error

	deleteID  uuid.UUID
	deleteErr error

	pollResult agentclient.TaskTerminal
	pollErr    error
}

func (f *fakeVMClient) PostVMCreate(
	_ context.Context, _ string, idemKey string, req agentclient.VMCreateRequest,
) (uuid.UUID, error) {
	f.postCreateCalls.Add(1)
	f.lastCreateReq.Store(req)
	f.lastIdemKey.Store(idemKey)
	return f.postCreateID, f.postCreateErr
}

func (f *fakeVMClient) DeleteVM(
	_ context.Context, _ string, _ string, idemKey string,
) (uuid.UUID, error) {
	f.deleteCalls.Add(1)
	f.lastIdemKey.Store(idemKey)
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
		Disk:        store.VMDisk{ID: uuid.New(), VmID: uuid.New()},
		ImageURL:    "https://example.test/img.qcow2",
		ImageSHA256: "abcdef",
		Format:      "qcow2",
		DiskGiB:     20,
		Pool:        store.StoragePool{ID: uuid.New(), Name: "default"},
		Node:        store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://node.test"},
		OnAgentTaskID: func(_ context.Context, id uuid.UUID) error {
			calls.Add(1)
			arg.Store(id)
			return nil
		},
	}, &calls, &arg
}

// TestAgentVMCreateExecutor_PollGoneIsAgentTaskGone pins the wrap depth between Execute and
// isAgentTaskGone: a PollTask 404 (the agent task vanished after a restart) must survive Execute's
// `poll task: %w` wrap so the M2 clear-on-404 path in runCreate fires. A non-404 poll error must
// NOT match. Without the `%w` in Execute this test fails, guarding a regression the bare-AgentError
// handler tests cannot catch.
func TestAgentVMCreateExecutor_PollGoneIsAgentTaskGone(t *testing.T) {
	t.Parallel()

	resumeID := uuid.New()
	args, _, _ := fixtureCreateArgs()
	args.AgentTaskID = &resumeID // resumption: skip POST, go straight to PollTask.

	gone := &fakeVMClient{pollErr: &agentclient.AgentError{Status: 404}}
	_, err := NewAgentVMCreateExecutor(gone).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute(poll 404) = nil error, want a wrapped agent error")
	}
	if !isAgentTaskGone(err) {
		t.Errorf("isAgentTaskGone(%v) = false, want true (the 404 must survive Execute's wrap)", err)
	}

	other := &fakeVMClient{pollErr: &agentclient.AgentError{Status: 500}}
	_, err = NewAgentVMCreateExecutor(other).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute(poll 500) = nil error, want a wrapped agent error")
	}
	if isAgentTaskGone(err) {
		t.Errorf("isAgentTaskGone(%v) = true, want false (only 404 is task-gone)", err)
	}
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

// TestAgentVMCreateExecutor_StableIdempotencyKey pins the agent idempotency key
// to the stable CP task id (args.TaskID) rather than a fresh random uuid per
// attempt. The key must equal args.TaskID.String() and be identical across two
// separate Execute attempts for the same task.
func TestAgentVMCreateExecutor_StableIdempotencyKey(t *testing.T) {
	t.Parallel()

	args, _, _ := fixtureCreateArgs()

	fc1 := &fakeVMClient{postCreateID: uuid.New(), pollResult: agentclient.TaskTerminal{Status: "success"}}
	exec := NewAgentVMCreateExecutor(fc1)
	if _, err := exec.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute attempt 1: %v", err)
	}
	key1, _ := fc1.lastIdemKey.Load().(string)
	if key1 != args.TaskID.String() {
		t.Errorf("idempotency key = %q, want stable task id %q", key1, args.TaskID.String())
	}

	fc2 := &fakeVMClient{postCreateID: uuid.New(), pollResult: agentclient.TaskTerminal{Status: "success"}}
	exec = NewAgentVMCreateExecutor(fc2)
	if _, err := exec.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute attempt 2: %v", err)
	}
	key2, _ := fc2.lastIdemKey.Load().(string)
	if key2 != key1 {
		t.Errorf("idempotency key not stable across attempts: %q vs %q", key1, key2)
	}
}

// TestAgentVMDeleteExecutor_StableIdempotencyKey: the delete executor likewise
// uses the stable CP task id as the agent idempotency key.
func TestAgentVMDeleteExecutor_StableIdempotencyKey(t *testing.T) {
	t.Parallel()

	taskID := uuid.New()
	fc := &fakeVMClient{
		deleteID:   uuid.New(),
		pollResult: agentclient.TaskTerminal{Status: "success"},
	}
	args := DeleteArgs{
		TaskID:        taskID,
		VMID:          uuid.New(),
		VMName:        "demo",
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}

	exec := NewAgentVMDeleteExecutor(fc)
	if _, err := exec.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	key, _ := fc.lastIdemKey.Load().(string)
	if key != taskID.String() {
		t.Errorf("delete idempotency key = %q, want stable task id %q", key, taskID.String())
	}
}

// TestAgentVMCreateExecutor_ForwardsImageSource pins the CP->agent vm-create
// seam: the executor builds the agent VMCreateRequest from the CreateArgs image
// fields (image_url + expected_sha256 + format + disk_gib), matching the agent's
// wire decode.
func TestAgentVMCreateExecutor_ForwardsImageSource(t *testing.T) {
	t.Parallel()

	fc := &fakeVMClient{
		postCreateID: uuid.New(),
		pollResult:   agentclient.TaskTerminal{Status: "success"},
	}
	args, _, _ := fixtureCreateArgs()

	exec := NewAgentVMCreateExecutor(fc)
	if _, err := exec.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req, _ := fc.lastCreateReq.Load().(agentclient.VMCreateRequest)
	if req.ImageURL != args.ImageURL {
		t.Errorf("req.ImageURL = %q, want %q", req.ImageURL, args.ImageURL)
	}
	if req.ExpectedSHA256 != args.ImageSHA256 {
		t.Errorf("req.ExpectedSHA256 = %q, want %q", req.ExpectedSHA256, args.ImageSHA256)
	}
	if req.Format != args.Format {
		t.Errorf("req.Format = %q, want %q", req.Format, args.Format)
	}
	if req.DiskGiB != args.DiskGiB {
		t.Errorf("req.DiskGiB = %d, want %d", req.DiskGiB, args.DiskGiB)
	}
}

// TestAgentVMCreateExecutor_SurfacesAgentResult pins the other half of the seam:
// on agent success the executor decodes the terminal task result
// (agentclient.VMCreateResult) and surfaces the resolved sha + disk sizes in the
// CreateResult.
func TestAgentVMCreateExecutor_SurfacesAgentResult(t *testing.T) {
	t.Parallel()

	fc := &fakeVMClient{
		postCreateID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{
				"image_sha256":       "deadbeef",
				"disk_size_bytes":    float64(123),
				"virtual_size_bytes": float64(456),
			},
		},
	}
	args, _, _ := fixtureCreateArgs()

	exec := NewAgentVMCreateExecutor(fc)
	res, err := exec.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.VMID != args.VM.ID.String() {
		t.Errorf("res.VMID = %q, want %q", res.VMID, args.VM.ID.String())
	}
	if res.ImageSHA256 != "deadbeef" {
		t.Errorf("res.ImageSHA256 = %q, want %q", res.ImageSHA256, "deadbeef")
	}
	if res.DiskSizeBytes != 123 {
		t.Errorf("res.DiskSizeBytes = %d, want 123", res.DiskSizeBytes)
	}
	if res.VirtualSizeBytes != 456 {
		t.Errorf("res.VirtualSizeBytes = %d, want 456", res.VirtualSizeBytes)
	}
}

func TestAgentVMCreateExecutor_ForwardsNics(t *testing.T) {
	t.Parallel()

	fc := &fakeVMClient{
		postCreateID: uuid.New(),
		pollResult:   agentclient.TaskTerminal{Status: "success"},
	}
	args, _, _ := fixtureCreateArgs()
	nic := agentclient.VMCreateNIC{
		ID: uuid.New(), Bridge: "br0", MAC: "52:54:00:ab:cd:ef", Model: "virtio", MTU: 1500, DeviceOrder: 0,
	}
	args.NICs = []agentclient.VMCreateNIC{nic}

	exec := NewAgentVMCreateExecutor(fc)
	if _, err := exec.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req, _ := fc.lastCreateReq.Load().(agentclient.VMCreateRequest)
	if len(req.Nics) != 1 {
		t.Fatalf("req.Nics = %d, want 1", len(req.Nics))
	}
	if req.Nics[0] != nic {
		t.Errorf("req.Nics[0] = %+v, want %+v", req.Nics[0], nic)
	}
}

// TestAgentVMCreateExecutor_ForwardsNetworkConfig pins the cloud-init
// network-config half of the CP->agent seam: a vm.NetworkConfig set on the
// CreateArgs.VM reaches the agent VMCreateRequest.NetworkConfig field through
// the real Execute/postOrResumeCreate path, while a disabled VM forwards "".
func TestAgentVMCreateExecutor_ForwardsNetworkConfig(t *testing.T) {
	t.Parallel()

	nc := "network:\n  version: 2\n"

	t.Run("set forwards verbatim", func(t *testing.T) {
		t.Parallel()
		fc := &fakeVMClient{
			postCreateID: uuid.New(),
			pollResult:   agentclient.TaskTerminal{Status: "success"},
		}
		args, _, _ := fixtureCreateArgs()
		args.VM.NetworkConfig = &nc

		exec := NewAgentVMCreateExecutor(fc)
		if _, err := exec.Execute(context.Background(), args); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		req, _ := fc.lastCreateReq.Load().(agentclient.VMCreateRequest)
		if req.NetworkConfig != nc {
			t.Errorf("req.NetworkConfig = %q, want %q", req.NetworkConfig, nc)
		}
	})

	t.Run("disabled wins forwards empty", func(t *testing.T) {
		t.Parallel()
		fc := &fakeVMClient{
			postCreateID: uuid.New(),
			pollResult:   agentclient.TaskTerminal{Status: "success"},
		}
		args, _, _ := fixtureCreateArgs()
		args.VM.NetworkConfig = &nc
		args.VM.CloudInitDisabled = true

		exec := NewAgentVMCreateExecutor(fc)
		if _, err := exec.Execute(context.Background(), args); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		req, _ := fc.lastCreateReq.Load().(agentclient.VMCreateRequest)
		if req.NetworkConfig != "" {
			t.Errorf("req.NetworkConfig = %q, want %q (disabled wins)", req.NetworkConfig, "")
		}
	})
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
