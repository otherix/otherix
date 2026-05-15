// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/agentmock"
)

// importHexSha returns a deterministic 64-char hex sha256 seeded with
// b repeated 32 times.
func importHexSha(b byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i := 0; i < 32; i++ {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

// startImportMock spins up a Mock with one registered pool and
// returns the mock + the pool name. Tests then drive
// StorageImagesImport / StorageImagesDelete through the mock's HTTP
// surface.
func startImportMock(t *testing.T) (*agentmock.Mock, string) {
	t.Helper()
	m := agentmock.Start(t, agentmock.Options{})
	poolName := "import-pool"
	m.AddStoragePool(agentmock.StoragePool{
		ID:   uuid.New(),
		Name: poolName,
		Type: "local_dir",
		Path: "/opt/otherix/pools/import-pool",
	})
	return m, poolName
}

// postImport issues a POST /v1/storage-pools/{pool_name}/images
// against the mock with the supplied request body and returns the
// agent task uuid (or t.Fatal on non-202).
func postImport(t *testing.T, m *agentmock.Mock, poolName string, req agentapi.ImageImportRequest) uuid.UUID {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	url := m.URL() + "/v1/storage-pools/" + poolName + "/images"
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post import: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("import status = %d, want 202", resp.StatusCode)
	}
	var accepted agentapi.AsyncTaskAccepted
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode accepted: %v", err)
	}
	if accepted.TaskId == uuid.Nil {
		t.Fatal("accepted.TaskId is zero")
	}
	return accepted.TaskId
}

func TestImport_DefaultSuccess(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0xa1)

	taskID := postImport(t, m, poolName, agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &checksum,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
	})

	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("status = %q, want success", task.Status)
	}
	if task.Result == nil {
		t.Fatalf("result = nil, want populated map")
	}
	r := *task.Result
	if got, _ := r["checksum_sha256"].(string); got != checksum {
		t.Errorf("result.checksum_sha256 = %q, want %q", got, checksum)
	}
	if got, _ := r["format"].(string); got != "qcow2" {
		t.Errorf("result.format = %q, want qcow2", got)
	}
	if got, _ := r["size_bytes"].(float64); int64(got) != 1<<20 {
		t.Errorf("result.size_bytes = %v, want %d (default 1 MiB)", got, int64(1<<20))
	}

	// Materialisation: the cached image is staged on terminal poll.
	img, ok := m.GetStoredImage(poolName, checksum)
	if !ok {
		t.Fatalf("GetStoredImage(%s, %s) = false; want staged after terminal-success", poolName, checksum)
	}
	if img.SizeBytes != 1<<20 || img.Format != "qcow2" {
		t.Errorf("img = {SizeBytes: %d, Format: %q}, want {1<<20, qcow2}", img.SizeBytes, img.Format)
	}
}

func TestImport_PreStaged(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0xb2)

	m.AddImageImportResult(poolName, checksum, agentmock.ImageImportResult{
		Status:    "success",
		SizeBytes: 5_000_000,
		Format:    "qcow2",
		Delay:     50 * time.Millisecond,
	})

	taskID := postImport(t, m, poolName, agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &checksum,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
	})
	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("status = %q, want success", task.Status)
	}

	r := *task.Result
	if got, _ := r["size_bytes"].(float64); int64(got) != 5_000_000 {
		t.Errorf("result.size_bytes = %v, want 5_000_000 (queued override)", got)
	}

	img, ok := m.GetStoredImage(poolName, checksum)
	if !ok {
		t.Fatalf("GetStoredImage = false; want staged")
	}
	if img.SizeBytes != 5_000_000 {
		t.Errorf("img.SizeBytes = %d, want 5_000_000", img.SizeBytes)
	}
}

