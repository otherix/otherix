// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestListTemplates_HappyPath(t *testing.T) {
	t.Parallel()
	tplID := uuid.NewString()
	ownerID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/templates" {
			t.Errorf("path = %s, want /v1/templates", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":                        tplID,
					"owner_id":                  ownerID,
					"name":                      "ubuntu-jammy",
					"description":               "",
					"visibility":                "public",
					"architecture":              "arm64",
					"os_family":                 "linux",
					"os_variant":                "ubuntu22.04",
					"image_url":                 "https://example.com/jammy.qcow2",
					"image_checksum_sha256":     "abcd1234",
					"image_format":              "qcow2",
					"image_size_bytes":          int64(1024),
					"firmware_type":             "uefi",
					"firmware_id":               nil,
					"default_cpu_cores":         2,
					"default_memory_mib":        2048,
					"default_disk_gib":          10,
					"cloud_init_supported":      true,
					"qemu_guest_agent_expected": false,
					"metadata":                  map[string]any{},
					"created_at":                "2026-05-11T10:00:00Z",
					"updated_at":                "2026-05-11T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListTemplates(context.Background(), cpclient.ListTemplatesParams{})
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(got.Data))
	}
	if got.Data[0].Name != "ubuntu-jammy" {
		t.Errorf("Name = %s, want ubuntu-jammy", got.Data[0].Name)
	}
	if got.Data[0].Architecture != "arm64" {
		t.Errorf("Architecture = %s, want arm64", got.Data[0].Architecture)
	}
}

func TestListTemplates_FiltersEncoded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("visibility") != "public" {
			t.Errorf("visibility = %q, want public", q.Get("visibility"))
		}
		if q.Get("architecture") != "amd64" {
			t.Errorf("architecture = %q, want amd64", q.Get("architecture"))
		}
		if q.Get("os_family") != "linux" {
			t.Errorf("os_family = %q, want linux", q.Get("os_family"))
		}
		if q.Get("limit") != "25" {
			t.Errorf("limit = %q, want 25", q.Get("limit"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.ListTemplates(context.Background(), cpclient.ListTemplatesParams{
		Limit:        25,
		Visibility:   "public",
		Architecture: "amd64",
		OSFamily:     "linux",
	}); err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
}

func TestListTemplates_Pagination(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cursor"); got != "abc" {
			t.Errorf("cursor = %q, want abc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": "def"},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListTemplates(context.Background(), cpclient.ListTemplatesParams{Cursor: "abc"})
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if got.Meta.NextCursor == nil || *got.Meta.NextCursor != "def" {
		t.Errorf("NextCursor = %v, want def", got.Meta.NextCursor)
	}
}

func TestListTemplates_Unauthenticated(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"missing bearer"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.ListTemplates(context.Background(), cpclient.ListTemplatesParams{})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "unauthenticated" {
		t.Errorf("Code = %s, want unauthenticated", ae.Code)
	}
}

func TestGetTemplate_ByUUID(t *testing.T) {
	t.Parallel()
	tplID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates/"+tplID {
			t.Errorf("path = %s, want /v1/templates/<uuid>", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                        tplID,
			"owner_id":                  uuid.NewString(),
			"name":                      "ubuntu-jammy",
			"description":               "",
			"visibility":                "public",
			"architecture":              "arm64",
			"os_family":                 "linux",
			"os_variant":                "",
			"image_url":                 "https://example.com/x.qcow2",
			"image_checksum_sha256":     "deadbeef",
			"image_format":              "qcow2",
			"image_size_bytes":          nil,
			"firmware_type":             "uefi",
			"firmware_id":               nil,
			"default_cpu_cores":         2,
			"default_memory_mib":        2048,
			"default_disk_gib":          10,
			"cloud_init_supported":      true,
			"qemu_guest_agent_expected": false,
			"metadata":                  map[string]any{},
			"created_at":                "2026-05-11T10:00:00Z",
			"updated_at":                "2026-05-11T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetTemplate(context.Background(), tplID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.Name != "ubuntu-jammy" {
		t.Errorf("Name = %s, want ubuntu-jammy", got.Name)
	}
}

func TestGetTemplate_ByName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates/ubuntu-jammy" {
			t.Errorf("path = %s, want /v1/templates/ubuntu-jammy", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000001","owner_id":"00000000-0000-0000-0000-000000000002","name":"ubuntu-jammy","visibility":"public","architecture":"arm64","os_family":"linux","image_url":"x","image_checksum_sha256":"d","image_format":"qcow2","firmware_type":"uefi","default_cpu_cores":2,"default_memory_mib":2048,"default_disk_gib":10,"cloud_init_supported":true,"qemu_guest_agent_expected":false,"metadata":{},"created_at":"2026-05-11T10:00:00Z","updated_at":"2026-05-11T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.GetTemplate(context.Background(), "ubuntu-jammy"); err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
}

func TestGetTemplate_404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"template not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.GetTemplate(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", ae.Code)
	}
}

