// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// ensureClientSpy records PostImageImport / PollTask calls so tests can
// assert the no-op path never reaches the agent and the import path
// forwards endpoint + pool name verbatim.
type ensureClientSpy struct {
	postCalls    atomic.Int32
	pollCalls    atomic.Int32
	lastPostBody atomic.Value
	lastEndpoint atomic.Value
	lastPoolName atomic.Value

	postID  uuid.UUID
	postErr error

	pollResult agentclient.TaskTerminal
	pollErr    error
}

func (s *ensureClientSpy) PostImageImport(
	_ context.Context, endpoint string, poolName string, _ string, req agentapi.ImageImportRequest,
) (uuid.UUID, error) {
	s.postCalls.Add(1)
	s.lastEndpoint.Store(endpoint)
	s.lastPoolName.Store(poolName)
	s.lastPostBody.Store(req)
	return s.postID, s.postErr
}

func (s *ensureClientSpy) PollTask(
	_ context.Context, _ string, _ uuid.UUID,
) (agentclient.TaskTerminal, error) {
	s.pollCalls.Add(1)
	return s.pollResult, s.pollErr
}

// ensureStoreSpy fakes the imageEnsureStore seam. existsResult drives the
// fast-path branch; projectCalls / lastProjectImg record the projection.
type ensureStoreSpy struct {
	existsResult bool
	existsErr    error

	projectCalls atomic.Int32
	lastImg      atomic.Value
	lastChecksum atomic.Value
	projectErr   error
}

func (s *ensureStoreSpy) StorageImageExists(
	_ context.Context, _, _ uuid.UUID,
) (bool, error) {
	return s.existsResult, s.existsErr
}

func (s *ensureStoreSpy) EnsureStorageImageProjected(
	_ context.Context, img store.CreateStorageImageParams, tmplChecksum store.UpdateTemplateImageChecksumIfNullParams,
) error {
	s.projectCalls.Add(1)
	s.lastImg.Store(img)
	s.lastChecksum.Store(tmplChecksum)
	return s.projectErr
}

func ensureFixtureTemplate() store.Template {
	return store.Template{
		ID:                  uuid.New(),
		ImageUrl:            "https://example.test/img.qcow2",
		ImageChecksumSha256: []byte{0xde, 0xad, 0xbe, 0xef},
		ImageFormat:         store.ImageFormatQcow2,
	}
}

func TestImageEnsurer_NoOpWhenPresent(t *testing.T) {
	t.Parallel()

	client := &ensureClientSpy{}
	st := &ensureStoreSpy{existsResult: true}
	e := NewImageEnsurer(client, st)

	err := e.EnsureImageOnPool(
		context.Background(),
		ensureFixtureTemplate(),
		store.StoragePool{ID: uuid.New(), Name: "pool-a"},
		store.Node{AdvertisedEndpoint: "https://node.test"},
	)
	if err != nil {
		t.Fatalf("EnsureImageOnPool: %v", err)
	}
	if client.postCalls.Load() != 0 {
		t.Errorf("post calls = %d, want 0 (present image must not contact agent)", client.postCalls.Load())
	}
	if client.pollCalls.Load() != 0 {
		t.Errorf("poll calls = %d, want 0", client.pollCalls.Load())
	}
	if st.projectCalls.Load() != 0 {
		t.Errorf("project calls = %d, want 0", st.projectCalls.Load())
	}
}

func TestImageEnsurer_DrivesImportWhenAbsent(t *testing.T) {
	t.Parallel()

	tpl := ensureFixtureTemplate()
	pool := store.StoragePool{ID: uuid.New(), Name: "pool-a"}
	client := &ensureClientSpy{
		postID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{
				"checksum_sha256": "abcd", "size_bytes": float64(2048), "format": "qcow2",
			},
		},
	}
	st := &ensureStoreSpy{existsResult: false}
	e := NewImageEnsurer(client, st)

	err := e.EnsureImageOnPool(
		context.Background(),
		tpl,
		pool,
		store.Node{AdvertisedEndpoint: "https://node.test"},
	)
	if err != nil {
		t.Fatalf("EnsureImageOnPool: %v", err)
	}
	if client.postCalls.Load() != 1 {
		t.Errorf("post calls = %d, want 1", client.postCalls.Load())
	}
	if client.pollCalls.Load() != 1 {
		t.Errorf("poll calls = %d, want 1", client.pollCalls.Load())
	}
	if got, _ := client.lastEndpoint.Load().(string); got != "https://node.test" {
		t.Errorf("endpoint = %q, want https://node.test", got)
	}
	if got, _ := client.lastPoolName.Load().(string); got != "pool-a" {
		t.Errorf("pool name = %q, want pool-a", got)
	}
	if st.projectCalls.Load() != 1 {
		t.Fatalf("project calls = %d, want 1", st.projectCalls.Load())
	}
	img, _ := st.lastImg.Load().(store.CreateStorageImageParams)
	if img.TemplateID != tpl.ID || img.PoolID != pool.ID {
		t.Errorf("project img ids = (%s,%s), want (%s,%s)", img.TemplateID, img.PoolID, tpl.ID, pool.ID)
	}
	if img.ChecksumSha256 != "abcd" || img.SizeBytes != 2048 || img.Format != "qcow2" {
		t.Errorf("project img = {%s,%d,%s}, want {abcd,2048,qcow2}", img.ChecksumSha256, img.SizeBytes, img.Format)
	}
	chk, _ := st.lastChecksum.Load().(store.UpdateTemplateImageChecksumIfNullParams)
	wantBytes, _ := hex.DecodeString("abcd")
	if chk.ID != tpl.ID || !bytes.Equal(chk.ImageChecksumSha256, wantBytes) {
		t.Errorf("checksum params = {%s,%x}, want {%s,%x}", chk.ID, chk.ImageChecksumSha256, tpl.ID, wantBytes)
	}
}

