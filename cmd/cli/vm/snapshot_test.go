// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

const (
	testSnapshotID = "11111111-1111-1111-1111-111111111111"
	testTaskID     = "22222222-2222-2222-2222-222222222222"
)

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

// TestSnapshotVerb_NamedWait covers `vm snapshot <vm> --name daily --wait`:
// the CLI POSTs /v1/vms/{vm}/snapshots with name=daily, then polls the backing
// task to success.
func TestSnapshotVerb_NamedWait(t *testing.T) {
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

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "myvm", "--name", "daily", "--wait"})
	if err != nil {
		t.Fatalf("snapshot --wait err = %v (stderr=%s)", err, stderr)
	}
	if !posted {
		t.Errorf("vm snapshot did not POST")
	}
	if !strings.Contains(stdout, "task="+testTaskID) {
		t.Errorf("stdout missing task id:\n%s", stdout)
	}
	if !strings.Contains(stdout, "complete") {
		t.Errorf("stdout missing completion line:\n%s", stdout)
	}
}

// snapTimestampName matches the snap<unix_seconds> default name shape.
var snapTimestampName = regexp.MustCompile(`^snap[0-9]{8,}$`)

// TestSnapshotVerb_DefaultName covers `vm snapshot <vm>` with no --name: the
// CLI synthesizes a snap<unix_seconds> name, POSTs it, and prints it so the
// operator sees the chosen name.
func TestSnapshotVerb_DefaultName(t *testing.T) {
	t.Parallel()
	var postedName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/myvm/snapshots" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		postedName, _ = body["name"].(string)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":"` + testTaskID + `","status":"pending","links":{"self":"/v1/tasks/` + testTaskID + `"}}`))
	}))
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, []string{"snapshot", "myvm"})
	if err != nil {
		t.Fatalf("snapshot err = %v (stderr=%s)", err, stderr)
	}
	if !snapTimestampName.MatchString(postedName) {
		t.Errorf("posted name %q does not match snap<unix_seconds> shape", postedName)
	}
	// The chosen default name must be printed so the operator can query it.
	if !strings.Contains(stdout, "snapshot name: "+postedName) {
		t.Errorf("stdout did not print the chosen default name %q:\n%s", postedName, stdout)
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