func TestGetTemplate_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.GetTemplate(context.Background(), "ubuntu-jammy")
	if err == nil {
		t.Fatal("expected decode error")
	}
}

func TestCreateTemplate_HappyPath(t *testing.T) {
	t.Parallel()
	tplID := uuid.NewString()
	ownerID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/templates" {
			t.Errorf("path = %s, want /v1/templates", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["name"] != "ubuntu-jammy" {
			t.Errorf("name = %v, want ubuntu-jammy", body["name"])
		}
		if body["architecture"] != "arm64" {
			t.Errorf("architecture = %v, want arm64", body["architecture"])
		}
		if body["image_checksum_sha256"] != "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234" {
			t.Errorf("image_checksum_sha256 mismatch: %v", body["image_checksum_sha256"])
		}
		if _, present := body["visibility"]; present {
			t.Errorf("visibility key must not appear in request body")
		}
		if _, present := body["image_format"]; present {
			t.Errorf("image_format omitted when caller did not set it (server defaults)")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": tplID, "owner_id": ownerID, "name": "ubuntu-jammy",
			"description": "", "visibility": "private",
			"architecture": "arm64", "os_family": "linux", "os_variant": "",
			"image_url":             "https://example.com/jammy.qcow2",
			"image_checksum_sha256": "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234",
			"image_format":          "qcow2", "image_size_bytes": nil,
			"firmware_type": "uefi", "firmware_id": nil,
			"default_cpu_cores": 2, "default_memory_mib": 2048, "default_disk_gib": 20,
			"cloud_init_supported": true, "qemu_guest_agent_expected": false,
			"metadata":   map[string]any{},
			"created_at": "2026-05-13T10:00:00Z", "updated_at": "2026-05-13T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.CreateTemplate(context.Background(), cpclient.CreateTemplateRequest{
		Name:                "ubuntu-jammy",
		Architecture:        "arm64",
		OSFamily:            "linux",
		ImageURL:            "https://example.com/jammy.qcow2",
		ImageChecksumSHA256: "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234",
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if got.Name != "ubuntu-jammy" {
		t.Errorf("Name = %s, want ubuntu-jammy", got.Name)
	}
	if got.Visibility != "private" {
		t.Errorf("Visibility = %s, want private (server default)", got.Visibility)
	}
}

func TestCreateTemplate_OptionalFieldsSerialised(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000001","owner_id":"00000000-0000-0000-0000-000000000002","name":"x","visibility":"private","architecture":"amd64","os_family":"linux","image_url":"u","image_checksum_sha256":"d","image_format":"raw","firmware_type":"bios","default_cpu_cores":4,"default_memory_mib":4096,"default_disk_gib":40,"cloud_init_supported":false,"qemu_guest_agent_expected":true,"metadata":{},"created_at":"2026-05-13T10:00:00Z","updated_at":"2026-05-13T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	desc := "test"
	osVar := "ubuntu22.04"
	format := "raw"
	fwType := "bios"
	cpu := int32(4)
	mem := int32(4096)
	disk := int32(40)
	cloud := false
	qga := true
	_, err := c.CreateTemplate(context.Background(), cpclient.CreateTemplateRequest{
		Name:                   "x",
		Description:            &desc,
		Architecture:           "amd64",
		OSFamily:               "linux",
		OSVariant:              &osVar,
		ImageURL:               "u",
		ImageChecksumSHA256:    "d",
		ImageFormat:            &format,
		FirmwareType:           &fwType,
		DefaultCPUCores:        &cpu,
		DefaultMemoryMiB:       &mem,
		DefaultDiskGiB:         &disk,
		CloudInitSupported:     &cloud,
		QemuGuestAgentExpected: &qga,
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if captured["description"] != "test" {
		t.Errorf("description = %v, want test", captured["description"])
	}
	if captured["image_format"] != "raw" {
		t.Errorf("image_format = %v, want raw", captured["image_format"])
	}
	if captured["firmware_type"] != "bios" {
		t.Errorf("firmware_type = %v, want bios", captured["firmware_type"])
	}
	if captured["qemu_guest_agent_expected"] != true {
		t.Errorf("qemu_guest_agent_expected = %v, want true", captured["qemu_guest_agent_expected"])
	}
}

func TestCreateTemplate_409ReturnsSentinel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"template name already exists"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.CreateTemplate(context.Background(), cpclient.CreateTemplateRequest{
		Name: "dup", Architecture: "arm64", OSFamily: "linux",
		ImageURL: "u", ImageChecksumSHA256: "d",
	})
	if !errors.Is(err, cpclient.ErrTemplateExists) {
		t.Fatalf("err = %v, want errors.Is(err, ErrTemplateExists)", err)
	}
}

func TestCreateTemplate_400Validation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"image_checksum_sha256 must be exactly 64 hex characters"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.CreateTemplate(context.Background(), cpclient.CreateTemplateRequest{
		Name: "bad", Architecture: "arm64", OSFamily: "linux",
		ImageURL: "u", ImageChecksumSHA256: "short",
	})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "validation_failed" {
		t.Errorf("Code = %s, want validation_failed", apiErr.Code)
	}
}

func TestDeleteTemplate_Happy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/templates/ubuntu-jammy" {
			t.Errorf("path = %s, want /v1/templates/ubuntu-jammy", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.DeleteTemplate(context.Background(), "ubuntu-jammy"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
}

func TestDeleteTemplate_BlockedByStorageImages(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"template still has materialised storage images; delete them first","details":{"blocking_resources":{"storage_images":3,"vms":1},"kind":"template"}}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeleteTemplate(context.Background(), "ubuntu-jammy")
	var blocked *cpclient.ErrTemplateBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *ErrTemplateBlocked", err)
	}
	if blocked.Code != "resource_in_use" {
		t.Errorf("Code = %s, want resource_in_use", blocked.Code)
	}
	if blocked.Resources["storage_images"] != 3 {
		t.Errorf("storage_images = %d, want 3", blocked.Resources["storage_images"])
	}
	if blocked.Resources["vms"] != 1 {
		t.Errorf("vms = %d, want 1", blocked.Resources["vms"])
	}
}

func TestDeleteTemplate_BlockedByVMsOnly(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"template is in use; remove or migrate the dependent virtual machines first","details":{"blocking_resources":{"vms":2}}}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeleteTemplate(context.Background(), "ubuntu-jammy")
	var blocked *cpclient.ErrTemplateBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *ErrTemplateBlocked", err)
	}
	if blocked.Code != "conflict" {
		t.Errorf("Code = %s, want conflict", blocked.Code)
	}
	if blocked.Resources["vms"] != 2 {
		t.Errorf("vms = %d, want 2", blocked.Resources["vms"])
	}
	if _, present := blocked.Resources["storage_images"]; present {
		t.Errorf("storage_images key should not appear when only VMs block")
	}
}

