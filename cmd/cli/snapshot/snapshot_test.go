// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshot_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/snapshot"
)

const (
	testSnapshotID = "11111111-1111-1111-1111-111111111111"
	testTaskID     = "22222222-2222-2222-2222-222222222222"
)

// runSnapshotCmd mounts the `snapshot` subcommand tree on a throwaway parent
// with the persistent flags the real root provides, then executes args.
// Returns captured stdout / stderr and the cobra error. Mirrors vm.runVMCmd.
func runSnapshotCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := snapshot.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	full := append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...)
	parent.SetArgs(full)
	parent.SilenceUsage = true
	parent.SilenceErrors = true
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

// snapshotJSON emits a minimal valid VmSnapshot projection the CLI can decode.
func snapshotJSON(id, name, status string) map[string]any {
	return map[string]any{
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
}

func taskJSON(id, status string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"id":            id,
		"type":          "vm.snapshot.delete",
		"status":        status,
		"resource_type": "snapshot",
		"resource_id":   testSnapshotID,
		"attempts":      1,
		"max_attempts":  25,
		"created_at":    "2026-06-17T10:00:00Z",
	})
	return raw
}

// TestList renders the global text table: short ids, resolved vm names and
// owner display names, status and size columns.
func TestList(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/snapshots" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		item := snapshotJSON(testSnapshotID, "daily", "ready")
		item["vm_name"] = "web-01"
		item["owner_display_name"] = "alice"
		envelope := map[string]any{
			"data": []map[string]any{item},
			"meta": map[string]any{"next_cursor": nil},
		}
		raw, _ := json.Marshal(envelope)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, stderr, err := runSnapshotCmd(t, srv.URL, []string{"list"})
	if err != nil {
		t.Fatalf("list err = %v (stderr=%s)", err, stderr)
	}
	// Header columns.
	if !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "VM") ||
		!strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "OWNER") ||
		!strings.Contains(stdout, "SIZE") || !strings.Contains(stdout, "AGE") {
		t.Errorf("table header missing columns:\n%s", stdout)
	}
	// Short id (first 8 chars) shown; full UUID must not leak into the table.
	if !strings.Contains(stdout, "11111111") {
		t.Errorf("table missing short id:\n%s", stdout)
	}
	if strings.Contains(stdout, testSnapshotID) {
		t.Errorf("table leaked the full snapshot id:\n%s", stdout)
	}
	for _, want := range []string{"web-01", "daily", "ready", "alice", "1.0GiB"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
}

// TestList_Filters covers --vm and --status passing through to the query string.
func TestList_Filters(t *testing.T) {
	t.Parallel()
	const vmFilter = "33333333-3333-3333-3333-333333333333"
	var gotVM, gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVM = r.URL.Query().Get("vm")
		gotStatus = r.URL.Query().Get("status")
		envelope := map[string]any{"data": []map[string]any{}, "meta": map[string]any{"next_cursor": nil}}
		raw, _ := json.Marshal(envelope)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	_, stderr, err := runSnapshotCmd(t, srv.URL, []string{"list", "--vm", vmFilter, "--status", "ready"})
	if err != nil {
		t.Fatalf("list --vm --status err = %v (stderr=%s)", err, stderr)
	}
	if gotVM != vmFilter {
		t.Errorf("vm query = %q, want %q", gotVM, vmFilter)
	}
	if gotStatus != "ready" {
		t.Errorf("status query = %q, want ready", gotStatus)
	}
}

// TestGet_JSON echoes the server JSON verbatim under -o json.
func TestGet_JSON(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(snapshotJSON(testSnapshotID, "daily", "ready"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/snapshots/"+testSnapshotID {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, stderr, err := runSnapshotCmd(t, srv.URL, []string{"get", testSnapshotID, "-o", "json"})
	if err != nil {
		t.Fatalf("get -o json err = %v (stderr=%s)", err, stderr)
	}
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

// TestGet_Text renders the key=value text view.
func TestGet_Text(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(snapshotJSON(testSnapshotID, "daily", "ready"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, stderr, err := runSnapshotCmd(t, srv.URL, []string{"get", testSnapshotID})
	if err != nil {
		t.Fatalf("get err = %v (stderr=%s)", err, stderr)
	}
	for _, want := range []string{"name: daily", "status: ready"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text view missing %q:\n%s", want, stdout)
		}
	}
}

// TestDelete_Wait covers `snapshot delete <id> --wait`: DELETE then poll.
func TestDelete_Wait(t *testing.T) {
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

	stdout, stderr, err := runSnapshotCmd(t, srv.URL, []string{"delete", testSnapshotID, "--wait"})
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
