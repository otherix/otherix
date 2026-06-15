// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
)

func TestNBDServerStartCmd_LegacyAddrShape(t *testing.T) {
	got := nbdServerStartCmd("0.0.0.0", 49153, "tls0", "authz0")
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	args := m["arguments"].(map[string]any)
	addr := args["addr"].(map[string]any)
	if addr["type"] != "inet" {
		t.Errorf("addr.type = %v, want inet", addr["type"])
	}
	data := addr["data"].(map[string]any)
	if data["port"] != "49153" { // STRING, legacy tagged form
		t.Errorf("addr.data.port = %v (%T), want string \"49153\"", data["port"], data["port"])
	}
	if args["tls-creds"] != "tls0" || args["tls-authz"] != "authz0" {
		t.Errorf("tls fields = %v/%v", args["tls-creds"], args["tls-authz"])
	}
}

func TestBlockdevAddNBDCmd_FlatAddr_StringPort(t *testing.T) {
	got := blockdevAddNBDCmd("mirror-target", "10.0.0.2", 49153, "migrate-disk0", "tls0", "node-b.agents.otherix.local")
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	args := m["arguments"].(map[string]any)
	srv := args["server"].(map[string]any)
	if _, nested := srv["data"]; nested {
		t.Errorf("blockdev-add server must be FLAT SocketAddress, got nested data: %v", srv)
	}
	if srv["host"] != "10.0.0.2" || srv["port"] != "49153" {
		t.Errorf("server = %v, want flat host/port string", srv)
	}
	if args["tls-hostname"] != "node-b.agents.otherix.local" {
		t.Errorf("tls-hostname = %v", args["tls-hostname"])
	}
}

func TestBlockJobCancelCmd_ForceFlag(t *testing.T) {
	final := blockJobCancelCmd("mirror-disk0", false)
	abort := blockJobCancelCmd("mirror-disk0", true)
	var f, a map[string]any
	_ = json.Unmarshal(final, &f)
	_ = json.Unmarshal(abort, &a)
	if f["arguments"].(map[string]any)["force"] != false {
		t.Error("finalize must be force=false")
	}
	if a["arguments"].(map[string]any)["force"] != true {
		t.Error("abort must be force=true")
	}
}

