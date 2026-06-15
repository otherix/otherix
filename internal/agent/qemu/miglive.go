// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
)

// LiveSourceConn is the QMP surface RunLiveSource drives on the source
// side of a live migration. It exposes the multiplexed event channel
// directly so the package-private MIGRATION / block-job waiters are
// reused unchanged. *QMPClient satisfies it.
type LiveSourceConn interface {
	ObjectAddTLSCreds(id, dir, endpoint string) error
	ObjectAddAuthz(id, identity string) error
	ObjectDel(id string) error
	BlockdevAddNBD(nodeName, host string, port int, export, tlsCreds, tlsHostname string) error
	BlockdevMirror(jobID, device, target string) error
	NBDServerStart(host string, port int, tlsCreds, tlsAuthz string) error
	BlockExportAdd(id, nodeName, name string, writable bool) error
	MigrateSetCapabilities(caps map[string]bool) error
	MigrateSetParameters(p LiveParams) error
	Migrate(uri string) error
	MigrateIncoming(uri string) error
	BlockJobCancel(device string, force bool) error
	MigrateContinue() error
	MigrateCancel() error
	BlockdevDel(nodeName string) error
	QueryMigrate() (MigrateInfo, error)
	QueryBlockJobs() ([]BlockJobInfo, error)
	Events(ctx context.Context) (<-chan qmp.Event, error)
	Close() error
}

var _ LiveSourceConn = (*QMPClient)(nil)

var _ LiveTargetConn = (*QMPClient)(nil)

// LiveSourceDisk is one source disk mirrored to a matching target NBD export.
type LiveSourceDisk struct {
	SrcNode string // running guest BlockBackend device name (blockdev-mirror device=), "virtio<i>"
	JobID   string // blockdev-mirror job-id, "mirror-disk<i>"
	NBDNode string // local NBD client blockdev node-name, "mirror-target<i>"
	Export  string // target NBD export name, "<migrationID>-<i>"
}

// LiveSourceSpec parameterizes the source side of a live migration.
type LiveSourceSpec struct {
	// Disk mirrors: every source disk mirrored to its matching target export,
	// in boot-first index order. Disks[0] is the boot disk the reporter tracks.
	Disks          []LiveSourceDisk
	TargetHost     string // target NBD host
	NBDPort        int    // target NBD port
	CredsDir       string // client TLS creds dir
	TargetIdentity string // tls-hostname pin (node-<target>.agents.otherix.local)
	// RAM stream.
	RAMHost string // target RAM host (usually the same host)
	RAMPort int    // target RAM port
	// Tuning.
	DowntimeMs           int
	MaxBandwidthBytes    int64
	ConvergenceTimeout   time.Duration // watchdog: max time without RAM progress before abort
	ProgressPollInterval time.Duration // watchdog poll cadence (default 2s if zero)
	DiskMirrorTimeout    time.Duration // max time to reach BLOCK_JOB_READY (bulk disk copy); default liveDefaultDiskMirrorTimeout
	SwitchoverTimeout    time.Duration // max time for each post-pre-switchover wait (finalize, final completed); default liveDefaultSwitchoverTimeout
}

// Per-phase fail-closed bounds applied by RunLiveSource when a LiveSourceSpec
// leaves them zero. They turn a genuinely stuck QEMU (a wait whose event never
// arrives) into an aborted migration that leaves the guest on the source, rather
// than a wait that blocks forever. The disk-copy bound is generous (the whole
// disk may transfer); the switchover bounds are short (the mirror is already
// caught up and the RAM already converged at pre-switchover).
const (
	liveDefaultDiskMirrorTimeout = 30 * time.Minute
	liveDefaultSwitchoverTimeout = 2 * time.Minute
)

// LiveProgress is a point-in-time snapshot of a live migration's disk-mirror
// and RAM-transfer state, emitted periodically by RunLiveSource so the manager
// can log it and surface progress on the migration record.
type LiveProgress struct {
	BlockJobs []BlockJobInfo
	Migrate   MigrateInfo
}