func TestImport_PreExistingImageSkipsQueue(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0xc3)

	// Pre-stage the image — the import handler must observe this and
	// emit terminal-success without consuming the (failure) queue.
	if err := m.AddImage(poolName, agentmock.CachedImage{
		ChecksumSHA256: checksum,
		Format:         "qcow2",
		SizeBytes:      777,
		Path:           "/staged/path",
	}); err != nil {
		t.Fatalf("AddImage: %v", err)
	}

	// Queue a failure that must NOT fire because the image is already
	// staged. We assert non-consumption by re-using the queue head
	// after the first import completes.
	m.AddImageImportResult(poolName, checksum, agentmock.ImageImportResult{
		Status: "failed",
		Error:  &agentmock.ErrorEnvelope{Code: "synthetic_failure", Message: "should not fire"},
		Delay:  20 * time.Millisecond,
	})

	taskID := postImport(t, m, poolName, agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &checksum,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
	})
	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("status = %q, want success (pre-existing image short-circuits the failure queue)", task.Status)
	}
	r := *task.Result
	if got, _ := r["size_bytes"].(float64); int64(got) != 777 {
		t.Errorf("result.size_bytes = %v, want 777 (from pre-staged entry)", got)
	}

	// The failure is still queued — un-stage the image and re-import
	// to drain the queue, confirming non-consumption.
	if !m.EvictImage(poolName, checksum) {
		t.Fatalf("EvictImage returned false; pre-staged image was not in state.images")
	}
	taskID2 := postImport(t, m, poolName, agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &checksum,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
	})
	task2 := pollUntilTerminal(t, m, taskID2, 2*time.Second)
	if string(task2.Status) != "failed" {
		t.Fatalf("second import status = %q, want failed (queue head was preserved)", task2.Status)
	}
}

func TestImport_QueuedFailure(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0xd4)

	m.AddImageImportResult(poolName, checksum, agentmock.ImageImportResult{
		Status: "failed",
		Error: &agentmock.ErrorEnvelope{
			Code:    "image_checksum_mismatch",
			Message: "fetched bytes do not match expected_checksum_sha256",
		},
		Delay: 30 * time.Millisecond,
	})

	taskID := postImport(t, m, poolName, agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &checksum,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
	})
	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "failed" {
		t.Fatalf("status = %q, want failed", task.Status)
	}
	if task.Error == nil || task.Error.Code == nil || *task.Error.Code != "image_checksum_mismatch" {
		t.Fatalf("Error = %+v, want code=image_checksum_mismatch", task.Error)
	}
	if _, present := m.GetStoredImage(poolName, checksum); present {
		t.Errorf("GetStoredImage = true after failed import; want false (no materialisation)")
	}
}

func TestImport_PoolNotFound(t *testing.T) {
	m := agentmock.Start(t, agentmock.Options{})
	sha := importHexSha(0xe5)
	body, _ := json.Marshal(agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &sha,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
	})
	resp, err := http.Post(m.URL()+"/v1/storage-pools/"+uuid.NewString()+"/images",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDelete_HappyPath(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0xf6)

	if err := m.AddImage(poolName, agentmock.CachedImage{
		ChecksumSHA256: checksum,
		Format:         "qcow2",
		SizeBytes:      1024,
		Path:           "/p",
	}); err != nil {
		t.Fatalf("AddImage: %v", err)
	}

	url := m.URL() + "/v1/storage-pools/" + poolName + "/images/" + checksum
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, present := m.GetStoredImage(poolName, checksum); present {
		t.Errorf("image still present after delete")
	}
}

func TestDelete_MissingIsIdempotent(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0x07)

	url := m.URL() + "/v1/storage-pools/" + poolName + "/images/" + checksum
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (idempotent)", resp.StatusCode)
	}
}

func TestImport_ConcurrentSamePoolSamesha256(t *testing.T) {
	m, poolName := startImportMock(t)
	checksum := importHexSha(0xab)

	const n = 4
	taskIDs := make([]uuid.UUID, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			taskIDs[i] = postImport(t, m, poolName, agentapi.ImageImportRequest{
				ExpectedChecksumSha256: &checksum,
				Format:                 agentapi.ImageImportRequestFormat("qcow2"),
			})
		}(i)
	}
	wg.Wait()

	// All four task uuids must be distinct.
	seen := map[uuid.UUID]struct{}{}
	for _, id := range taskIDs {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate agent task id in concurrent imports: %s", id)
		}
		seen[id] = struct{}{}
	}

	// All four must reach terminal-success and end up with exactly
	// one entry in state.images for (pool, checksum).
	for _, id := range taskIDs {
		task := pollUntilTerminal(t, m, id, 3*time.Second)
		if string(task.Status) != "success" {
			t.Errorf("task %s status = %q, want success", id, task.Status)
		}
	}
	imgs := m.ListStoredImages(poolName)
	matches := 0
	for _, img := range imgs {
		if img.ChecksumSHA256 == checksum {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("staged entries for (pool, checksum) = %d, want 1; full list = %+v", matches, imgs)
	}
}
