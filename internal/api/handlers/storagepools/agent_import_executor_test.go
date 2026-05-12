// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// fakeImportClient records every PostImageImport / PollTask call so
// tests can assert resumption (zero PostImageImport on retry) and
// terminal mapping. Each method returns the canned (postResult,
// pollResult) the test injects.
type fakeImportClient struct {
	postCalls       atomic.Int32
	pollCalls       atomic.Int32
	lastIdempotency atomic.Value
	lastPostBody    atomic.Value
	lastPollID      atomic.Value

	postID  uuid.UUID
	postErr error

	pollResult agentclient.TaskTerminal
	pollErr    error
}

func (f *fakeImportClient) PostImageImport(
	_ context.Context, _ string, _ string, idempotencyKey string, req agentapi.ImageImportRequest,
) (uuid.UUID, error) {
	f.postCalls.Add(1)
	f.lastIdempotency.Store(idempotencyKey)
	f.lastPostBody.Store(req)
	return f.postID, f.postErr
}

func (f *fakeImportClient) PollTask(
	_ context.Context, _ string, agentTaskID uuid.UUID,
) (agentclient.TaskTerminal, error) {
	f.pollCalls.Add(1)
	f.lastPollID.Store(agentTaskID)
	return f.pollResult, f.pollErr
}

// fixtureTemplate returns a minimal Template with the three fields the
// executor reads on the first-run POST.
func fixtureTemplate() store.Template {
	return store.Template{
		ID:                  uuid.New(),
		ImageUrl:            "https://example.test/img.qcow2",
		ImageChecksumSha256: []byte{0xde, 0xad, 0xbe, 0xef},
		ImageFormat:         store.ImageFormatQcow2,
	}
}

// TestAgentImportExecutor_VerifyMode_PostsExpectedChecksum locks in the
// verify-mode wire body shape: when the template carries
// non-nil ImageChecksumSha256, the agent request's
// expected_checksum_sha256 is the lowercase-hex encoding of those bytes.
func TestAgentImportExecutor_VerifyMode_PostsExpectedChecksum(t *testing.T) {
	t.Parallel()
	fc := &fakeImportClient{
		postID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{
				"checksum_sha256": "deadbeef", "size_bytes": float64(1), "format": "qcow2",
			},
		},
	}
	args := ImportArgs{
		TaskID:        uuid.New(),
		Pool:          store.StoragePool{ID: uuid.New()},
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:      fixtureTemplate(), // ImageChecksumSha256: [0xde,0xad,0xbe,0xef]
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}
	if _, err := NewAgentImportExecutor(fc).Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body, ok := fc.lastPostBody.Load().(agentapi.ImageImportRequest)
	if !ok {
		t.Fatalf("post body not captured")
	}
	if body.ExpectedChecksumSha256 == nil {
		t.Fatal("ExpectedChecksumSha256 = nil, want pointer к 'deadbeef'")
	}
	if *body.ExpectedChecksumSha256 != "deadbeef" {
		t.Errorf("ExpectedChecksumSha256 = %q, want 'deadbeef'", *body.ExpectedChecksumSha256)
	}
}

// TestAgentImportExecutor_ComputeMode_OmitsExpectedChecksum confirms
// the compute-mode shape: а template с nil/empty ImageChecksumSha256
// triggers an agent request without expected_checksum_sha256 set.
// The agentapi field is *string + omitempty, so leaving it nil
// produces а body the agent's relaxed validator interprets как
// "no upstream hash; compute during download".
func TestAgentImportExecutor_ComputeMode_OmitsExpectedChecksum(t *testing.T) {
	t.Parallel()
	fc := &fakeImportClient{
		postID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{
				"checksum_sha256": "abcdef", "size_bytes": float64(1), "format": "qcow2",
			},
		},
	}
	tpl := fixtureTemplate()
	tpl.ImageChecksumSha256 = nil // compute mode
	args := ImportArgs{
		TaskID:        uuid.New(),
		Pool:          store.StoragePool{ID: uuid.New()},
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:      tpl,
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}
	if _, err := NewAgentImportExecutor(fc).Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body, ok := fc.lastPostBody.Load().(agentapi.ImageImportRequest)
	if !ok {
		t.Fatalf("post body not captured")
	}
	if body.ExpectedChecksumSha256 != nil {
		t.Errorf("ExpectedChecksumSha256 = %q, want nil (compute mode)",
			*body.ExpectedChecksumSha256)
	}
}

