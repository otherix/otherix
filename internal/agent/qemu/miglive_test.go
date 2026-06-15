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
	"github.com/google/go-cmp/cmp"
)

// fakeLiveConn is a LiveSourceConn test double that records the ordered
// call sequence and serves events from a pre-loaded channel.
type fakeLiveConn struct {
	mu         sync.Mutex
	calls      []string
	events     chan qmp.Event
	migInfo    MigrateInfo
	exportAdds []exportAddCall
}

// exportAddCall records one BlockExportAdd invocation so multi-disk tests can
// assert the per-disk export id / node / writability in order.
type exportAddCall struct {
	id       string
	node     string
	name     string
	writable bool
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
	f.mu.Lock()
	f.exportAdds = append(f.exportAdds, exportAddCall{id: id, node: nodeName, name: name, writable: writable})
	f.mu.Unlock()
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
		Disks: []LiveSourceDisk{
			{SrcNode: "disk0", JobID: "mirror0", NBDNode: "nbd0", Export: "tok"},
		},
		TargetHost:         "10.0.0.2",
		NBDPort:            10000,
		CredsDir:           "/tmp/creds",
		TargetIdentity:     "node-t.agents.otherix.local",
		RAMHost:            "10.0.0.2",
		RAMPort:            10001,
		DowntimeMs:         300,
		MaxBandwidthBytes:  0,
		ConvergenceTimeout: 5 * time.Second,
	}
}

// twoDiskSpec is a 2-disk source spec (boot virtio0 + cidata virtio1) whose
// per-disk job ids and export names match the target convention
// (mirror-disk<i> / <token>-<i>).
func twoDiskSpec() LiveSourceSpec {
	s := testSpec()
	s.Disks = []LiveSourceDisk{
		{SrcNode: "virtio0", JobID: "mirror-disk0", NBDNode: "mirror-target0", Export: "tok-0"},
		{SrcNode: "virtio1", JobID: "mirror-disk1", NBDNode: "mirror-target1", Export: "tok-1"},
	}
	return s
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

// TestRunLiveSource_MultiDiskMirrorsBothOutOfOrderReady drives a 2-disk live
// migration. It asserts RunLiveSource adds an NBD blockdev + starts a mirror for
// BOTH disks, waits READY for BOTH even though the cidata (mirror-disk1) READY
// arrives BEFORE the boot (mirror-disk0) READY, then at switchover issues
// block-job-cancel(force=false) for BOTH and waits COMPLETED for BOTH before
// migrate-continue, and finally dels BOTH client blockdevs. The out-of-order
// READY is the deterministic seam the single-job waiter would mishandle.
func TestRunLiveSource_MultiDiskMirrorsBothOutOfOrderReady(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 8)}
	// cidata READY BEFORE boot READY (out of order on purpose).
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk1")
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk0")
	f.events <- migrationEvent("pre-switchover")
	// COMPLETED also out of order (cidata first).
	f.events <- blockJobEvent("BLOCK_JOB_COMPLETED", "mirror-disk1")
	f.events <- blockJobEvent("BLOCK_JOB_COMPLETED", "mirror-disk0")
	f.events <- migrationEvent("completed")

	if err := RunLiveSource(context.Background(), f, twoDiskSpec(), nil); err != nil {
		t.Fatalf("RunLiveSource() = %v, want nil", err)
	}

	got := f.snapshot()
	// One TLS creds, then per-disk blockdev-add + blockdev-mirror (2 each).
	if n := countCall(got, "blockdev-add"); n != 2 {
		t.Errorf("blockdev-add count = %d, want 2 (one per disk)", n)
	}
	if n := countCall(got, "blockdev-mirror"); n != 2 {
		t.Errorf("blockdev-mirror count = %d, want 2 (one per disk)", n)
	}
	if n := countCall(got, "object-add:client"); n != 1 {
		t.Errorf("object-add:client count = %d, want 1 (single TLS creds)", n)
	}
	// Finalize issues block-job-cancel(force=false) for BOTH disks.
	if n := countCall(got, "block-job-cancel:false"); n != 2 {
		t.Errorf("block-job-cancel:false count = %d, want 2 (finalize both mirrors)", n)
	}
	// Both client blockdevs dropped on completion.
	if n := countCall(got, "blockdev-del"); n != 2 {
		t.Errorf("blockdev-del count = %d, want 2 (one per disk)", n)
	}
	// Both mirrors started before any RAM migrate; both finalized before continue.
	migrateIdx := indexOf(got, "migrate")
	if c := lastIndexOf(got, "blockdev-mirror"); c >= 0 && migrateIdx >= 0 && c > migrateIdx {
		t.Errorf("call order = %v, both mirrors must start before RAM migrate", got)
	}
	contIdx := indexOf(got, "migrate-continue")
	if c := lastIndexOf(got, "block-job-cancel:false"); c >= 0 && contIdx >= 0 && c > contIdx {
		t.Errorf("call order = %v, both mirrors must be finalized before migrate-continue", got)
	}
}

