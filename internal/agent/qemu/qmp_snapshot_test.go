// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"encoding/json"
	"testing"
)

func TestBlockdevBackupCmd(t *testing.T) {
	raw := blockdevBackupCmd("backup-disk0", "virtio0", "snap-target-disk0")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["execute"] != "blockdev-backup" {
		t.Errorf("execute = %v, want blockdev-backup", m["execute"])
	}
	args, ok := m["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments missing or wrong type: %v", m["arguments"])
	}
	want := map[string]string{
		"job-id": "backup-disk0",
		"device": "virtio0",
		"target": "snap-target-disk0",
		"sync":   "full",
	}
	for k, v := range want {
		if got, _ := args[k].(string); got != v {
			t.Errorf("arguments.%s = %v, want %q", k, args[k], v)
		}
	}
	if len(args) != len(want) {
		t.Errorf("arguments has %d keys, want %d: %v", len(args), len(want), args)
	}
}

func TestBlockdevAddQcow2FileCmd(t *testing.T) {
	raw := blockdevAddQcow2FileCmd("snap-target-disk0", "/pools/p1/snapshots/.staging/x.qcow2")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["execute"] != "blockdev-add" {
		t.Errorf("execute = %v, want blockdev-add", m["execute"])
	}
	args, ok := m["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments missing or wrong type: %v", m["arguments"])
	}
	if args["driver"] != "qcow2" {
		t.Errorf("driver = %v, want qcow2", args["driver"])
	}
	if args["node-name"] != "snap-target-disk0" {
		t.Errorf("node-name = %v, want snap-target-disk0", args["node-name"])
	}
	file, ok := args["file"].(map[string]any)
	if !ok {
		t.Fatalf("file node missing or wrong type: %v", args["file"])
	}
	if file["driver"] != "file" {
		t.Errorf("file.driver = %v, want file", file["driver"])
	}
	if file["filename"] != "/pools/p1/snapshots/.staging/x.qcow2" {
		t.Errorf("file.filename = %v, want the staging path", file["filename"])
	}
}