// RunLiveSource drives the source side of a live migration to completion
// or fails closed (aborting and leaving the guest on the source).
// report, if non-nil, is called periodically (every
// s.ProgressPollInterval, default 3s) with a LiveProgress snapshot of the
// disk-mirror block jobs and the RAM-migration counters so the caller can
// log and surface progress.
//
// The load-bearing order is finalize-then-continue: the disk mirror is
// gracefully finalized (block-job-cancel force=false then wait
// BLOCK_JOB_COMPLETED) WHILE the migration is paused at pre-switchover,
// and only then is migrate-continue issued. This guarantees the
// destination disk is a fully synced copy before the guest is released
// to the target. On ANY failure (including a non-converging RAM stream
// caught by the watchdog) RunLiveSource aborts both the RAM migration
// and the disk mirror and returns the wrapped error, leaving the guest
// running on the source.
func RunLiveSource(ctx context.Context, conn LiveSourceConn, s LiveSourceSpec, report func(LiveProgress)) error {
	rawCh, err := conn.Events(ctx)
	if err != nil {
		return fmt.Errorf("open qmp events: %w", err)
	}

	// go-qemu demultiplexes command responses AND async events out of ONE
	// goroutine over unbuffered channels: if nobody is receiving the event
	// channel, that goroutine blocks delivering the event and therefore stops
	// delivering command responses too. Every QMP command that starts a job
	// emits its status event on start (blockdev-mirror, migrate,
	// block-job-cancel, migrate-continue), and that event can reach the wire
	// before the command's own reply - so without a continuous drainer the very
	// next command's reply never arrives and the run wedges forever. fanoutEvents
	// drains rawCh from now until this function returns, buffering events so the
	// staged waiters below still see them in order. Its lifetime is an
	// independent drainCtx cancelled by the deferred stopDrain, which runs AFTER
	// the inline abort path, so a parent-ctx-triggered abort cannot re-wedge the
	// listener mid-teardown.
	drainCtx, stopDrain := context.WithCancel(context.Background())
	defer stopDrain()
	ch := fanoutEvents(drainCtx, rawCh)

	diskMirrorTimeout := durationOr(s.DiskMirrorTimeout, liveDefaultDiskMirrorTimeout)
	switchoverTimeout := durationOr(s.SwitchoverTimeout, liveDefaultSwitchoverTimeout)

	// Periodic best-effort progress reporter. It polls query-block-jobs
	// (disk phase) and query-migrate (RAM phase) concurrently with the
	// main sequencer and the watchdog; the underlying SocketMonitor.Run is
	// mutex-serialized, so concurrent polling cannot mix responses. The
	// reporter exits when RunLiveSource returns (done closed via defer).
	done := make(chan struct{})
	defer close(done)
	go reportLiveProgress(done, conn, s, report)

	if err := conn.ObjectAddTLSCreds(migTLSCredsID, s.CredsDir, "client"); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("add client tls creds: %w", err)
	}
	// Remove the client TLS-creds object on every return below (success tail and
	// every abort): a guest that stays on the source after an abort must not keep
	// "migtls", or its NEXT migration fails "duplicate property 'migtls'".
	// Best-effort - on success the guest is already on the target.
	defer func() { _ = conn.ObjectDel(migTLSCredsID) }()
	// Mirror EVERY source disk to its matching target export so the device
	// topology matches. The boot disk and the cidata disk each get an NBD
	// client blockdev + a blockdev-mirror; both must reach READY before the RAM
	// migrate.
	jobIDs, err := startDiskMirrors(conn, s)
	if err != nil {
		abortLiveSource(conn, s)
		return err
	}
	// Wait for ALL mirrors to reach READY in a single pass. The tiny cidata
	// mirror reaches READY long before the boot disk, so this MUST be a
	// set-based wait that does not discard the early event (a sequential
	// single-job wait per disk would block forever - see waitBlockJobsEvent).
	if err := waitBlockJobsTimeout(ctx, ch, "BLOCK_JOB_READY", jobIDs, diskMirrorTimeout); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait mirrors ready: %w", err)
	}

	caps := map[string]bool{
		"events":                  true,
		"pause-before-switchover": true,
		"auto-converge":           true,
	}
	if err := conn.MigrateSetCapabilities(caps); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("set migration capabilities: %w", err)
	}
	params := LiveParams{
		TLSCreds:             migTLSCredsID,
		TLSHostname:          s.TargetIdentity,
		DowntimeMs:           s.DowntimeMs,
		MaxBandwidthBytes:    s.MaxBandwidthBytes,
		CPUThrottleInitial:   20,
		CPUThrottleIncrement: 10,
	}
	if err := conn.MigrateSetParameters(params); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("set migration parameters: %w", err)
	}

	uri := "tcp:" + net.JoinHostPort(s.RAMHost, strconv.Itoa(s.RAMPort))
	if err := conn.Migrate(uri); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("start ram migration: %w", err)
	}

	if err := waitPreSwitchoverWithWatchdog(ctx, conn, ch, s); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait pre-switchover: %w", err)
	}

	// Finalize EVERY disk mirror WHILE paused at pre-switchover, then release
	// the source. This order is load-bearing and must not be reordered. Issue
	// all the graceful cancels BEFORE waiting the COMPLETED set, mirroring the
	// READY phase, so no completed event is missed.
	if err := finalizeDiskMirrors(conn, s); err != nil {
		abortLiveSource(conn, s)
		return err
	}
	if err := waitBlockJobsTimeout(ctx, ch, "BLOCK_JOB_COMPLETED", jobIDs, switchoverTimeout); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait mirrors completed: %w", err)
	}
	if err := conn.MigrateContinue(); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("continue migration: %w", err)
	}
	if err := waitMigrationStatusTimeout(ctx, ch, "completed", []string{"failed", "cancelled"}, switchoverTimeout); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait migration completed: %w", err)
	}

	// The migration has completed; the guest now runs on the target. The
	// stale NBD client blockdevs are best-effort cleanup - a failure here
	// does not undo a completed migration.
	for _, d := range s.Disks {
		_ = conn.BlockdevDel(d.NBDNode)
	}
	return nil
}

