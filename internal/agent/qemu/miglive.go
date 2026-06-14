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

// LiveSourceSpec parameterizes the source side of a live migration.
type LiveSourceSpec struct {
	// Disk mirror.
	SrcDiskNode    string // the running guest's source disk node-name (blockdev-mirror device=)
	JobID          string // blockdev-mirror job-id
	NBDNode        string // local NBD client node-name (blockdev-add target)
	TargetHost     string // target NBD host
	NBDPort        int    // target NBD port
	Export         string // NBD export name (the migration auth token)
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
}

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
	ch, err := conn.Events(ctx)
	if err != nil {
		return fmt.Errorf("open qmp events: %w", err)
	}

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
	if err := conn.BlockdevAddNBD(s.NBDNode, s.TargetHost, s.NBDPort, s.Export, migTLSCredsID, s.TargetIdentity); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("add nbd client blockdev: %w", err)
	}
	if err := conn.BlockdevMirror(s.JobID, s.SrcDiskNode, s.NBDNode); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("start disk mirror: %w", err)
	}
	if err := waitBlockJobEvent(ctx, ch, "BLOCK_JOB_READY", s.JobID); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait mirror ready: %w", err)
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

	// Finalize the disk mirror WHILE paused at pre-switchover, then
	// release the source. This order is load-bearing and must not be
	// reordered.
	if err := conn.BlockJobCancel(s.JobID, false); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("finalize disk mirror: %w", err)
	}
	if err := waitBlockJobEvent(ctx, ch, "BLOCK_JOB_COMPLETED", s.JobID); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait mirror completed: %w", err)
	}
	if err := conn.MigrateContinue(); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("continue migration: %w", err)
	}
	if _, err := waitMigrationStatus(ctx, ch, "completed", []string{"failed", "cancelled"}); err != nil {
		abortLiveSource(conn, s)
		return fmt.Errorf("wait migration completed: %w", err)
	}

	// The migration has completed; the guest now runs on the target. The
	// stale NBD client blockdev is best-effort cleanup - a failure here
	// does not undo a completed migration.
	_ = conn.BlockdevDel(s.NBDNode)
	return nil
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
	CredsDir       string // server TLS creds dir
	SourceIdentity string // "CN=node-<source>"; pins the connecting source DN
	DiskNode       string // node-name of the destination disk blockdev (exported + booted)
	DiskPath       string // destination disk file path (for LiveIncomingArgs -blockdev)
	Export         string // NBD export name (the migration auth token)
	ExportID       string // block-export-add id (handle for teardown)
	BindHost       string // migration ingress bind host
	NBDPort        int    // NBD disk-export ingress port
	RAMPort        int    // RAM migrate-incoming ingress port
}

// SetupLiveIncoming configures a just-launched (-incoming defer) target QEMU to
// receive a live migration: install server TLS creds + the source-DN authz pin,
// start the in-process NBD server and export the destination disk writable, then
// arm the deferred incoming RAM channel. The order matters - the NBD export and
// TLS objects must exist before migrate-incoming opens the socket.
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
	if err := conn.BlockExportAdd(s.ExportID, s.DiskNode, s.Export, true); err != nil {
		return fmt.Errorf("export destination disk: %w", err)
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
// appends to the base VM cmdline: the destination disk as a node-named blockdev
// (so SetupLiveIncoming can NBD-export it and the guest can boot it) and
// -incoming defer (start paused, defer the transport so TLS can be set over QMP
// before the socket accepts).
//
// The base cmdline (cmdline.go) expresses the OS disk as `-drive ...,if=virtio`,
// which carries no node-name. block-export-add needs a node-name to export, so
// the migration disk is expressed instead as an explicit two-layer -blockdev
// (qcow2 format node over a file protocol node) whose top node-name matches the
// export node-name SetupLiveIncoming references.
func LiveIncomingArgs(s LiveIncomingSpec) []string {
	return []string{
		"-blockdev", fmt.Sprintf(
			"driver=qcow2,node-name=%s,file.driver=file,file.filename=%s",
			s.DiskNode, s.DiskPath,
		),
		"-incoming", "defer",
	}
}

// abortLiveSource fails the source side closed: it cancels the RAM
// migration, force-aborts the disk mirror, and removes the NBD client
// blockdev. Each step is best-effort; an error in one does not stop the
// others, because the goal is to return the guest to a clean running
// state on the source no matter which step originally failed.
func abortLiveSource(conn LiveSourceConn, s LiveSourceSpec) {
	_ = conn.MigrateCancel()
	_ = conn.BlockJobCancel(s.JobID, true)
	_ = conn.BlockdevDel(s.NBDNode)
}
