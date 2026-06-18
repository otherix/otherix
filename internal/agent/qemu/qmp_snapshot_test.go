// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
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

// fakeBackupConn is a BackupConn test double that records the ordered call
// sequence and serves events from a pre-loaded channel. It models go-qemu's
// real single-producer contract: BlockdevBackup does a BLOCKING send of the
// triggering BLOCK_JOB_COMPLETED event before returning (the job's status event
// can reach the wire before the command's reply, and go-qemu's listen goroutine
// blocks delivering the event until someone receives it - which also blocks
// delivery of the reply). So if runBackup does not keep the event channel
// drained from the moment it subscribes, BlockdevBackup deadlocks and the
// completion event is lost. This is the exact hang the real smoke caught.
type fakeBackupConn struct {
	mu     sync.Mutex
	calls  []string
	events chan qmp.Event

	// completeJobID, when non-empty, is the job id BlockdevBackup emits a
	// terminal event for (blocking send) right after recording its call.
	completeJobID string
	// completeEvent is the event name emitted (BLOCK_JOB_COMPLETED by default).
	completeEvent string
	// completeError, when non-empty, is attached as the event's "error" field so
	// the waiter must surface it instead of treating the job as done.
	completeError string
	// emitOnBackup gates the blocking emit: when false BlockdevBackup returns
	// without emitting (models a genuinely stuck job whose event never arrives).
	emitOnBackup bool
}

func (f *fakeBackupConn) rec(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeBackupConn) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBackupConn) Events(context.Context) (<-chan qmp.Event, error) {
	return f.events, nil
}

func (f *fakeBackupConn) BlockdevAddQcow2File(nodeName, filename string) error {
	f.rec("blockdev-add")
	return nil
}

func (f *fakeBackupConn) BlockdevBackup(jobID, device, target string) error {
	f.rec("blockdev-backup")
	if f.emitOnBackup {
		name := f.completeEvent
		if name == "" {
			name = "BLOCK_JOB_COMPLETED"
		}
		ev := qmp.Event{Event: name, Data: map[string]interface{}{"device": f.completeJobID}}
		if f.completeError != "" {
			ev.Data["error"] = f.completeError
		}
		// BLOCKING send: models the producer that cannot return BlockdevBackup's
		// reply until the event is drained. Without a continuous fan-out drainer
		// in runBackup, this deadlocks.
		f.events <- ev
	}
	return nil
}

func (f *fakeBackupConn) BlockdevDel(nodeName string) error {
	f.rec("blockdev-del")
	return nil
}

// TestRunBackup_CompletionEventNotLost is the regression for the smoke hang: the
// BLOCK_JOB_COMPLETED event is emitted by BlockdevBackup via a BLOCKING send
// (the producer cannot deliver the command reply until the event is drained).
// runBackup must fan the event channel out from the moment it subscribes so the
// completion event is never lost, then return nil. A non-draining waiter (the
// old raw-channel BackupDiskToFile) wedges forever here.
func TestRunBackup_CompletionEventNotLost(t *testing.T) {
	f := &fakeBackupConn{
		events:        make(chan qmp.Event), // UNBUFFERED: one producer, no slack
		completeJobID: "snap-abc",
		emitOnBackup:  true,
	}

	done := make(chan error, 1)
	go func() {
		done <- runBackup(context.Background(), f, "snap-abc", "snaptgt-abc", "virtio0", "/p/x.qcow2", time.Second)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runBackup() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runBackup deadlocked: BlockdevBackup's blocking completion event was not drained (lost-event hang)")
	}

	got := f.snapshot()
	want := []string{"blockdev-add", "blockdev-backup", "blockdev-del"}
	if len(got) != len(want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRunBackup_FailsClosedOnTimeout asserts that a backup whose
// BLOCK_JOB_COMPLETED never arrives (a genuinely stuck job) fails closed within
// the timeout instead of hanging the task forever, and still drops the orphaned
// target node.
func TestRunBackup_FailsClosedOnTimeout(t *testing.T) {
	f := &fakeBackupConn{
		events:       make(chan qmp.Event, 1), // no event ever fed
		emitOnBackup: false,
	}

	done := make(chan error, 1)
	go func() {
		done <- runBackup(context.Background(), f, "snap-abc", "snaptgt-abc", "virtio0", "/p/x.qcow2", 80*time.Millisecond)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runBackup() = nil, want timeout error on a stuck backup")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runBackup did not fail closed on a stuck backup within the timeout")
	}
	if got := f.snapshot(); !contains(got, "blockdev-del") {
		t.Errorf("calls = %v, want blockdev-del cleanup on the timeout path", got)
	}
}

// TestRunBackup_SurfacesJobError asserts an errored BLOCK_JOB_COMPLETED fails
// closed (surfaces the job error) rather than being treated as success or
// silently waited out.
func TestRunBackup_SurfacesJobError(t *testing.T) {
	f := &fakeBackupConn{
		events:        make(chan qmp.Event, 1),
		completeJobID: "snap-abc",
		completeError: "No space left on device",
		emitOnBackup:  true,
	}

	err := runBackup(context.Background(), f, "snap-abc", "snaptgt-abc", "virtio0", "/p/x.qcow2", time.Second)
	if err == nil {
		t.Fatal("runBackup() = nil, want error on an errored BLOCK_JOB_COMPLETED")
	}
	if got := f.snapshot(); !contains(got, "blockdev-del") {
		t.Errorf("calls = %v, want blockdev-del cleanup on the error path", got)
	}
}
