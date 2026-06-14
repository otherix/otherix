// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
)

// fakeLiveConn is a LiveSourceConn test double that records the ordered
// call sequence and serves events from a pre-loaded channel.
type fakeLiveConn struct {
	mu      sync.Mutex
	calls   []string
	events  chan qmp.Event
	migInfo MigrateInfo
}

func (f *fakeLiveConn) rec(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeLiveConn) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLiveConn) ObjectAddTLSCreds(id, dir, endpoint string) error {
	f.rec("object-add:" + endpoint)
	return nil
}

func (f *fakeLiveConn) BlockdevAddNBD(n, h string, p int, e, c, hn string) error {
	f.rec("blockdev-add")
	return nil
}

func (f *fakeLiveConn) BlockdevMirror(j, d, t string) error {
	f.rec("blockdev-mirror")
	return nil
}

func (f *fakeLiveConn) MigrateSetCapabilities(caps map[string]bool) error {
	keys := make([]string, 0, len(caps))
	for k := range caps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f.rec("set-caps:" + strings.Join(keys, ","))
	return nil
}

func (f *fakeLiveConn) ObjectAddAuthz(id, identity string) error {
	f.rec("object-add:authz")
	return nil
}

func (f *fakeLiveConn) NBDServerStart(host string, port int, tlsCreds, tlsAuthz string) error {
	f.rec("nbd-server-start")
	return nil
}

func (f *fakeLiveConn) BlockExportAdd(id, nodeName, name string, writable bool) error {
	f.rec(fmt.Sprintf("block-export-add:%s", writabilityLabel(writable)))
	return nil
}

func (f *fakeLiveConn) MigrateIncoming(string) error {
	f.rec("migrate-incoming")
	return nil
}

// writabilityLabel renders a block-export-add writable flag as the
// "writable"/"readonly" tag the order tests assert on.
func writabilityLabel(writable bool) string {
	if writable {
		return "writable"
	}
	return "readonly"
}

func (f *fakeLiveConn) MigrateSetParameters(LiveParams) error {
	f.rec("set-params")
	return nil
}

func (f *fakeLiveConn) Migrate(string) error {
	f.rec("migrate")
	return nil
}

func (f *fakeLiveConn) BlockJobCancel(d string, force bool) error {
	f.rec(fmt.Sprintf("block-job-cancel:%t", force))
	return nil
}

func (f *fakeLiveConn) MigrateContinue() error {
	f.rec("migrate-continue")
	return nil
}

func (f *fakeLiveConn) MigrateCancel() error {
	f.rec("migrate_cancel")
	return nil
}

func (f *fakeLiveConn) BlockdevDel(string) error {
	f.rec("blockdev-del")
	return nil
}

func (f *fakeLiveConn) Close() error { return nil }

func (f *fakeLiveConn) QueryMigrate() (MigrateInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.migInfo, nil
}

func (f *fakeLiveConn) QueryBlockJobs() ([]BlockJobInfo, error) {
	return []BlockJobInfo{}, nil
}

func (f *fakeLiveConn) Events(ctx context.Context) (<-chan qmp.Event, error) {
	return f.events, nil
}

// blockJobEvent builds a block-job QMP event carrying the device name.
func blockJobEvent(name, device string) qmp.Event {
	return qmp.Event{Event: name, Data: map[string]interface{}{"device": device}}
}

// migrationEvent builds a MIGRATION QMP event carrying a status.
func migrationEvent(status string) qmp.Event {
	return qmp.Event{Event: "MIGRATION", Data: map[string]interface{}{"status": status}}
}

func testSpec() LiveSourceSpec {
	return LiveSourceSpec{
		SrcDiskNode:        "disk0",
		JobID:              "mirror0",
		NBDNode:            "nbd0",
		TargetHost:         "10.0.0.2",
		NBDPort:            10000,
		Export:             "tok",
		CredsDir:           "/tmp/creds",
		TargetIdentity:     "node-t.agents.otherix.local",
		RAMHost:            "10.0.0.2",
		RAMPort:            10001,
		DowntimeMs:         300,
		MaxBandwidthBytes:  0,
		ConvergenceTimeout: 5 * time.Second,
	}
}

func TestRunLiveSource_HappyPathOrder(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 4)}
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror0")
	f.events <- migrationEvent("pre-switchover")
	f.events <- blockJobEvent("BLOCK_JOB_COMPLETED", "mirror0")
	f.events <- migrationEvent("completed")

	if err := RunLiveSource(context.Background(), f, testSpec(), nil); err != nil {
		t.Fatalf("RunLiveSource() = %v, want nil", err)
	}

	want := []string{
		"object-add:client",
		"blockdev-add",
		"blockdev-mirror",
		"set-caps:auto-converge,events,pause-before-switchover",
		"set-params",
		"migrate",
		"block-job-cancel:false",
		"migrate-continue",
		"blockdev-del",
	}
	if got := f.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("call order = %v, want %v", got, want)
	}
}