func TestAgentImportExecutor_FirstRunPostsAndPersists(t *testing.T) {
	t.Parallel()

	wantAgentID := uuid.New()
	fc := &fakeImportClient{
		postID: wantAgentID,
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{
				"checksum_sha256": "abcd",
				"size_bytes":      float64(2048),
				"format":          "qcow2",
			},
		},
	}

	var (
		callbackInvocations atomic.Int32
		callbackArg         atomic.Value
	)
	args := ImportArgs{
		TaskID:      uuid.New(),
		AgentTaskID: nil, // first run
		Pool:        store.StoragePool{ID: uuid.New(), NodeID: uuid.New()},
		Node:        store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://node.test"},
		Template:    fixtureTemplate(),
		OnAgentTaskID: func(_ context.Context, id uuid.UUID) error {
			callbackInvocations.Add(1)
			callbackArg.Store(id)
			return nil
		},
	}

	exec := NewAgentImportExecutor(fc)
	got, err := exec.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ChecksumSHA256 != "abcd" || got.SizeBytes != 2048 || got.Format != "qcow2" {
		t.Errorf("result = %+v, want {abcd, 2048, qcow2}", got)
	}
	if fc.postCalls.Load() != 1 {
		t.Errorf("post calls = %d, want 1", fc.postCalls.Load())
	}
	if fc.pollCalls.Load() != 1 {
		t.Errorf("poll calls = %d, want 1", fc.pollCalls.Load())
	}
	if callbackInvocations.Load() != 1 {
		t.Errorf("callback invocations = %d, want 1", callbackInvocations.Load())
	}
	if got, _ := callbackArg.Load().(uuid.UUID); got != wantAgentID {
		t.Errorf("callback arg = %s, want %s", got, wantAgentID)
	}
	if got, _ := fc.lastPollID.Load().(uuid.UUID); got != wantAgentID {
		t.Errorf("poll task id = %s, want %s (callback arg = same)", got, wantAgentID)
	}
}

func TestAgentImportExecutor_ResumptionSkipsPost(t *testing.T) {
	t.Parallel()

	existingAgentID := uuid.New()
	fc := &fakeImportClient{
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{
				"checksum_sha256": "deadbeef", "size_bytes": float64(1024), "format": "qcow2",
			},
		},
	}

	var callbackInvocations atomic.Int32
	args := ImportArgs{
		TaskID:      uuid.New(),
		AgentTaskID: &existingAgentID, // resume
		Pool:        store.StoragePool{ID: uuid.New()},
		Node:        store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:    fixtureTemplate(),
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error {
			callbackInvocations.Add(1)
			return nil
		},
	}

	if _, err := NewAgentImportExecutor(fc).Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fc.postCalls.Load() != 0 {
		t.Errorf("post calls = %d, want 0 (resumption must skip POST)", fc.postCalls.Load())
	}
	if callbackInvocations.Load() != 0 {
		t.Errorf("callback invocations = %d, want 0 (resumption must not re-persist)", callbackInvocations.Load())
	}
	if got, _ := fc.lastPollID.Load().(uuid.UUID); got != existingAgentID {
		t.Errorf("poll arg = %s, want existing %s", got, existingAgentID)
	}
}