// LiveTargetConn is the QMP surface RunLiveTarget drives on the target side
// after the incoming RAM stream completes. *QMPClient satisfies it.
type LiveTargetConn interface {
	Events(ctx context.Context) (<-chan qmp.Event, error)
	BlockExportDel(id string) error
	ObjectDel(id string) error
	NBDServerStop() error
	Cont() error
	AnnounceSelf(p AnnounceParameters) error
	Close() error
}

// LiveTargetSpec parameterizes the target-side resume.
type LiveTargetSpec struct {
	ExportIDs       []string      // block-export-add ids to remove, in index order (one per LiveIncomingSpec.Disks entry)
	IncomingTimeout time.Duration // fail-closed bound on the incoming wait; default liveDefaultIncomingTimeout
}

// liveDefaultIncomingTimeout bounds the wait for the incoming migration to
// complete on the target. It covers the whole RAM transfer, so it is generous.
const liveDefaultIncomingTimeout = 30 * time.Minute

// RunLiveTarget drives the target side of a live migration to a resumed guest
// or fails closed. It waits for the incoming MIGRATION to reach "completed",
// removes every writable NBD disk export, and issues cont - which, under the
// late-block-activate capability, activates the destination disks and resumes
// the guest from the transferred RAM. It MUST be called (and reach the wait)
// before the source reaches pre-switchover, so the completed event is observed
// without adding to switchover downtime. Once the incoming migration completes
// there is no fall-back to the source; any failure here is terminal for the
// guest on this node and the caller marks the VM failed.
func RunLiveTarget(ctx context.Context, conn LiveTargetConn, s LiveTargetSpec) error {
	rawCh, err := conn.Events(ctx)
	if err != nil {
		return fmt.Errorf("open qmp events: %w", err)
	}
	drainCtx, stopDrain := context.WithCancel(context.Background())
	defer stopDrain()
	ch := fanoutEvents(drainCtx, rawCh)

	timeout := durationOr(s.IncomingTimeout, liveDefaultIncomingTimeout)
	if err := waitMigrationStatusTimeout(ctx, ch, "completed", []string{"failed"}, timeout); err != nil {
		return fmt.Errorf("wait incoming completed: %w", err)
	}
	// Order is load-bearing: drop EVERY writable export before the guest takes
	// the disks, so no external NBD writer can touch a disk once cont activates
	// it.
	for _, id := range s.ExportIDs {
		if err := conn.BlockExportDel(id); err != nil {
			return fmt.Errorf("remove nbd export %q: %w", id, err)
		}
	}
	if err := conn.NBDServerStop(); err != nil {
		return fmt.Errorf("stop nbd server: %w", err)
	}
	if err := conn.Cont(); err != nil {
		return fmt.Errorf("resume guest: %w", err)
	}
	// The guest is now running on this node. Remove the migration TLS objects so
	// this qemu can migrate AGAIN later without "duplicate property 'migtls'".
	// Best-effort: the guest is already resumed, so a del failure must NOT fail
	// the resume (it only degrades to the prior leak, blocking the NEXT migration).
	_ = conn.ObjectDel(migTLSCredsID)
	_ = conn.ObjectDel(migAuthzID)
	return nil
}