var errAgentImportFailed = &agentclient.AgentError{Status: 0, Code: "checksum_mismatch", Message: "bytes differ"}

func TestImageEnsurer_SurfacesAgentFailureCause(t *testing.T) {
	t.Parallel()

	client := &ensureClientSpy{
		postID:     uuid.New(),
		pollResult: agentclient.TaskTerminal{Status: "failed", Error: errAgentImportFailed},
	}
	st := &ensureStoreSpy{existsResult: false}
	e := NewImageEnsurer(client, st)

	err := e.EnsureImageOnPool(
		context.Background(),
		ensureFixtureTemplate(),
		store.StoragePool{ID: uuid.New(), Name: "pool-a"},
		store.Node{AdvertisedEndpoint: "https://node.test"},
	)
	if err == nil {
		t.Fatal("EnsureImageOnPool returned nil on terminal-failed")
	}
	if !errors.Is(err, errAgentImportFailed) {
		t.Errorf("err = %v, want errors.Is errAgentImportFailed", err)
	}
	if st.projectCalls.Load() != 0 {
		t.Errorf("project calls = %d, want 0 on agent failure", st.projectCalls.Load())
	}
}

func TestImageEnsurer_PostTransportErrorWrapped(t *testing.T) {
	t.Parallel()

	client := &ensureClientSpy{postErr: &agentclient.AgentError{Status: 502, Code: "internal", Message: "upstream"}}
	st := &ensureStoreSpy{existsResult: false}
	e := NewImageEnsurer(client, st)

	err := e.EnsureImageOnPool(
		context.Background(),
		ensureFixtureTemplate(),
		store.StoragePool{ID: uuid.New(), Name: "pool-a"},
		store.Node{AdvertisedEndpoint: "https://node.test"},
	)
	if err == nil {
		t.Fatal("EnsureImageOnPool returned nil on POST transport error")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Errorf("err = %v, want unwrap to *AgentError", err)
	}
	if client.pollCalls.Load() != 0 {
		t.Errorf("poll calls = %d, want 0 after POST failure", client.pollCalls.Load())
	}
}

func TestImageEnsurer_PollTransportErrorWrapped(t *testing.T) {
	t.Parallel()

	client := &ensureClientSpy{
		postID:  uuid.New(),
		pollErr: &agentclient.TimeoutError{AgentTaskID: "x", Budget: "5s"},
	}
	st := &ensureStoreSpy{existsResult: false}
	e := NewImageEnsurer(client, st)

	err := e.EnsureImageOnPool(
		context.Background(),
		ensureFixtureTemplate(),
		store.StoragePool{ID: uuid.New(), Name: "pool-a"},
		store.Node{AdvertisedEndpoint: "https://node.test"},
	)
	if err == nil {
		t.Fatal("EnsureImageOnPool returned nil on poll error")
	}
	var te *agentclient.TimeoutError
	if !errors.As(err, &te) {
		t.Errorf("err = %v, want unwrap to *TimeoutError", err)
	}
}

func TestImageEnsurer_PostsExpectedChecksumWhenPresent(t *testing.T) {
	t.Parallel()

	client := &ensureClientSpy{
		postID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{"checksum_sha256": "deadbeef", "size_bytes": float64(1), "format": "qcow2"},
		},
	}
	st := &ensureStoreSpy{existsResult: false}
	e := NewImageEnsurer(client, st)

	tpl := ensureFixtureTemplate() // ImageChecksumSha256: [de ad be ef]
	if err := e.EnsureImageOnPool(
		context.Background(), tpl,
		store.StoragePool{ID: uuid.New(), Name: "pool-a"},
		store.Node{AdvertisedEndpoint: "https://node.test"},
	); err != nil {
		t.Fatalf("EnsureImageOnPool: %v", err)
	}
	body, _ := client.lastPostBody.Load().(agentapi.ImageImportRequest)
	if body.ExpectedChecksumSha256 == nil {
		t.Fatal("ExpectedChecksumSha256 = nil, want pointer to deadbeef")
	}
	if *body.ExpectedChecksumSha256 != "deadbeef" {
		t.Errorf("ExpectedChecksumSha256 = %q, want deadbeef", *body.ExpectedChecksumSha256)
	}
	if body.SourceURL == nil || *body.SourceURL != tpl.ImageUrl {
		t.Errorf("SourceURL = %v, want %q", body.SourceURL, tpl.ImageUrl)
	}
}

func TestImageEnsurer_OmitsExpectedChecksumWhenAbsent(t *testing.T) {
	t.Parallel()

	client := &ensureClientSpy{
		postID: uuid.New(),
		pollResult: agentclient.TaskTerminal{
			Status: "success",
			Result: map[string]any{"checksum_sha256": "abcdef", "size_bytes": float64(1), "format": "qcow2"},
		},
	}
	st := &ensureStoreSpy{existsResult: false}
	e := NewImageEnsurer(client, st)

	tpl := ensureFixtureTemplate()
	tpl.ImageChecksumSha256 = nil // compute mode
	if err := e.EnsureImageOnPool(
		context.Background(), tpl,
		store.StoragePool{ID: uuid.New(), Name: "pool-a"},
		store.Node{AdvertisedEndpoint: "https://node.test"},
	); err != nil {
		t.Fatalf("EnsureImageOnPool: %v", err)
	}
	body, _ := client.lastPostBody.Load().(agentapi.ImageImportRequest)
	if body.ExpectedChecksumSha256 != nil {
		t.Errorf("ExpectedChecksumSha256 = %q, want nil (compute mode)", *body.ExpectedChecksumSha256)
	}
}