func TestAgentImportExecutor_PostFailsBeforeCallback(t *testing.T) {
	t.Parallel()

	postErr := &agentclient.AgentError{Status: 502, Code: "internal", Message: "upstream"}
	fc := &fakeImportClient{postErr: postErr}

	var callbackInvocations atomic.Int32
	args := ImportArgs{
		TaskID:      uuid.New(),
		AgentTaskID: nil,
		Pool:        store.StoragePool{},
		Node:        store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:    fixtureTemplate(),
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error {
			callbackInvocations.Add(1)
			return nil
		},
	}

	_, err := NewAgentImportExecutor(fc).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute returned nil error on POST failure")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want unwrap to *AgentError", err, err)
	}
	if callbackInvocations.Load() != 0 {
		t.Errorf("callback invoked = %d, want 0 on POST failure", callbackInvocations.Load())
	}
	if fc.pollCalls.Load() != 0 {
		t.Errorf("poll calls = %d, want 0 on POST failure", fc.pollCalls.Load())
	}
}

func TestAgentImportExecutor_TerminalFailedReturnsAgentError(t *testing.T) {
	t.Parallel()

	envelope := &agentclient.AgentError{Status: 0, Code: "checksum_mismatch", Message: "bytes differ"}
	fc := &fakeImportClient{
		postID:     uuid.New(),
		pollResult: agentclient.TaskTerminal{Status: "failed", Error: envelope},
	}
	args := ImportArgs{
		AgentTaskID:   nil,
		Pool:          store.StoragePool{},
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:      fixtureTemplate(),
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}

	_, err := NewAgentImportExecutor(fc).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute returned nil on terminal-failed")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want *AgentError", err)
	}
	if ae.Code != "checksum_mismatch" {
		t.Errorf("Code = %q, want checksum_mismatch", ae.Code)
	}
}

func TestAgentImportExecutor_PollErrorPropagates(t *testing.T) {
	t.Parallel()

	fc := &fakeImportClient{
		postID:  uuid.New(),
		pollErr: &agentclient.TimeoutError{AgentTaskID: "x", Budget: "5s"},
	}
	args := ImportArgs{
		Pool:          store.StoragePool{},
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:      fixtureTemplate(),
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return nil },
	}

	_, err := NewAgentImportExecutor(fc).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute returned nil on poll error")
	}
	var te *agentclient.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *TimeoutError", err)
	}
}

func TestAgentImportExecutor_CallbackFailureAborts(t *testing.T) {
	t.Parallel()

	fc := &fakeImportClient{postID: uuid.New()}
	cbErr := errors.New("persist failed")
	args := ImportArgs{
		Pool:          store.StoragePool{},
		Node:          store.Node{AdvertisedEndpoint: "https://node.test"},
		Template:      fixtureTemplate(),
		OnAgentTaskID: func(_ context.Context, _ uuid.UUID) error { return cbErr },
	}

	_, err := NewAgentImportExecutor(fc).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute returned nil on callback failure")
	}
	if fc.pollCalls.Load() != 0 {
		t.Errorf("poll calls = %d, want 0 (callback failure must abort before poll)", fc.pollCalls.Load())
	}
}

func TestDecodeImportResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      map[string]any
		wantErr bool
		want    ImportResult
	}{
		{
			name: "happy",
			in: map[string]any{
				"checksum_sha256": "deadbeef",
				"size_bytes":      float64(8192),
				"format":          "qcow2",
			},
			want: ImportResult{ChecksumSHA256: "deadbeef", SizeBytes: 8192, Format: "qcow2"},
		},
		{name: "nil map", in: nil, wantErr: true},
		{
			name:    "missing checksum",
			in:      map[string]any{"size_bytes": float64(1), "format": "qcow2"},
			wantErr: true,
		},
		{
			name:    "missing size",
			in:      map[string]any{"checksum_sha256": "x", "format": "qcow2"},
			wantErr: true,
		},
		{
			name:    "missing format",
			in:      map[string]any{"checksum_sha256": "x", "size_bytes": float64(1)},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeImportResult(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decodeImportResult: want err, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeImportResult: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
