// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testSnapshotID = "11111111-1111-1111-1111-111111111111"
	testTaskID     = "22222222-2222-2222-2222-222222222222"
)

// snapshotJSON emits a minimal valid VmSnapshot projection the CLI can decode.
func snapshotJSON(id, name, status string) []byte {
	body := map[string]any{
		"id":                   id,
		"vm_id":                "33333333-3333-3333-3333-333333333333",
		"owner_id":             "44444444-4444-4444-4444-444444444444",
		"name":                 name,
		"description":          "",
		"status":               status,
		"with_memory":          false,
		"vm_state_at_snapshot": "stopped",
		"architecture":         "arm64",
		"disks": []map[string]any{
			{"index": 0, "device": "virtio0", "sha256": "abc123", "size_bytes": 1073741824},
		},
		"disk_size_bytes": 1073741824,
		"created_at":      "2026-06-17T10:00:00Z",
		"updated_at":      "2026-06-17T10:00:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

func taskJSON(id, status string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"id":            id,
		"type":          "vm.snapshot.create",
		"status":        status,
		"resource_type": "snapshot",
		"resource_id":   testSnapshotID,
		"attempts":      1,
		"max_attempts":  25,
		"created_at":    "2026-06-17T10:00:00Z",
	})
	return raw
}

// TestSnapshotCreate_Wait covers `vm snapshot create <vm> --name daily --wait`:
// the CLI POSTs /v1/vms/{vm}/snapshots, then polls the backing task to success.
func TestSnapshotCreate_Wait(t *testing.T) {
	t.Parallel()
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/myvm/snapshots":
			posted = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["name"] != "daily" {
				t.Errorf("name = %v, want daily", body["name"])
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + testTaskID + `","status":"pending","links":{"self":"/v1/tasks/` + testTaskID + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+testTaskID:
			_, _ = w.Write(taskJSON(testTaskID, "success"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "create", "myvm", "--name", "daily", "--wait"})
	if err != nil {
		t.Fatalf("create --wait err = %v (stderr=%s)", err, stderr)
	}
	if !posted {
		t.Errorf("snapshot create did not POST")
	}
	if !strings.Contains(stdout, "task="+testTaskID) {
		t.Errorf("stdout missing task id:\n%s", stdout)
	}
	if !strings.Contains(stdout, "complete") {
		t.Errorf("stdout missing completion line:\n%s", stdout)
	}
}

// TestSnapshotList renders the text table for `vm snapshot list <vm>`.
func TestSnapshotList(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vms/myvm/snapshots" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		envelope := map[string]any{
			"data": []json.RawMessage{snapshotJSON(testSnapshotID, "daily", "ready")},
			"meta": map[string]any{"next_cursor": nil},
		}
		raw, _ := json.Marshal(envelope)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "list", "myvm"})
	if err != nil {
		t.Fatalf("list err = %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "daily") {
		t.Errorf("table missing snapshot name:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("table missing status:\n%s", stdout)
	}
}

// TestSnapshotGet_JSON echoes the server JSON verbatim under -o json.
func TestSnapshotGet_JSON(t *testing.T) {
	t.Parallel()
	raw := snapshotJSON(testSnapshotID, "daily", "ready")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/snapshots/"+testSnapshotID {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "get", testSnapshotID, "-o", "json"})
	if err != nil {
		t.Fatalf("get -o json err = %v (stderr=%s)", err, stderr)
	}
	// The server projection should round-trip through the JSON output.
	var want, got map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal stdout %q: %v", stdout, err)
	}
	if got["id"] != want["id"] || got["status"] != want["status"] {
		t.Errorf("json mismatch:\ngot  %v\nwant %v", got, want)
	}
}

// TestSnapshotGet_Text renders the key=value text view.
func TestSnapshotGet_Text(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(snapshotJSON(testSnapshotID, "daily", "ready"))
	}))
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "get", testSnapshotID})
	if err != nil {
		t.Fatalf("get err = %v (stderr=%s)", err, stderr)
	}
	for _, want := range []string{"name: daily", "status: ready"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text view missing %q:\n%s", want, stdout)
		}
	}
}

// TestSnapshotDelete_Wait covers `vm snapshot delete <id> --wait`: DELETE then poll.
func TestSnapshotDelete_Wait(t *testing.T) {
	t.Parallel()
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/snapshots/"+testSnapshotID:
			deleted = true
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + testTaskID + `","status":"pending","links":{"self":"/v1/tasks/` + testTaskID + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+testTaskID:
			_, _ = w.Write(taskJSON(testTaskID, "success"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "delete", testSnapshotID, "--wait"})
	if err != nil {
		t.Fatalf("delete --wait err = %v (stderr=%s)", err, stderr)
	}
	if !deleted {
		t.Errorf("snapshot delete did not DELETE")
	}
	if !strings.Contains(stdout, "complete") {
		t.Errorf("stdout missing completion line:\n%s", stdout)
	}
}

// TestCreateFromSnapshotXorImageURL enforces the CLI-edge exclusivity check.
func TestCreateFromSnapshotXorImageURL(t *testing.T) {
	t.Parallel()
	// No server call should be made; the CLI rejects before dispatch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s; CLI should reject before dispatch", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create", "web1", "--image-url", "https://example/img.qcow2", "--from-snapshot", testSnapshotID, "--arch", "arm64",
	})
	if err == nil {
		t.Fatalf("create with both --image-url and --from-snapshot: err = nil, want exclusivity error")
	}
}