// TestRunLiveSource_MultiDiskAbortCancelsAllDisks asserts a RAM failure aborts
// BOTH disk mirrors (force cancel + del each) and still cancels the RAM once.
func TestRunLiveSource_MultiDiskAbortCancelsAllDisks(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 4)}
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk1")
	f.events <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk0")
	f.events <- migrationEvent("failed")

	if err := RunLiveSource(context.Background(), f, twoDiskSpec(), nil); err == nil {
		t.Fatal("RunLiveSource() = nil, want error on RAM failure")
	}

	got := f.snapshot()
	if n := countCall(got, "migrate_cancel"); n != 1 {
		t.Errorf("migrate_cancel count = %d, want 1 (RAM cancelled once)", n)
	}
	if n := countCall(got, "block-job-cancel:true"); n != 2 {
		t.Errorf("block-job-cancel:true count = %d, want 2 (force-abort both mirrors)", n)
	}
	if contains(got, "migrate-continue") {
		t.Errorf("call order = %v, must NOT contain migrate-continue after failure", got)
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
		CredsDir: "/c", SourceIdentity: "CN=node-a", BindHost: "0.0.0.0", NBDPort: 49153, RAMPort: 49200,
		Disks: []LiveIncomingDisk{
			{Node: "target-disk0", Path: "/pool/vm/disk.qcow2", Export: "tok-0", ExportID: "exp0", Format: "qcow2"},
		},
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

// TestSetupLiveIncoming_MultiDiskExportsAllWritableInOrder asserts a 2-disk
// spec yields exactly ONE nbd-server-start and one block-export-add per disk,
// both writable (they must receive the mirror) and in index order, before the
// caps/params/migrate-incoming arming.
func TestSetupLiveIncoming_MultiDiskExportsAllWritableInOrder(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 1)}
	err := SetupLiveIncoming(f, LiveIncomingSpec{
		CredsDir: "/c", SourceIdentity: "CN=node-a", BindHost: "0.0.0.0", NBDPort: 49153, RAMPort: 49200,
		Disks: []LiveIncomingDisk{
			{Node: "target-disk0", Path: "/pool/vm/disk.qcow2", Export: "mig-0", ExportID: "exp0", Format: "qcow2"},
			{Node: "target-disk1", Path: "/pool/vm/cidata.iso", Export: "mig-1", ExportID: "exp1", Format: "raw", ReadOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("SetupLiveIncoming error = %v", err)
	}
	want := []string{
		"object-add:server", "object-add:authz", "nbd-server-start",
		"block-export-add:writable", "block-export-add:writable",
		"set-caps:events,late-block-activate", "set-params", "migrate-incoming",
	}
	if got := f.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("setup order =\n %v\nwant\n %v", got, want)
	}
	if n := strings.Count(strings.Join(f.snapshot(), " "), "nbd-server-start"); n != 1 {
		t.Errorf("nbd-server-start count = %d, want 1", n)
	}
	wantExports := []exportAddCall{
		{id: "exp0", node: "target-disk0", name: "mig-0", writable: true},
		{id: "exp1", node: "target-disk1", name: "mig-1", writable: true},
	}
	if diff := cmp.Diff(wantExports, f.exportAdds, cmp.AllowUnexported(exportAddCall{})); diff != "" {
		t.Errorf("export-add calls mismatch (-want +got):\n%s", diff)
	}
}

func TestLiveIncomingArgs_HasDeferAndDiskNode(t *testing.T) {
	args := LiveIncomingArgs(LiveIncomingSpec{
		Disks: []LiveIncomingDisk{{Node: "target-disk0", Path: "/pool/vm/disk.qcow2", Format: "qcow2"}},
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-incoming") || !strings.Contains(joined, "defer") {
		t.Errorf("LiveIncomingArgs missing -incoming defer: %v", args)
	}
	if !strings.Contains(joined, "target-disk0") || !strings.Contains(joined, "/pool/vm/disk.qcow2") {
		t.Errorf("LiveIncomingArgs missing disk node-name/path: %v", args)
	}
}

// TestLiveIncomingArgs_SingleDiskEmitsOneBlockdevDeviceDefer asserts the
// boot-only case still produces exactly one -blockdev, one -device (no
// read-only), and one -incoming defer after the hardcoded device was removed
// from launchIncomingQemu.
func TestLiveIncomingArgs_SingleDiskEmitsOneBlockdevDeviceDefer(t *testing.T) {
	args := LiveIncomingArgs(LiveIncomingSpec{
		Disks: []LiveIncomingDisk{{Node: "target-disk0", Path: "/pool/vm/disk.qcow2", Format: "qcow2"}},
	})
	joined := strings.Join(args, " ")
	if got := countArg(args, "-blockdev"); got != 1 {
		t.Errorf("-blockdev count = %d, want 1 (%v)", got, args)
	}
	if got := countArg(args, "-device"); got != 1 {
		t.Errorf("-device count = %d, want 1 (%v)", got, args)
	}
	if got := countArg(args, "-incoming"); got != 1 {
		t.Errorf("-incoming count = %d, want 1 (%v)", got, args)
	}
	if !strings.Contains(joined, "virtio-blk-pci,drive=target-disk0") {
		t.Errorf("missing boot -device virtio-blk-pci,drive=target-disk0: %v", args)
	}
	if strings.Contains(joined, "read-only=on") {
		t.Errorf("boot device must not be read-only: %v", args)
	}
}

// TestLiveIncomingArgs_MultiDiskEmitsPerDiskBlockdevAndDevice asserts a 2-disk
// spec emits per-disk -blockdev (correct driver) + -device (read-only=on only
// for the cidata device) and exactly one -incoming defer.
func TestLiveIncomingArgs_MultiDiskEmitsPerDiskBlockdevAndDevice(t *testing.T) {
	args := LiveIncomingArgs(LiveIncomingSpec{
		Disks: []LiveIncomingDisk{
			{Node: "target-disk0", Path: "/pool/vm/disk.qcow2", Format: "qcow2"},
			{Node: "target-disk1", Path: "/pool/vm/cidata.iso", Format: "raw", ReadOnly: true},
		},
	})
	joined := strings.Join(args, " ")
	if got := countArg(args, "-blockdev"); got != 2 {
		t.Errorf("-blockdev count = %d, want 2 (%v)", got, args)
	}
	if got := countArg(args, "-device"); got != 2 {
		t.Errorf("-device count = %d, want 2 (%v)", got, args)
	}
	if got := countArg(args, "-incoming"); got != 1 {
		t.Errorf("-incoming count = %d, want 1 (%v)", got, args)
	}
	if !strings.Contains(joined, "driver=qcow2,node-name=target-disk0") {
		t.Errorf("missing boot blockdev driver=qcow2,node-name=target-disk0: %v", args)
	}
	if !strings.Contains(joined, "driver=raw,node-name=target-disk1") {
		t.Errorf("missing cidata blockdev driver=raw,node-name=target-disk1: %v", args)
	}
	if !strings.Contains(joined, "virtio-blk-pci,drive=target-disk0") || strings.Contains(joined, "virtio-blk-pci,drive=target-disk0,read-only=on") {
		t.Errorf("boot device wrong (want no read-only): %v", args)
	}
	if !strings.Contains(joined, "virtio-blk-pci,drive=target-disk1,read-only=on") {
		t.Errorf("cidata device must be read-only=on: %v", args)
	}
}

// countArg counts occurrences of a flag token in an arg slice.
func countArg(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
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

// coupledLiveConn models go-qemu's real concurrency contract that the buffered
// fakeLiveConn does NOT: a single producer goroutine demultiplexes one socket
// into BOTH the command-response stream and the async-event channel, and the
// event channel is unbuffered. Concretely, a QMP command that starts a job
// (blockdev-mirror, migrate, block-job-cancel, migrate-continue) makes QEMU emit
// the job's status event, and that event can reach the wire before the command's
// own reply; go-qemu's listen goroutine blocks delivering the event until someone
// receives it, which also blocks delivery of the reply. So here the event-emitting
// methods do a BLOCKING send of their triggering event before returning - i.e. the
// method cannot "return its reply" until the event is drained. If RunLiveSource
// does not continuously drain the event channel from the moment it subscribes, the
// very first such command deadlocks. This is the regression the buffered fake can
// never catch.
type coupledLiveConn struct {
	mu     sync.Mutex
	calls  []string
	events chan qmp.Event // UNBUFFERED on purpose: one producer, no slack
}

func (c *coupledLiveConn) rec(s string) {
	c.mu.Lock()
	c.calls = append(c.calls, s)
	c.mu.Unlock()
}

func (c *coupledLiveConn) ObjectAddTLSCreds(id, dir, endpoint string) error {
	c.rec("object-add:" + endpoint)
	return nil
}
func (c *coupledLiveConn) ObjectAddAuthz(string, string) error { c.rec("object-add:authz"); return nil }
func (c *coupledLiveConn) BlockdevAddNBD(n, h string, p int, e, cr, hn string) error {
	c.rec("blockdev-add")
	return nil
}

func (c *coupledLiveConn) BlockdevMirror(jobID, device, target string) error {
	c.rec("blockdev-mirror")
	// The mirror job starts and emits its event before the reply lands.
	c.events <- blockJobEvent("BLOCK_JOB_READY", jobID)
	return nil
}

func (c *coupledLiveConn) MigrateSetCapabilities(map[string]bool) error {
	c.rec("set-caps")
	return nil
}

func (c *coupledLiveConn) NBDServerStart(string, int, string, string) error {
	c.rec("nbd-server-start")
	return nil
}

func (c *coupledLiveConn) BlockExportAdd(string, string, string, bool) error {
	c.rec("block-export-add")
	return nil
}
func (c *coupledLiveConn) MigrateIncoming(string) error          { c.rec("migrate-incoming"); return nil }
func (c *coupledLiveConn) MigrateSetParameters(LiveParams) error { c.rec("set-params"); return nil }

func (c *coupledLiveConn) Migrate(string) error {
	c.rec("migrate")
	c.events <- migrationEvent("pre-switchover")
	return nil
}

func (c *coupledLiveConn) BlockJobCancel(device string, force bool) error {
	c.rec(fmt.Sprintf("block-job-cancel:%t", force))
	if !force {
		c.events <- blockJobEvent("BLOCK_JOB_COMPLETED", "mirror0")
	}
	return nil
}

func (c *coupledLiveConn) MigrateContinue() error {
	c.rec("migrate-continue")
	c.events <- migrationEvent("completed")
	return nil
}

func (c *coupledLiveConn) MigrateCancel() error                    { c.rec("migrate_cancel"); return nil }
func (c *coupledLiveConn) BlockdevDel(string) error                { c.rec("blockdev-del"); return nil }
func (c *coupledLiveConn) Close() error                            { return nil }
func (c *coupledLiveConn) QueryMigrate() (MigrateInfo, error)      { return MigrateInfo{}, nil }
func (c *coupledLiveConn) QueryBlockJobs() ([]BlockJobInfo, error) { return nil, nil }
func (c *coupledLiveConn) Events(context.Context) (<-chan qmp.Event, error) {
	return c.events, nil
}

// TestRunLiveSource_EventEmittingCommandDoesNotDeadlock drives the source
// against a connection whose event-emitting commands block until their event is
// drained (go-qemu's real single-producer contract). RunLiveSource must keep the
// event channel drained from the moment it subscribes, so blockdev-mirror (and
// every later event-emitting command) returns and the run converges. Without a
// continuous drainer the run wedges forever in blockdev-mirror.
func TestRunLiveSource_EventEmittingCommandDoesNotDeadlock(t *testing.T) {
	c := &coupledLiveConn{events: make(chan qmp.Event)}

	done := make(chan error, 1)
	go func() { done <- RunLiveSource(context.Background(), c, testSpec(), nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLiveSource() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunLiveSource deadlocked: an event-emitting QMP command blocked because the event channel was not drained from subscription")
	}
}

// TestRunLiveSource_DiskMirrorStallFailsClosed asserts that a disk mirror that
// never reaches BLOCK_JOB_READY (a genuinely stuck QEMU, not a wrapper deadlock)
// fails closed within DiskMirrorTimeout instead of blocking forever. The buffered
// fake returns from blockdev-mirror immediately but feeds no BLOCK_JOB_READY, so
// only the per-phase timeout can release the wait. A live migration that cannot
// converge must abort and leave the guest on the source, not hang.
func TestRunLiveSource_DiskMirrorStallFailsClosed(t *testing.T) {
	f := &fakeLiveConn{events: make(chan qmp.Event, 4)} // no BLOCK_JOB_READY ever fed
	spec := testSpec()
	spec.DiskMirrorTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- RunLiveSource(context.Background(), f, spec, nil) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunLiveSource() = nil, want error on a stalled disk mirror")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLiveSource did not fail closed on a stalled disk mirror within the timeout")
	}
}

// fakeTargetConn is a LiveTargetConn test double that records the ordered
// call sequence and serves events from a pre-loaded channel.
type fakeTargetConn struct {
	mu     sync.Mutex
	calls  []string
	events chan qmp.Event
}

func (f *fakeTargetConn) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeTargetConn) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeTargetConn) Events(context.Context) (<-chan qmp.Event, error) { return f.events, nil }
func (f *fakeTargetConn) BlockExportDel(id string) error                   { f.rec("block-export-del"); return nil }
func (f *fakeTargetConn) NBDServerStop() error                             { f.rec("nbd-server-stop"); return nil }
func (f *fakeTargetConn) Cont() error                                      { f.rec("cont"); return nil }
func (f *fakeTargetConn) Close() error                                     { return nil }