// fanoutEvents continuously drains src into an unbounded in-memory buffer and
// re-emits every event, in order, on the returned channel. It decouples
// go-qemu's single producer goroutine - which serializes command responses and
// async events and blocks on an unbuffered event channel - from RunLiveSource's
// staged consumers, which only receive events while inside a waiter. The drainer
// never blocks src for longer than one loop iteration, so the producer is free to
// keep delivering command responses no matter when an event is emitted. It stops
// and closes the returned channel when ctx is cancelled or src is closed (after
// flushing the buffer), so waiters observe end-of-stream.
func fanoutEvents(ctx context.Context, src <-chan qmp.Event) <-chan qmp.Event {
	out := make(chan qmp.Event)
	go func() {
		defer close(out)
		var buf []qmp.Event
		for {
			var outCh chan<- qmp.Event
			var next qmp.Event
			if len(buf) > 0 {
				outCh = out
				next = buf[0]
			}
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					// Source closed: stop receiving, flush what remains.
					src = nil
					if len(buf) == 0 {
						return
					}
					continue
				}
				buf = append(buf, ev)
			case outCh <- next:
				buf = buf[1:]
				if src == nil && len(buf) == 0 {
					return
				}
			}
		}
	}()
	return out
}

// durationOr returns v when it is positive, otherwise def. Used to apply the
// per-phase timeout defaults when a LiveSourceSpec leaves them zero.
func durationOr(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// waitBlockJobsTimeout waits until eventName has arrived for EVERY job id in
// jobIDs (in any order), reading the single fanned-out event channel once. It
// must not discard an event for a still-pending job in the set - the cidata
// mirror reaches READY long before the boot disk, so events arrive out of
// order. A matching terminal event carrying a non-empty "error" fails closed.
// One deadline context bounds the whole set so a job whose event never arrives
// (a stuck mirror) aborts the migration instead of blocking forever.
func waitBlockJobsTimeout(ctx context.Context, ch <-chan qmp.Event, eventName string, jobIDs []string, timeout time.Duration) error {
	remaining := make(map[string]bool, len(jobIDs))
	for _, id := range jobIDs {
		remaining[id] = true
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return waitBlockJobsEvent(wctx, ch, eventName, remaining)
}

// waitMigrationStatusTimeout wraps waitMigrationStatus with a fail-closed
// deadline for the post-switchover completion wait.
func waitMigrationStatusTimeout(ctx context.Context, ch <-chan qmp.Event, want string, failStates []string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := waitMigrationStatus(wctx, ch, want, failStates)
	return err
}

// reportLiveProgress polls the disk-mirror block jobs and the RAM-migration
// counters every s.ProgressPollInterval (default 3s) and, when report is
// non-nil, hands each snapshot to it. Both queries are best-effort: an
// individual query error is skipped so a transient failure never stalls
// progress reporting. It runs until done is closed (RunLiveSource returns).
func reportLiveProgress(done <-chan struct{}, conn LiveSourceConn, s LiveSourceSpec, report func(LiveProgress)) {
	if report == nil {
		return
	}
	interval := s.ProgressPollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			bj, _ := conn.QueryBlockJobs()
			mi, _ := conn.QueryMigrate()
			report(LiveProgress{BlockJobs: bj, Migrate: mi})
		}
	}
}