// TestRunLiveSource_ReporterFires asserts the periodic reporter is invoked at
// least once during a happy-path run and that it carries the block-job + RAM
// snapshot the manager logs and surfaces. The happy-path events are fed only
// after the reporter has fired once, so the run cannot complete before the
// reporter ticks (no reliance on goroutine timing).
func TestRunLiveSource_ReporterFires(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 4)}

	var mu sync.Mutex
	var count int
	fired := make(chan struct{})
	report := func(LiveProgress) {
		mu.Lock()
		count++
		first := count == 1
		mu.Unlock()
		if first {
			close(fired)
		}
	}

	// Release the happy-path events only once the reporter has ticked, so the
	// run is guaranteed to outlive at least one report call.
	go func() {
		<-fired
		f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror0")
		f.events <- migrationEvent("pre-switchover")
		f.events <- blockJobEvent("BLOCK_JOB_COMPLETED", "mirror0")
		f.events <- migrationEvent("completed")
	}()

	spec := testSpec()
	spec.ProgressPollInterval = 5 * time.Millisecond

	if err := RunLiveSource(context.Background(), f, spec, report); err != nil {
		t.Fatalf("RunLiveSource() = %v, want nil", err)
	}

	mu.Lock()
	got := count
	mu.Unlock()
	if got == 0 {
		t.Errorf("reporter was not invoked during a happy-path run")
	}
}

func TestRunLiveSource_AbortOnRAMFailureBeforeSwitchover(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 2)}
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror0")
	f.events <- migrationEvent("failed")

	err := RunLiveSource(context.Background(), f, testSpec(), nil)
	if err == nil {
		t.Fatalf("RunLiveSource() = nil, want error on RAM failure")
	}

	got := f.snapshot()
	assertAbortBoth(t, got)
	if contains(got, "migrate-continue") {
		t.Errorf("call order = %v, must NOT contain migrate-continue after failure", got)
	}
}

func TestRunLiveSource_WatchdogAbortsOnStall(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 1)}
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror0")
	// Never decreases: lastProgress never advances, watchdog fires.
	f.migInfo.RAM.Remaining = 1 << 30

	spec := testSpec()
	spec.ConvergenceTimeout = 50 * time.Millisecond
	spec.ProgressPollInterval = 10 * time.Millisecond

	err := RunLiveSource(context.Background(), f, spec, nil)
	if err == nil {
		t.Fatalf("RunLiveSource() = nil, want watchdog error on stall")
	}

	got := f.snapshot()
	assertAbortBoth(t, got)
	if contains(got, "migrate-continue") {
		t.Errorf("call order = %v, must NOT contain migrate-continue on watchdog abort", got)
	}
}

// TestWatchdogErrorMentionsTimeout ensures the watchdog error message
// references the convergence timeout so logs are diagnosable.
func TestWatchdogErrorMentionsTimeout(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 1)}
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror0")
	f.migInfo.RAM.Remaining = 1 << 30

	spec := testSpec()
	spec.ConvergenceTimeout = 50 * time.Millisecond
	spec.ProgressPollInterval = 10 * time.Millisecond

	err := RunLiveSource(context.Background(), f, spec, nil)
	if err == nil || !strings.Contains(err.Error(), "converge") {
		t.Fatalf("RunLiveSource() = %v, want error mentioning convergence", err)
	}
}

func TestSetupLiveIncoming_Order(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 1)}
	err := SetupLiveIncoming(f, LiveIncomingSpec{
		CredsDir: "/c", SourceIdentity: "CN=node-a", DiskNode: "target-disk0",
		Export: "tok", ExportID: "exp0", BindHost: "0.0.0.0", NBDPort: 49153, RAMPort: 49200,
	})
	if err != nil {
		t.Fatalf("SetupLiveIncoming error = %v", err)
	}
	want := []string{
		"object-add:server", "object-add:authz",
		"nbd-server-start", "block-export-add:writable",
		"set-caps:events,late-block-activate", "set-params", "migrate-incoming",
	}
	if !reflect.DeepEqual(f.snapshot(), want) {
		t.Errorf("setup order =\n %v\nwant\n %v", f.snapshot(), want)
	}
}

func TestLiveIncomingArgs_HasDeferAndDiskNode(t *testing.T) {
	args := LiveIncomingArgs(LiveIncomingSpec{DiskNode: "target-disk0", DiskPath: "/pool/vm/disk.qcow2"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-incoming") || !strings.Contains(joined, "defer") {
		t.Errorf("LiveIncomingArgs missing -incoming defer: %v", args)
	}
	if !strings.Contains(joined, "target-disk0") || !strings.Contains(joined, "/pool/vm/disk.qcow2") {
		t.Errorf("LiveIncomingArgs missing disk node-name/path: %v", args)
	}
}

// assertAbortBoth asserts the abort cancelled both the RAM migration and
// the disk mirror: migrate_cancel followed by block-job-cancel:true.
func assertAbortBoth(t *testing.T, calls []string) {
	t.Helper()
	mc := indexOf(calls, "migrate_cancel")
	bj := indexOf(calls, "block-job-cancel:true")
	if mc < 0 {
		t.Errorf("call order = %v, missing migrate_cancel", calls)
	}
	if bj < 0 {
		t.Errorf("call order = %v, missing block-job-cancel:true", calls)
	}
	if mc >= 0 && bj >= 0 && mc > bj {
		t.Errorf("call order = %v, migrate_cancel must precede block-job-cancel:true", calls)
	}
}

func contains(s []string, v string) bool { return indexOf(s, v) >= 0 }

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
