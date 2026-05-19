// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

// seedTemplate inserts a template owned by ownerID with the supplied
// name + checksum byte (32 identical bytes — handy for fixture
// distinguishability without pulling crypto/rand). Visibility defaults
// to 'private' per the templates schema; tests that need public
// visibility flip it via SetTemplateVisibility after the fact.
func seedTemplate(t *testing.T, ctx context.Context, s *store.Store, ownerID uuid.UUID, name string, checksumByte byte) store.Template {
	t.Helper()
	checksum := bytes.Repeat([]byte{checksumByte}, 32)
	tpl, err := s.Queries().CreateTemplate(ctx, store.CreateTemplateParams{
		ID:                     uuid.New(),
		OwnerID:                ownerID,
		Name:                   name + "-" + uuid.NewString()[:8],
		Description:            "vertical-slice fixture",
		Architecture:           store.CpuArchAmd64,
		OsFamily:               store.OsFamilyLinux,
		OsVariant:              "ubuntu-24.04",
		ImageUrl:               "https://images.example.test/" + name + ".qcow2",
		ImageChecksumSha256:    checksum,
		ImageFormat:            store.ImageFormatQcow2,
		ImageSizeBytes:         nil,
		FirmwareType:           store.FirmwareTypeUefi,
		FirmwareID:             nil,
		DefaultCpuCores:        2,
		DefaultMemoryMib:       2048,
		DefaultDiskGib:         20,
		CloudInitSupported:     true,
		QemuGuestAgentExpected: false,
		Metadata:               []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	return tpl
}

// hexChecksum mirrors agent_import_executor.go's wire conversion: the
// agent receives `expected_checksum_sha256` as lowercase hex of the
// CP-side bytea. Mock-agent storedImages buckets are keyed on the
// same string.
func hexChecksum(b []byte) string {
	return hex.EncodeToString(b)
}

// importImage POSTs /v1/templates/{templateID}/images, asserts 202,
// and returns the decoded task uuid plus the raw response body. An
// empty idemKey omits the Idempotency-Key header.
func (v *verticalSlice) importImage(t *testing.T, ctx context.Context, templateID uuid.UUID, idemKey string) (uuid.UUID, []byte) {
	t.Helper()
	row, err := v.store.Queries().GetTemplate(ctx, templateID)
	if err != nil {
		t.Fatalf("resolve template name for %s: %v", templateID, err)
	}
	body, err := json.Marshal(map[string]string{"pool": v.pool.ID.String()})
	if err != nil {
		t.Fatalf("marshal import body: %v", err)
	}
	url := v.cpServer.URL + "/v1/templates/" + row.Name + "/images"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new import request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("import POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read import body: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("import status = %d, body = %s, want 202", resp.StatusCode, respBody)
	}
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		t.Fatalf("decode AsyncTaskAccepted: %v", err)
	}
	id, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("task_id %q is not a uuid: %v", accepted.TaskID, err)
	}
	return id, respBody
}

// awaitImportEvent drains the river completions channel until a
// storage_image.import event surfaces (or the deadline fires).
func (v *verticalSlice) awaitImportEvent(t *testing.T, deadline time.Duration) *river.Event {
	t.Helper()
	timer := time.After(deadline)
	for {
		select {
		case ev := <-v.completions:
			if ev.Job.Kind == "storage_image.import" {
				return ev
			}
		case <-timer:
			t.Fatalf("storage_image.import event did not arrive within %v", deadline)
		}
	}
}

// agentImportCallCount returns the number of POST /v1/storage-pools/{id}/images
// calls observed by the mock — used to verify resumption (no second
// POST) and idempotency (one POST per unique Idempotency-Key).
func (v *verticalSlice) agentImportCallCount() int {
	return v.countOperation("storageImages.import")
}

// agentDeleteImageCallCount returns the number of DELETE /v1/storage-pools/{id}/images/{checksum}
// calls observed by the mock — used to verify refcount-gated deletes
// did not invoke the agent on multi-referent rows.
func (v *verticalSlice) agentDeleteImageCallCount() int {
	return v.countOperation("storageImages.delete")
}

func (v *verticalSlice) countOperation(opID string) int {
	count := 0
	for _, rec := range v.mock.ReceivedRequests() {
		if rec.OperationID == opID {
			count++
		}
	}
	return count
}

// deleteImage issues DELETE /v1/storage-pools/{poolID}/images/{imageID}
// authenticated as admin. Returns the response (caller closes the
// body after inspection).
func (v *verticalSlice) deleteImage(t *testing.T, ctx context.Context, poolID, imageID uuid.UUID, idemKey string) *http.Response {
	t.Helper()
	url := v.cpServer.URL + "/v1/storage-pools/" + poolID.String() + "/images/" + imageID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	return resp
}

// pollStorageImageRowGone polls until ListStorageImagesByPool no
// longer reports the supplied image_id (or the deadline fires).
// Used by delete tests that want to verify row-gone after a sync
// 200 response — sync delete commits before returning, so this
// should be observable immediately, but a tiny grace loop guards
// against scheduler weirdness under -race.
func (v *verticalSlice) pollStorageImageRowGone(t *testing.T, ctx context.Context, imageID uuid.UUID, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		_, err := v.store.Queries().GetStorageImageByID(ctx, imageID)
		if err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("storage_images row %s still present after %v", imageID, deadline)
}