// waitPreSwitchoverWithWatchdog waits for the MIGRATION pre-switchover
// event while a background watchdog polls query-migrate. If the RAM
// remaining stops decreasing for longer than s.ConvergenceTimeout the
// watchdog cancels the wait so RunLiveSource aborts to the source rather
// than blocking forever on a migration that will never converge.
func waitPreSwitchoverWithWatchdog(ctx context.Context, conn LiveSourceConn, ch <-chan qmp.Event, s LiveSourceSpec) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var stalled atomic.Bool
	go func() {
		interval := s.ProgressPollInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var minRemaining int64 = -1
		lastProgress := time.Now()
		for {
			select {
			case <-wctx.Done():
				return
			case <-ticker.C:
				info, err := conn.QueryMigrate()
				if err != nil {
					continue
				}
				if minRemaining < 0 || info.RAM.Remaining < minRemaining {
					minRemaining = info.RAM.Remaining
					lastProgress = time.Now()
				}
				if time.Since(lastProgress) > s.ConvergenceTimeout {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	if _, err := waitMigrationStatus(wctx, ch, "pre-switchover", []string{"failed", "cancelled"}); err != nil {
		if stalled.Load() {
			return fmt.Errorf("migration did not converge within %s (watchdog abort)", s.ConvergenceTimeout)
		}
		return err
	}
	return nil
}

// LiveIncomingSpec parameterizes the target side of a live migration.
type LiveIncomingSpec struct {
	CredsDir       string             // server TLS creds dir
	SourceIdentity string             // "CN=node-<source>"; pins the connecting source DN
	BindHost       string             // migration ingress bind host
	NBDPort        int                // NBD disk-export ingress port
	RAMPort        int                // RAM migrate-incoming ingress port
	Disks          []LiveIncomingDisk // destination disks, in boot-first index order
}

// LiveIncomingDisk is one destination disk the target boots in index order.
//
// A WRITABLE disk (ReadOnly false, e.g. the boot disk) is created empty and
// NBD-exported (Export/ExportID set) so the source live-mirrors it.
//
// A READ-ONLY disk (ReadOnly true, the cidata ISO) cannot be live block-mirrored
// (the guest's VIRTIO_BLK_F_RO forces the blockdev read-only, but an NBD mirror
// needs a writable target). It is instead rebuilt locally from the migrated VM's
// cloud-init inputs and attached read-only - Export/ExportID are empty, it is
// NOT exported and NOT mirrored. The read-only blockdev makes the guest see the
// disk read-only, matching the source's VIRTIO_BLK_F_RO.
type LiveIncomingDisk struct {
	Node     string // blockdev node-name, "target-disk<i>"
	Path     string // destination file path
	Export   string // NBD export name, "<migrationID>-<i>"; empty for read-only disks (not exported)
	ExportID string // block-export-add id, "exp<i>"; empty for read-only disks (not exported)
	Format   string // "qcow2" | "raw"
	ReadOnly bool
}

// SetupLiveIncoming configures a just-launched (-incoming defer) target QEMU to
// receive a live migration: install server TLS creds + the source-DN authz pin,
// start the in-process NBD server and export the WRITABLE destination disks
// writable, then arm the deferred incoming RAM channel. The order matters - the
// NBD exports and TLS objects must exist before migrate-incoming opens the socket.
//
// Read-only disks (cidata) are rebuilt locally and attached read-only, NOT
// exported/mirrored, so they are skipped here. Only writable disks are exported.
//
// NOTE: NBDServerStart's max-connections is currently 1, which is correct because
// today exactly one WRITABLE disk (boot) is exported. When multiple writable data
// disks are exported in future, max-connections must grow to the writable count,
// else the 2nd mirror connection blocks (a real hang observed when the read-only
// cidata was wrongly exported + mirrored).
func SetupLiveIncoming(conn LiveSourceConn, s LiveIncomingSpec) error {
	if err := conn.ObjectAddTLSCreds(migTLSCredsID, s.CredsDir, "server"); err != nil {
		return fmt.Errorf("add server tls creds: %w", err)
	}
	if err := conn.ObjectAddAuthz(migAuthzID, s.SourceIdentity); err != nil {
		return fmt.Errorf("add source authz: %w", err)
	}
	if err := conn.NBDServerStart(s.BindHost, s.NBDPort, migTLSCredsID, migAuthzID); err != nil {
		return fmt.Errorf("start nbd server: %w", err)
	}
	// Export every WRITABLE destination disk writable (each must receive the
	// mirror), in boot-first index order, before arming the incoming RAM
	// channel. A disk is writable iff !d.ReadOnly (equivalently d.ExportID != "");
	// read-only disks (cidata) are rebuilt locally, never exported, so skip them.
	for _, d := range s.Disks {
		if d.ReadOnly {
			continue
		}
		if err := conn.BlockExportAdd(d.ExportID, d.Node, d.Export, true); err != nil {
			return fmt.Errorf("export destination disk %s: %w", d.Node, err)
		}
	}
	// late-block-activate defers VM-level disk activation to switchover so
	// the booting guest does not lock the disk while the NBD receive writes
	// it (the 8.2-safe path - no allow-inactive).
	caps := map[string]bool{
		"events":              true,
		"late-block-activate": true,
	}
	if err := conn.MigrateSetCapabilities(caps); err != nil {
		return fmt.Errorf("set migration capabilities: %w", err)
	}
	params := LiveParams{
		TLSCreds: migTLSCredsID,
		TLSAuthz: migAuthzID,
	}
	if err := conn.MigrateSetParameters(params); err != nil {
		return fmt.Errorf("set migration parameters: %w", err)
	}
	uri := "tcp:" + net.JoinHostPort(s.BindHost, strconv.Itoa(s.RAMPort))
	if err := conn.MigrateIncoming(uri); err != nil {
		return fmt.Errorf("arm incoming migration: %w", err)
	}
	return nil
}

// LiveIncomingArgs returns the migration-specific QEMU flags the target launch
// appends to the base VM cmdline: each destination disk as a node-named
// blockdev plus its guest virtio-blk device (so SetupLiveIncoming can
// NBD-export each one and the guest boots them in index order) and -incoming
// defer (start paused, defer the transport so TLS can be set over QMP before
// the socket accepts).
//
// The base cmdline (cmdline.go) expresses the OS disk as `-drive ...,if=virtio`,
// which carries no node-name. block-export-add needs a node-name to export, so
// each migration disk is expressed instead as an explicit two-layer -blockdev
// (format node over a file protocol node) whose top node-name matches the
// export node-name SetupLiveIncoming references, paired with a virtio-blk-pci
// device bound to that node.
//
// A read-only disk (cidata) gets read-only=on on the -blockdev (so the guest
// sees the disk read-only, matching the source's VIRTIO_BLK_F_RO). It is NOT
// auto-read-only - the rebuilt cidata is a static file that is never mirrored.
// Its -device is PLAIN: virtio-blk-pci has no device-level read-only property,
// and -device ...,read-only=on makes qemu refuse to launch. A writable disk gets
// neither (writable blockdev, plain device) so it can receive the NBD mirror.
func LiveIncomingArgs(s LiveIncomingSpec) []string {
	args := make([]string, 0, len(s.Disks)*4+2)
	for _, d := range s.Disks {
		// discard=unmap + detect-zeroes=unmap so the WRITE_ZEROES the source
		// mirror sends (for the disk's allocated-but-zero and unallocated
		// clusters) land as cheap hole punches instead of allocating and
		// writing runs of literal zeros. A sync=full mirror otherwise copies
		// every zero cluster byte-for-byte, which under slow emulation wedges
		// the transfer; this keeps it to the disk's real data.
		blockdev := fmt.Sprintf(
			"driver=%s,node-name=%s,discard=unmap,detect-zeroes=unmap,file.driver=file,file.filename=%s",
			d.Format, d.Node, d.Path,
		)
		if d.ReadOnly {
			blockdev += ",read-only=on"
		}
		args = append(args, "-blockdev", blockdev)
		// Plain device for every disk: virtio-blk-pci carries no read-only
		// property (read-only=on here makes qemu refuse to launch); the guest's
		// read-only view comes from the blockdev above.
		args = append(args, "-device", fmt.Sprintf("virtio-blk-pci,drive=%s", d.Node))
	}
	args = append(args, "-incoming", "defer")
	return args
}

// startDiskMirrors adds an NBD client blockdev and starts a blockdev-mirror for
// every source disk, returning the started job ids in order. On the first error
// it returns a wrapped error; the caller aborts. The caller waits the returned
// job ids for BLOCK_JOB_READY as a set.
func startDiskMirrors(conn LiveSourceConn, s LiveSourceSpec) ([]string, error) {
	jobIDs := make([]string, 0, len(s.Disks))
	for _, d := range s.Disks {
		if err := conn.BlockdevAddNBD(d.NBDNode, s.TargetHost, s.NBDPort, d.Export, migTLSCredsID, s.TargetIdentity); err != nil {
			return nil, fmt.Errorf("add nbd client blockdev %q: %w", d.NBDNode, err)
		}
		if err := conn.BlockdevMirror(d.JobID, d.SrcNode, d.NBDNode); err != nil {
			return nil, fmt.Errorf("start disk mirror %q: %w", d.JobID, err)
		}
		jobIDs = append(jobIDs, d.JobID)
	}
	return jobIDs, nil
}

// finalizeDiskMirrors issues the graceful block-job-cancel (force=false) for
// every disk mirror. All cancels are issued before the caller waits the
// COMPLETED set, so no completed event is missed.
func finalizeDiskMirrors(conn LiveSourceConn, s LiveSourceSpec) error {
	for _, d := range s.Disks {
		if err := conn.BlockJobCancel(d.JobID, false); err != nil {
			return fmt.Errorf("finalize disk mirror %q: %w", d.JobID, err)
		}
	}
	return nil
}

// abortLiveSource fails the source side closed: it cancels the RAM
// migration, force-aborts every disk mirror, and removes every NBD client
// blockdev. Each step is best-effort; an error in one does not stop the
// others, because the goal is to return the guest to a clean running
// state on the source no matter which step originally failed.
func abortLiveSource(conn LiveSourceConn, s LiveSourceSpec) {
	_ = conn.MigrateCancel()
	for _, d := range s.Disks {
		_ = conn.BlockJobCancel(d.JobID, true)
		_ = conn.BlockdevDel(d.NBDNode)
	}
}