func TestDeleteTemplate_404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"template not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeleteTemplate(context.Background(), "missing")
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", apiErr.Code)
	}
}

func TestDeleteTemplate_409WithoutBlockingResources(t *testing.T) {
	t.Parallel()
	// Defensive: if CP ever returns 409 without details.blocking_resources,
	// CLI must surface raw *APIError (not crash, not wrap blindly).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"something else"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeleteTemplate(context.Background(), "anything")
	var blocked *cpclient.ErrTemplateBlocked
	if errors.As(err, &blocked) {
		t.Fatalf("err = %v, want plain *APIError (no blocking_resources in payload)", err)
	}
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError fallback", err)
	}
}

func TestImportTemplateImage_Happy(t *testing.T) {
	t.Parallel()
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/templates/ubuntu-jammy/images" {
			t.Errorf("path = %s, want /v1/templates/ubuntu-jammy/images", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["pool"] != "pool-mvp" {
			t.Errorf("pool = %v, want pool-mvp", body["pool"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ImportTemplateImage(context.Background(), "ubuntu-jammy", cpclient.ImportTemplateImageRequest{
		Pool: "pool-mvp",
	})
	if err != nil {
		t.Fatalf("ImportTemplateImage: %v", err)
	}
	if got.TaskID != taskID {
		t.Errorf("TaskID = %s, want %s", got.TaskID, taskID)
	}
	if got.Status != "pending" {
		t.Errorf("Status = %s, want pending", got.Status)
	}
}

func TestImportTemplateImage_404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"storage pool not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.ImportTemplateImage(context.Background(), "ubuntu-jammy", cpclient.ImportTemplateImageRequest{
		Pool: "missing-pool",
	})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", apiErr.Code)
	}
}