func TestQueryBlockJobsCmd_NoArguments(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(queryBlockJobsCmd(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["execute"] != "query-block-jobs" {
		t.Errorf("execute = %v, want query-block-jobs", m["execute"])
	}
	if _, ok := m["arguments"]; ok {
		t.Errorf("query-block-jobs must omit arguments, got %v", m["arguments"])
	}
}

func TestMigrateCancelCmd_Underscore(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal(migrateCancelCmd(), &m)
	if m["execute"] != "migrate_cancel" { // underscore, not hyphen
		t.Errorf("execute = %v, want migrate_cancel", m["execute"])
	}
}

func TestAnnounceSelfCmd_Schedule(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(announceSelfCmd(AnnounceParameters{Initial: 50, Max: 550, Rounds: 5, Step: 100}), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["execute"] != "announce-self" {
		t.Errorf("execute = %v, want announce-self", m["execute"])
	}
	args := m["arguments"].(map[string]any)
	// JSON numbers decode to float64.
	want := map[string]float64{"initial": 50, "max": 550, "rounds": 5, "step": 100}
	for k, v := range want {
		if got := args[k].(float64); got != v {
			t.Errorf("arguments.%s = %v, want %v", k, got, v)
		}
	}
}

func TestWaitMigrationStatus_ReturnsOnMatch(t *testing.T) {
	ch := make(chan qmp.Event, 3)
	ch <- qmp.Event{Event: "MIGRATION", Data: map[string]any{"status": "active"}}
	ch <- qmp.Event{Event: "MIGRATION", Data: map[string]any{"status": "pre-switchover"}}
	close(ch)
	got, err := waitMigrationStatus(context.Background(), ch, "pre-switchover", []string{"failed", "cancelled"})
	if err != nil {
		t.Fatalf("waitMigrationStatus error = %v", err)
	}
	if got != "pre-switchover" {
		t.Errorf("status = %q, want pre-switchover", got)
	}
}

func TestWaitMigrationStatus_FailsOnTerminalFailure(t *testing.T) {
	ch := make(chan qmp.Event, 1)
	ch <- qmp.Event{Event: "MIGRATION", Data: map[string]any{"status": "failed"}}
	close(ch)
	if _, err := waitMigrationStatus(context.Background(), ch, "pre-switchover", []string{"failed", "cancelled"}); err == nil {
		t.Fatal("waitMigrationStatus on failed = nil error, want failure")
	}
}

func TestWaitMigrationStatus_ErrorsOnClosedChannel(t *testing.T) {
	ch := make(chan qmp.Event)
	close(ch)
	if _, err := waitMigrationStatus(context.Background(), ch, "pre-switchover", []string{"failed"}); err == nil {
		t.Fatal("waitMigrationStatus on closed channel = nil error, want failure")
	}
}

func TestWaitBlockJobEvent_ReadyForDevice(t *testing.T) {
	ch := make(chan qmp.Event, 2)
	ch <- qmp.Event{Event: "BLOCK_JOB_READY", Data: map[string]any{"device": "other"}} // ignored: wrong device
	ch <- qmp.Event{Event: "BLOCK_JOB_READY", Data: map[string]any{"device": "mirror-disk0"}}
	close(ch)
	if err := waitBlockJobEvent(context.Background(), ch, "BLOCK_JOB_READY", "mirror-disk0"); err != nil {
		t.Errorf("waitBlockJobEvent READY error = %v", err)
	}
}

func TestWaitBlockJobEvent_SurfacesJobError(t *testing.T) {
	ch := make(chan qmp.Event, 1)
	ch <- qmp.Event{Event: "BLOCK_JOB_COMPLETED", Data: map[string]any{"device": "mirror-disk0", "error": "No space left on device"}}
	close(ch)
	if err := waitBlockJobEvent(context.Background(), ch, "BLOCK_JOB_COMPLETED", "mirror-disk0"); err == nil {
		t.Fatal("waitBlockJobEvent with job error = nil, want error surfaced")
	}
}

func TestWaitBlockJobsEvent_ReadyForAll(t *testing.T) {
	ch := make(chan qmp.Event, 3)
	ch <- qmp.Event{Event: "BLOCK_JOB_READY", Data: map[string]any{"device": "mirror-cidata"}}
	ch <- qmp.Event{Event: "BLOCK_JOB_READY", Data: map[string]any{"device": "mirror-disk0"}}
	close(ch)
	err := waitBlockJobsEvent(context.Background(), ch, "BLOCK_JOB_READY",
		map[string]bool{"mirror-cidata": true, "mirror-disk0": true})
	if err != nil {
		t.Errorf("waitBlockJobsEvent READY for all error = %v", err)
	}
}

func TestWaitBlockJobsEvent_CompletedForAll(t *testing.T) {
	ch := make(chan qmp.Event, 3)
	ch <- qmp.Event{Event: "BLOCK_JOB_COMPLETED", Data: map[string]any{"device": "mirror-cidata"}}
	ch <- qmp.Event{Event: "BLOCK_JOB_COMPLETED", Data: map[string]any{"device": "mirror-disk0"}}
	close(ch)
	err := waitBlockJobsEvent(context.Background(), ch, "BLOCK_JOB_COMPLETED",
		map[string]bool{"mirror-cidata": true, "mirror-disk0": true})
	if err != nil {
		t.Errorf("waitBlockJobsEvent COMPLETED for all error = %v", err)
	}
}

func TestWaitBlockJobsEvent_FailsFastOnBlockJobError(t *testing.T) {
	ch := make(chan qmp.Event, 2)
	ch <- qmp.Event{Event: "BLOCK_JOB_ERROR", Data: map[string]any{"device": "mirror-disk0", "error": "No space left on device"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := waitBlockJobsEvent(ctx, ch, "BLOCK_JOB_READY", map[string]bool{"mirror-disk0": true})
	if err == nil {
		t.Fatalf("want error on BLOCK_JOB_ERROR, got nil")
	}
	if ctx.Err() != nil {
		t.Errorf("returned via context timeout, not the job error: %v", ctx.Err())
	}
	if !strings.Contains(err.Error(), "mirror-disk0") {
		t.Errorf("error should name the device: %v", err)
	}
}

func TestWaitBlockJobsEvent_FailsOnErroredCompleted(t *testing.T) {
	ch := make(chan qmp.Event, 2)
	ch <- qmp.Event{Event: "BLOCK_JOB_COMPLETED", Data: map[string]any{"device": "mirror-disk0", "error": "Input/output error"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitBlockJobsEvent(ctx, ch, "BLOCK_JOB_READY", map[string]bool{"mirror-disk0": true}); err == nil {
		t.Fatalf("want error on errored BLOCK_JOB_COMPLETED while waiting for READY, got nil")
	}
}