func TestRunLiveTarget_HappyPathOrder(t *testing.T) {
	f := &fakeTargetConn{events: make(chan qmp.Event, 1)}
	f.events <- migrationEvent("completed")

	spec := LiveTargetSpec{ExportID: "exp0", IncomingTimeout: 2 * time.Second}
	if err := RunLiveTarget(context.Background(), f, spec); err != nil {
		t.Fatalf("RunLiveTarget() = %v, want nil", err)
	}
	want := []string{"block-export-del", "nbd-server-stop", "cont"}
	if got := f.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("call order = %v, want %v", got, want)
	}
}

func TestRunLiveTarget_FailClosedOnIncomingFailed(t *testing.T) {
	f := &fakeTargetConn{events: make(chan qmp.Event, 1)}
	f.events <- migrationEvent("failed")
	if err := RunLiveTarget(context.Background(), f, LiveTargetSpec{ExportID: "exp0", IncomingTimeout: time.Second}); err == nil {
		t.Fatal("RunLiveTarget() = nil, want error on incoming failed")
	}
}

func TestRunLiveTarget_FailClosedOnTimeout(t *testing.T) {
	f := &fakeTargetConn{events: make(chan qmp.Event, 1)} // no event ever
	if err := RunLiveTarget(context.Background(), f, LiveTargetSpec{ExportID: "exp0", IncomingTimeout: 100 * time.Millisecond}); err == nil {
		t.Fatal("RunLiveTarget() = nil, want error on incoming timeout")
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

// lastIndexOf returns the index of the last occurrence of v in s, or -1.
func lastIndexOf(s []string, v string) int {
	idx := -1
	for i, x := range s {
		if x == v {
			idx = i
		}
	}
	return idx
}

// countCall counts occurrences of v in the recorded call slice.
func countCall(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// TestWaitBlockJobsTimeout_OutOfOrderReady is the seam test: the tiny cidata
// mirror (mirror-disk1) reaches BLOCK_JOB_READY BEFORE the multi-GiB boot disk
// (mirror-disk0). A naive sequential single-job waiter would DISCARD the cidata
// READY while waiting on the boot job, then block on cidata forever. The
// set-based waiter must consume both in a single pass regardless of order.
func TestWaitBlockJobsTimeout_OutOfOrderReady(t *testing.T) {
	// Unbuffered channel + goroutine so the test controls event ordering and
	// can observe whether the waiter has returned. Feed cidata (mirror-disk1)
	// READY FIRST, assert the waiter is STILL blocked (it must wait for the
	// whole set, not return after the first match), then feed boot
	// (mirror-disk0) READY and assert it returns nil.
	ch := make(chan qmp.Event)
	done := make(chan error, 1)
	go func() {
		done <- waitBlockJobsTimeout(context.Background(), ch, "BLOCK_JOB_READY",
			[]string{"mirror-disk0", "mirror-disk1"}, time.Second)
	}()

	// First match: cidata READY. A correct waiter consumes it and keeps waiting.
	ch <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk1")

	// The waiter must NOT have returned yet: only one of two jobs is READY.
	// A "return after the first matching event" mutation returns here and fails.
	select {
	case err := <-done:
		t.Fatalf("waitBlockJobsTimeout() returned %v after only mirror-disk1 READY; want it still blocked for mirror-disk0", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Second match: boot READY. Now the set is satisfied and the waiter returns.
	ch <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk0")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitBlockJobsTimeout() = %v, want nil (both jobs reached READY out of order)", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitBlockJobsTimeout() did not return after both jobs reached READY")
	}
}

// TestWaitBlockJobsTimeout_TeethOneJobNeverReady proves the waiter fails closed:
// if one job in the set never emits, the deadline fires and the waiter returns
// an error rather than blocking forever.
func TestWaitBlockJobsTimeout_TeethOneJobNeverReady(t *testing.T) {
	ch := make(chan qmp.Event, 1)
	ch <- blockJobEvent("BLOCK_JOB_READY", "mirror-disk1") // boot job never emits

	err := waitBlockJobsTimeout(context.Background(), ch, "BLOCK_JOB_READY",
		[]string{"mirror-disk0", "mirror-disk1"}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("waitBlockJobsTimeout() = nil, want timeout error when one job never reaches READY")
	}
}

// TestWaitBlockJobsTimeout_SurfacesJobError asserts a terminal event carrying a
// non-empty "error" for a set member fails closed.
func TestWaitBlockJobsTimeout_SurfacesJobError(t *testing.T) {
	ch := make(chan qmp.Event, 1)
	ch <- qmp.Event{Event: "BLOCK_JOB_COMPLETED", Data: map[string]interface{}{
		"device": "mirror-disk0", "error": "No space left on device",
	}}

	err := waitBlockJobsTimeout(context.Background(), ch, "BLOCK_JOB_COMPLETED",
		[]string{"mirror-disk0"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "No space left") {
		t.Fatalf("waitBlockJobsTimeout() = %v, want error surfacing the job error", err)
	}
}

// TestWaitBlockJobsTimeout_ClosedChannel asserts a closed channel before the set
// is satisfied is an error (fail-closed end-of-stream).
func TestWaitBlockJobsTimeout_ClosedChannel(t *testing.T) {
	ch := make(chan qmp.Event)
	close(ch)
	err := waitBlockJobsTimeout(context.Background(), ch, "BLOCK_JOB_READY",
		[]string{"mirror-disk0"}, time.Second)
	if err == nil {
		t.Fatal("waitBlockJobsTimeout() = nil, want error on closed channel")
	}
}
