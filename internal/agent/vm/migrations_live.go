// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
)

// liveSourceDiskNode is the BlockBackend device name of the OS disk on the
// RUNNING source guest, the device blockdev-mirror copies. The base launch
// (cmdline.go BuildArgs) attaches the boot disk as the FIRST `-drive
// file=...,if=virtio` (cidata is a second if=virtio drive), and QEMU names
// if=virtio BlockBackends virtio0, virtio1, ... in order, so the boot disk's
// device name is virtio0. blockdev-mirror's device param accepts a
// BlockBackend device name, so virtio0 is the stable handle: the boot disk is
// always the first if=virtio drive and the source VM always launches with it
// (never OmitBootDisk). The auto node-name (#blockNNN) is unstable and must
// NOT be hardcoded. Verified via query-block on a running guest.
//
// virtio0 is now the INVARIANT boot-disk handle for BOTH a created VM (the
// if=virtio BlockBackend virtio0) AND a migrated-in VM (the explicit
// `-blockdev node-name=virtio0` set by replicateIncomingDisks/LiveIncomingArgs),
// so a second outgoing migration off a migrated-in guest resolves the boot disk
// identically with no special-casing.
const liveSourceDiskNode = "virtio0"

// liveExportID is the block-export-add handle for the boot (index 0)
// destination disk. The target replicates every manifest disk under the
// "virtio<i>" node-name / "exp<i>" export-id convention and the resume
// dels every export the migration record carries (ExportIDs). liveExportID is
// the defensive boot-export fallback used only when the record carries no
// ExportIDs, so an unexpectedly-empty record still tears down the boot export
// rather than leaking it.
const liveExportID = "exp0"

// liveMirrorJobID is the blockdev-mirror job-id on the source side.
const liveMirrorJobID = "mirror-disk0"

// liveDefaultDowntimeMs is the target max switchover downtime when the spec
// carries none.
const liveDefaultDowntimeMs = 300

// startIncomingLive prepares this node to RECEIVE a live migration: reserve a
// (RAM, NBD) port pair, materialize server TLS creds, create the destination
// disk, adopt a stopped Migrated VM, launch a paused -incoming qemu booting the
// node-named destination disk, and arm the deferred incoming RAM channel + NBD
// disk export over QMP. Synchronous; returns both endpoints + the token so the
// CP can relay them to the source. On any failure after reserving the pair the
// pair is released; no migration record is stored until every step succeeds.
func (m *Manager) startIncomingLive(ctx context.Context, s IncomingSpec) (IncomingResult, error) {
	// Idempotent resume: a re-driven task must not re-reserve ports or
	// re-adopt the VM. Return the endpoints minted the first time.
	if rec, ok := m.migrations.Get(s.MigrationID); ok && rec.Role == migration.RoleTarget {
		nbdEndpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(rec.NBDPort))
		return IncomingResult{ListenEndpoint: rec.ListenEndpt, NBDEndpoint: nbdEndpoint, AuthToken: rec.AuthToken}, nil
	}

	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		// %w so the handler can errors.Is(ErrNoFreePort) and surface a
		// retryable capacity condition rather than a hard failure.
		return IncomingResult{}, fmt.Errorf("reserve migration port pair: %w", err)
	}

	// Full rollback on any pre-success failure: a failed incoming prep must
	// leave NOTHING behind so the CP's retry (re-driving StartIncoming) starts
	// fresh. Each error return below calls cleanup, which unwinds exactly the
	// steps that ran: kill the launched -incoming qemu, drop the adopted VM
	// record + its on-disk state, and release the reserved port pair. adopted
	// and launched track how far execution got.
	var adopted *VM
	launched := false
	nicsMaterialised := false
	cleanup := func() {
		if launched && adopted != nil {
			m.killQEMU(adopted)
		}
		if nicsMaterialised && adopted != nil {
			m.teardownNICs(adopted.NICs)
		}
		if adopted != nil {
			m.removeAdoptedVM(adopted.ID)
		}
		m.migPorts.ReleasePair(ram, nbd)
	}

	credsDir := filepath.Join(m.stateDir, "migrations", s.MigrationID.String(), "tls")
	if err := qemu.MaterializeMigrationCreds(credsDir, qemu.MigrationTLSServer, m.tlsCA, m.tlsCert, m.tlsKey); err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: s.VMUUID, Name: s.VMName, VCPUs: s.VCPUs, MemoryMib: s.MemoryMib,
		PoolName: s.PoolName, Architecture: s.Architecture,
		InitialStatus: StatusMigratingIncoming,
		NICs:          s.NICs,
	})
	if err != nil {
		cleanup()
		return IncomingResult{}, err
	}
	adopted = v

	// Replicate every manifest disk in boot-first index order so the
	// -incoming qemu's device topology matches the source.
	token := s.MigrationID.String()
	incomingDisks, exportIDs, err := m.replicateIncomingDisks(ctx, v, s, token)
	if err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	ls := qemu.LiveIncomingSpec{
		CredsDir:       credsDir,
		SourceIdentity: s.SourceIdentity,
		BindHost:       s.BindHost,
		NBDPort:        nbd,
		RAMPort:        ram,
		Disks:          incomingDisks,
	}

	// Materialize the migrated VM's NIC taps before launching the incoming
	// qemu, so -netdev tap binds ready taps and the resumed guest has network.
	if err := m.materialiseNICs(v.NICs); err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("materialise migrated nics: %v", err)
	}
	nicsMaterialised = true

	// Launch the paused -incoming qemu, then dial it and arm the incoming
	// transport (NBD export + RAM channel) over QMP. The launched qemu keeps
	// running; the setup conn is closed once setup completes (it only carries
	// the monitoring socket, not the qemu process).
	if err := m.migLaunchIncoming(ctx, v, ls); err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("launch incoming qemu: %v", err)
	}
	launched = true
	if err := m.dialAndSetupIncoming(v, ls); err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	ramEndpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(ram))
	nbdEndpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(nbd))
	m.migrations.Put(&migration.Record{
		MigrationID: s.MigrationID, VMID: s.VMUUID, VMName: s.VMName,
		Role: migration.RoleTarget, Mode: migration.ModeLive, Phase: migration.PhaseSetup,
		Port: ram, NBDPort: nbd, ListenEndpt: ramEndpoint, AuthToken: token,
		CredsDir: credsDir, ExportIDs: exportIDs, CreatedAt: time.Now().UTC(),
	})

	// Drive the target-side resume autonomously: wait for the incoming RAM
	// stream to complete, tear down the writable export, and cont the guest.
	// Started now (before the source reaches pre-switchover) so the completed
	// event is observed without adding to switchover downtime. Tracked by a
	// local task; the CP polls the SOURCE task, not this one.
	task := m.tasks.Create(TaskKindVMMigrate, s.VMUUID)
	// Detach from the request context: the resume outlives the StartIncoming
	// call (202 semantics) and must observe the incoming-completed event well
	// after the request returns, so a request-scoped cancel must not abort it.
	// resumeWG lets tests await this detached goroutine at teardown (production
	// never waits) so its persistVM write cannot race t.TempDir cleanup.
	m.resumeWG.Add(1)
	// #nosec G118 -- the target resume intentionally outlives the request (fire-and-converge); a cancelled HTTP request must not abort an in-flight guest resume. The CP re-drives on failure.
	go func() {
		defer m.resumeWG.Done()
		m.runIncomingResume(context.Background(), task.ID, s.MigrationID, v.ID, ram, nbd)
	}()

	return IncomingResult{ListenEndpoint: ramEndpoint, NBDEndpoint: nbdEndpoint, AuthToken: token}, nil
}

// dialAndSetupIncoming dials the just-launched -incoming qemu's QMP monitor and
// arms the incoming transport (NBD export + RAM channel). It closes the setup
// conn before returning: closing the QMP client closes only the monitoring
// socket, NOT the launched qemu process, so the paused -incoming qemu keeps
// running.
func (m *Manager) dialAndSetupIncoming(v *VM, ls qemu.LiveIncomingSpec) error {
	conn, err := m.migDialQMP(v.QMPSocket)
	if err != nil {
		return fmt.Errorf("dial target qmp: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := qemu.SetupLiveIncoming(conn, ls); err != nil {
		return fmt.Errorf("setup live incoming: %v", err)
	}
	return nil
}

// replicateIncomingDisks materializes one destination disk per manifest entry,
// in boot-first index order, and returns the per-disk LiveIncomingSpec entries
// plus the export ids ("exp<i>") of the WRITABLE disks only.
//
// Writable disks (boot at v.DiskPath, sized max(SizeBytes, ExpectedSize) to
// preserve the under-size guard; future data disks at disk<i>.img) are created
// empty and NBD-exported so the source live-mirrors them; each gets an
// Export/ExportID and is appended to exportIDs.
//
// A read-only disk (the cidata ISO) CANNOT be live block-mirrored (an NBD mirror
// needs a writable target node, but the guest's VIRTIO_BLK_F_RO forces the target
// blockdev read-only). It is instead REBUILT locally from the migrated VM's
// cloud-init inputs (deterministic; the guest reads cidata once at boot) and
// attached read-only. It is NOT exported and NOT added to exportIDs, so the
// source never mirrors it and the resume never dels a non-existent export. Its
// path is recorded on v.CidataPath (by incomingDiskPath) so a later cold restart
// reproduces the device.
//
// The manifest is guaranteed non-empty (the handler falls back to a single boot
// disk); an absent manifest is treated defensively as a boot-only disk.
func (m *Manager) replicateIncomingDisks(ctx context.Context, v *VM, s IncomingSpec, token string) ([]qemu.LiveIncomingDisk, []string, error) {
	disks := s.Disks
	if len(disks) == 0 {
		disks = []MigrationDisk{{Index: 0, SizeBytes: s.DiskSizeBytes, Format: "qcow2"}}
	}
	incomingDisks := make([]qemu.LiveIncomingDisk, 0, len(disks))
	exportIDs := make([]string, 0, len(disks))
	for _, d := range disks {
		path := m.incomingDiskPath(v, d)

		if d.ReadOnly {
			// Read-only cidata: rebuild it locally instead of creating an empty
			// disk + NBD-exporting it. No size handling (the build is a fixed
			// 10 MiB ISO). It carries an empty Export/ExportID - it still gets a
			// LiveIncomingDisk entry so LiveIncomingArgs emits its read-only
			// blockdev+device for topology match, but it is never exported.
			if err := m.migBuildCidata(path, v.Name, []byte(s.UserData), []byte(s.NetworkConfig)); err != nil {
				return nil, nil, err
			}
			incomingDisks = append(incomingDisks, qemu.LiveIncomingDisk{
				Node:     fmt.Sprintf("virtio%d", d.Index),
				Path:     path,
				Format:   d.Format,
				ReadOnly: true,
			})
			continue
		}

		// Writable disk. Destination virtual size must be >= the source's (see
		// the offline StartIncoming for the full rationale). The boot disk
		// (index 0) is sized max(d.SizeBytes, ExpectedSize) regardless of the
		// manifest size, preserving the offline under-size guard; every other
		// writable disk keeps its exact manifest size and is never inflated by
		// ExpectedSize.
		virtualBytes := d.SizeBytes
		if d.Index == 0 && s.ExpectedSize > virtualBytes {
			virtualBytes = s.ExpectedSize
		}
		// Validate the format explicitly rather than silently defaulting an
		// unrecognized format to raw: the same string is passed verbatim into
		// `-blockdev driver=<Format>`, so an unknown value must fail closed.
		var create func(context.Context, string, int64) error
		switch d.Format {
		case "qcow2":
			create = m.migCreateDisk
		case "raw":
			create = m.migCreateRawDisk
		default:
			return nil, nil, fmt.Errorf("unsupported migration disk format %q for disk %d", d.Format, d.Index)
		}
		if err := create(ctx, path, virtualBytes); err != nil {
			return nil, nil, err
		}

		incomingDisks = append(incomingDisks, qemu.LiveIncomingDisk{
			Node:     fmt.Sprintf("virtio%d", d.Index),
			Path:     path,
			Export:   fmt.Sprintf("%s-%d", token, d.Index),
			ExportID: fmt.Sprintf("exp%d", d.Index),
			Format:   d.Format,
			ReadOnly: false,
		})
		exportIDs = append(exportIDs, fmt.Sprintf("exp%d", d.Index))
	}
	return incomingDisks, exportIDs, nil
}

// incomingDiskPath resolves the destination file path for one manifest disk and
// records the cidata path on v when the entry is the read-only raw cidata ISO.
func (m *Manager) incomingDiskPath(v *VM, d MigrationDisk) string {
	switch {
	case d.Index == 0:
		return v.DiskPath
	case d.Format == "raw" && d.ReadOnly:
		// The cidata ISO: use the codebase cidata convention and record it on
		// the VM so a later cold restart reproduces the device.
		path := filepath.Join(filepath.Dir(v.DiskPath), "cidata.iso")
		m.mu.Lock()
		v.CidataPath = path
		m.mu.Unlock()
		return path
	default:
		return filepath.Join(filepath.Dir(v.DiskPath), fmt.Sprintf("disk%d.img", d.Index))
	}
}

// runIncomingResume drives the target-side live-migration resume: it dials the
// paused -incoming qemu and runs RunLiveTarget (wait incoming completed -> drop
// every writable export -> cont). On success the guest is running from the
// transferred RAM: flip the VM to StatusRunning, mark the record completed at
// cont success (closing the post-cont cancel window), then announce/GARP the new
// location, persist meta, attach the serial mux so the resumed console persists,
// and release the migration port pair. On failure there is no fall-back to the
// source (it is already released): mark the VM StatusFailed and the record failed.
func (m *Manager) runIncomingResume(ctx context.Context, taskID, migrationID, vmID uuid.UUID, ram, nbd int) {
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.Get(vmID)
	if err != nil {
		m.failIncomingResume(taskID, migrationID, vmID, fmt.Sprintf("vm %s: %v", vmID, err))
		return
	}

	// The per-disk export ids were persisted on the record at startIncomingLive.
	// Fall back to the boot export alone if the record is missing or carries
	// none, so an unexpectedly-empty record still tears down the boot export
	// rather than leaking it.
	exportIDs := []string{liveExportID}
	if rec, ok := m.migrations.Get(migrationID); ok && len(rec.ExportIDs) > 0 {
		exportIDs = rec.ExportIDs
	}

	conn, err := m.migDialQMPTarget(v.QMPSocket)
	if err != nil {
		m.failIncomingResume(taskID, migrationID, vmID, fmt.Sprintf("dial target qmp: %v", err))
		return
	}
	defer func() { _ = conn.Close() }()

	if err := m.migRunLiveTarget(ctx, conn, qemu.LiveTargetSpec{ExportIDs: exportIDs}); err != nil {
		m.failIncomingResume(taskID, migrationID, vmID, fmt.Sprintf("resume: %v", err))
		return
	}

	m.transitionVM(v.ID, StatusRunning, "")

	// Stamp the record completed at cont success: the guest is now running from
	// the transferred RAM, so the migration IS done - the steps below
	// (announce/GARP/persist/mux) are best-effort decorations. Marking terminal
	// here closes the post-cont cancel window: a CancelMigration arriving after
	// the guest resumed finds Terminal()==true and cancelLive no-ops, instead of
	// taking the pre-cutover reap arm that would kill the now-live guest.
	m.migrations.Update(migrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseCompleted
		r.CompletedAt = time.Now().UTC()
	})

	// Tell the L2 segment the guest moved here: QEMU self-announce (RARP, MAC-only,
	// IP-independent) so a physical switch / learning bridge relearns the port on
	// this node. The PRIMARY cutover for type=bridge NICs; harmless belt-and-
	// suspenders for overlay (whose authoritative convergence is the CP FDB
	// fast-push). Best-effort - a failure degrades to the switch's MAC-aging
	// timeout and must not fail the resume (the guest is already running).
	if err := conn.AnnounceSelf(qemu.AnnounceParameters{Initial: 50, Max: 550, Rounds: 5, Step: 100}); err != nil {
		m.log.WarnContext(ctx, "post-resume announce-self failed",
			slog.String("vm", v.ID.String()), slog.String("error", err.Error()))
	}

	// Announce the VM's new location to the L2 segment: one gratuitous ARP per
	// NIC with an IPv4. Best-effort - a failure degrades to FDB/heartbeat
	// convergence and must not fail the resume (the guest is already running).
	for _, nic := range v.NICs {
		if !nic.IPv4.IsValid() {
			continue
		}
		if err := m.fabric.SendGARP(nic.Bridge, nic.MAC, nic.IPv4); err != nil {
			m.log.WarnContext(ctx, "post-resume gratuitous ARP failed",
				slog.String("vm", v.ID.String()), slog.String("bridge", nic.Bridge), slog.String("error", err.Error()))
		}
	}

	if persistErr := m.persistVM(v.ID); persistErr != nil {
		m.log.Warn("incoming resume: persist meta failed", "vm", v.Name, "err", persistErr)
	}
	if err := m.attachMux(m.log, v); err != nil {
		m.log.Warn("incoming resume: attach mux failed", "vm", v.Name, "err", err)
	}
	m.migPorts.ReleasePair(ram, nbd)
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
}

// failIncomingResume handles a target-side resume failure (Outcome C). This path
// can be POST-cutover (a cont failure after the source already completed and the CP
// committed the cutover - where the source copy is gone and this disk is the ONLY
// copy), so it is conservative: it stops the leak (kill the incoming qemu, free the
// port pair) but NEVER deletes the destination disk or the VM record - it marks the
// VM StatusFailed. Recovery of a post-cutover failure (cold-start from the intact
// migrated disk) is the operator's / CP reconcile's job, per the existing
// no-fall-back-to-source terminal semantics.
//
// If the record is already terminal, a racing teardownIncomingTarget (the CP
// failure/cancel trigger) already reaped everything - killing the qemu closed the
// QMP socket and unblocked this resume goroutine into here - so only finalize the
// tracking task and return, without touching the (already-reaped) VM / ports.
func (m *Manager) failIncomingResume(taskID, migrationID, vmID uuid.UUID, msg string) {
	m.log.Error("incoming resume failed",
		"vm_id", vmID.String(), "migration_id", migrationID.String(), "err", msg)

	if rec, ok := m.migrations.Get(migrationID); ok && rec.Terminal() {
		// Already reaped by a concurrent teardown; just finalize the task.
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{Code: "resume_failed", Message: msg}
		})
		return
	}

	// Stop the leak without destroying data: kill the incoming qemu + free the pair.
	var ram, nbd int
	if rec, ok := m.migrations.Get(migrationID); ok {
		ram, nbd = rec.Port, rec.NBDPort
	}
	if v, err := m.Get(vmID); err == nil {
		m.killQEMU(v)
		m.teardownNICs(v.NICs)
		// Defensive: close any serial mux so logs/console subscribers see
		// Done() instead of hanging on a dead qemu. Today this path is always
		// reached BEFORE the target attaches its mux (attachMux runs only on
		// resume success), so this is a no-op; it guards the implicit
		// pre-attachMux invariant against a future change that streams during
		// resume. Idempotent - a no-op when no mux is registered. Mirrors the
		// teardownDepartedSource fix on the source side.
		m.detachMux(v.Name)
	}
	m.transitionVM(vmID, StatusFailed, msg)
	_ = m.persistVM(vmID)
	// Stamp terminal and release the pair ONLY if this call WINS the transition.
	// The top Terminal() fast-path narrows but does NOT close the race with
	// teardownIncomingTarget (this goroutine can pass it before the teardown
	// reaches terminal), so gate the release on the won flag captured under the
	// store mutex - exactly as teardownIncomingTarget does. A loser leaks the pair
	// (recoverable) rather than freeing a second migration's re-reserved port.
	won := false
	m.migrations.Update(migrationID, func(r *migration.Record) {
		if r.Terminal() {
			return
		}
		won = true
		r.Phase = migration.PhaseFailed
		r.ErrorMessage = msg
		r.CompletedAt = time.Now().UTC()
	})
	if won && (ram > 0 || nbd > 0) {
		m.migPorts.ReleasePair(ram, nbd)
	}
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{Code: "resume_failed", Message: msg}
	})
}

// runOutgoingLive drives the SOURCE side of a live migration asynchronously.
// Unlike the offline push it does NOT power off the guest: a live migration
// moves the running guest, finalizing the disk mirror while paused at
// pre-switchover and only then releasing the guest to the target (the order
// RunLiveSource enforces). On success the guest now runs on the target; on any
// failure RunLiveSource aborts both the RAM stream and the disk mirror and the
// guest stays running on the source.
func (m *Manager) runOutgoingLive(ctx context.Context, taskID uuid.UUID, s OutgoingSpec) {
	fail := func(code, msg string) {
		m.migrations.Update(s.MigrationID, func(r *migration.Record) {
			r.Phase = migration.PhaseFailed
			r.ErrorMessage = msg
			r.CompletedAt = time.Now().UTC()
		})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{Code: code, Message: msg}
		})
	}

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.Get(s.VMUUID)
	if err != nil {
		fail("internal", fmt.Sprintf("vm %s: %v", s.VMUUID, err))
		return
	}
	m.migrations.Update(s.MigrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseActive
		r.StartedAt = time.Now().UTC()
		r.BlockJobID = liveMirrorJobID
	})

	credsDir := filepath.Join(m.stateDir, "migrations", s.MigrationID.String(), "tls")
	if err := qemu.MaterializeMigrationCreds(credsDir, qemu.MigrationTLSClient, m.tlsCA, m.tlsCert, m.tlsKey); err != nil {
		fail("tls_setup_failed", err.Error())
		return
	}
	m.migrations.Update(s.MigrationID, func(r *migration.Record) { r.CredsDir = credsDir })

	ramHost, ramPortStr, err := net.SplitHostPort(s.TargetEndpoint)
	if err != nil {
		fail("internal", fmt.Sprintf("bad target endpoint %q: %v", s.TargetEndpoint, err))
		return
	}
	ramPort, _ := strconv.Atoi(ramPortStr)
	nbdHost, nbdPortStr, err := net.SplitHostPort(s.NBDEndpoint)
	if err != nil {
		fail("internal", fmt.Sprintf("bad nbd endpoint %q: %v", s.NBDEndpoint, err))
		return
	}
	nbdPort, _ := strconv.Atoi(nbdPortStr)

	conn, err := m.migDialQMP(v.QMPSocket)
	if err != nil {
		fail("qmp_unavailable", fmt.Sprintf("dial source qmp: %v", err))
		return
	}
	// The manager owns the dialed conn; close the monitoring socket on return
	// (closing it does not affect the running guest). RunLiveSource borrows it
	// but never owns it.
	defer func() { _ = conn.Close() }()

	// Mirror only the WRITABLE source disk(s) to the matching target export.
	// Index 0 is the boot disk (virtio0). The read-only cidata disk is NOT
	// transferred over NBD: the target rebuilds it read-only locally and
	// attaches it (see migBuildCidata on the target side), so the source has
	// nothing to mirror for it. Today the boot disk is the only writable disk;
	// future writable data disks would be appended here, each with their own
	// virtioN/mirror-diskN/mirror-targetN/<token>-N entry (read-only false).
	// The boot export name s.AuthToken+"-0" byte-matches the target's
	// <migrationID>-0 because the target set its record.AuthToken =
	// migrationID.String() and the CP relays it as the outgoing auth_token.
	disks := []qemu.LiveSourceDisk{{
		SrcNode: liveSourceDiskNode, JobID: liveMirrorJobID,
		NBDNode: "mirror-target0", Export: s.AuthToken + "-0",
	}}

	spec := qemu.LiveSourceSpec{
		Disks:                disks,
		TargetHost:           nbdHost,
		NBDPort:              nbdPort,
		CredsDir:             credsDir,
		TargetIdentity:       s.TargetIdentity,
		RAMHost:              ramHost,
		RAMPort:              ramPort,
		DowntimeMs:           liveDefaultDowntimeMs,
		ConvergenceTimeout:   m.migConvergenceTimeout,
		ProgressPollInterval: 2 * time.Second,
	}

	runErr := m.migRunLiveSource(ctx, conn, spec, m.liveProgressReporter(s.MigrationID))
	if runErr != nil {
		fail("convergence_failed", fmt.Sprintf("live migration: %v", runErr))
		return
	}

	// Fail-safe defense-in-depth before the IRREVERSIBLE source teardown: the
	// source qemu is the ONLY copy of the VM until the target takes over, so a
	// false completion (a RunLiveSource completion-detection bug - it keys on the
	// QMP MIGRATION status EVENT) would force-kill the only copy and LOSE the VM.
	// Independently confirm convergence with a direct query-migrate on the still-
	// open source conn. If it is not "completed" (or the query errors), DO NOT
	// tear down the source: fail the migration so the source qemu + VM SURVIVE
	// (fail-safe-to-source; the VM is never lost). A healthy migration is
	// "completed" here (the common case) and is unaffected.
	info, qerr := conn.QueryMigrate()
	if qerr != nil || info.Status != "completed" {
		fail("convergence_failed", fmt.Sprintf("post-migration query-migrate not 'completed' (status=%q, err=%v); keeping source qemu (never destroy the only copy)", info.Status, qerr))
		return
	}

	// The guest is now running on the target. Tear down the departed source VM
	// NOW (before reporting success) so a reverse migration back to this node
	// adopts cleanly instead of racing the CP's slow async DeleteVMOnSource.
	m.teardownDepartedSource(v)

	var diskTransferred, diskTotal int64
	m.migrations.Update(s.MigrationID, func(r *migration.Record) {
		diskTransferred, diskTotal = r.DiskBytesTransferred, r.DiskBytesTotal
		r.Phase = migration.PhaseCompleted
		r.CompletedAt = time.Now().UTC()
	})

	// Final live-migration stats ride the task Result back to the CP. Fail-open:
	// a marshal failure leaves Result unset (the CP treats absent stats as "none")
	// and NEVER blocks the success transition - stats are observability only.
	result, merr := json.Marshal(map[string]any{
		"migration_id": s.MigrationID.String(),
		"stats":        buildLiveMigrationStats(info, diskTransferred, diskTotal),
	})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		if merr == nil {
			t.Result = result
		}
	})
}

// teardownDepartedSource removes a source VM that has just completed a successful
// outgoing LIVE migration: the guest is now irrevocably on the target, so the
// postmigrate source qemu and the stale source disk are garbage. It force-kills the
// qemu (no graceful wait - the postmigrate guest will never honour ACPI powerdown),
// tears down the source NICs, and removes the in-memory record + per-VM disk dir +
// state. Done synchronously at the end of runOutgoingLive (before the task is marked
// success) so that "migration completed" implies "source released" - a reverse
// migration back to this node then adopts cleanly instead of racing the CP's slow
// async DeleteVMOnSource. Safe pre-cutover: the heartbeat path is purely additive
// (omitting the VM is a no-op) and the cutover Txn is the sole writer of
// current_node_id; a completed live migrate is the same signal the CP already trusts
// to delete the source.
func (m *Manager) teardownDepartedSource(v *VM) {
	if v == nil {
		return
	}
	m.killQEMU(v)
	m.teardownNICs(v.NICs)
	// Close the serial multiplexer so every attached logs/console subscriber
	// sees Done(). That Done is what makes a streaming /logs handler return,
	// which is the upstream break the CP-side follow relay reattaches on - so a
	// `vm logs -f` follows the guest to the target instead of hanging on the
	// dead source mux. Also releases the pump goroutine + log file handle that
	// would otherwise leak on every live migration. Before removeAdoptedVM so
	// the log file is closed before its state dir is removed.
	m.detachMux(v.Name)
	m.removeAdoptedVM(v.ID)
}

// liveProgressReporter returns the periodic LiveProgress callback handed to
// RunLiveSource. Each tick it (1) logs the full disk-mirror + RAM detail at
// INFO so an operator (and we) can SEE exactly where a migration is stuck, and
// (2) updates the coarse bytes-progress on the migration record so
// `migration get` reflects it. During the DISK phase (no/empty RAM-migrate
// status, a mirror block job present) the bytes come from the boot disk's
// block job offset/len; once the RAM phase has started (migrate status
// non-empty and not "none") they come from the query-migrate RAM counters.
//
// NOTE: the reporter surfaces only the coarse bytes-progress on the record
// plus the full INFO-log breakdown. The FINAL query-migrate statistics are
// captured at completion in runOutgoingLive and surfaced through the CP
// `migration get` stats section.
func (m *Manager) liveProgressReporter(migrationID uuid.UUID) func(qemu.LiveProgress) {
	return func(p qemu.LiveProgress) {
		bootJob, hasBootJob := liveBootDiskJob(p.BlockJobs)
		ramStarted := p.Migrate.Status != "" && p.Migrate.Status != "none"

		log := m.log.With("migration_id", migrationID.String())
		if hasBootJob {
			log = log.With(
				"block_job_device", bootJob.Device,
				"block_job_offset", bootJob.Offset,
				"block_job_len", bootJob.Len,
				"block_job_ready", bootJob.Ready,
				"block_job_status", bootJob.Status,
			)
		} else {
			log = log.With("block_jobs", len(p.BlockJobs))
		}
		log.Info("live migration progress",
			"migrate_status", p.Migrate.Status,
			"ram_remaining", p.Migrate.RAM.Remaining,
			"ram_transferred", p.Migrate.RAM.Transferred,
			"ram_total", p.Migrate.RAM.Total,
			"ram_dirty_pages_rate", p.Migrate.RAM.DirtyPagesRate,
		)

		m.migrations.Update(migrationID, func(r *migration.Record) {
			r.DiskBytesTotal, r.DiskBytesTransferred = peakDiskBytes(r.DiskBytesTotal, r.DiskBytesTransferred, p.BlockJobs)
			switch {
			case ramStarted:
				r.BytesTotal = p.Migrate.RAM.Total
				r.BytesTransferred = p.Migrate.RAM.Transferred
			case hasBootJob:
				r.BytesTotal = bootJob.Len
				r.BytesTransferred = bootJob.Offset
			}
		})
	}
}

// peakDiskBytes folds one live-progress block-job snapshot into the running
// peak disk totals. Maxima are tracked independently because a finished mirror
// job disappears from query-block-jobs: len gives the device size (constant
// while the job exists), offset converges up to len. At full mirror both equal
// the summed disk virtual size.
func peakDiskBytes(total, transferred int64, jobs []qemu.BlockJobInfo) (int64, int64) {
	var sumLen, sumOff int64
	for _, j := range jobs {
		sumLen += j.Len
		sumOff += j.Offset
	}
	if sumLen > total {
		total = sumLen
	}
	if sumOff > transferred {
		transferred = sumOff
	}
	return total, transferred
}

// buildLiveMigrationStats assembles the final live-migration stats payload
// attached to the outgoing-live task Result on success. The JSON keys are the
// locked wire contract the CP commitCutover parses; do not rename without
// updating internal/api/handlers/migrations/run.go and the control-plane spec.
func buildLiveMigrationStats(info qemu.MigrateInfo, diskTransferred, diskTotal int64) map[string]any {
	return map[string]any{
		"ram": map[string]any{
			"transferred":      info.RAM.Transferred,
			"total":            info.RAM.Total,
			"dirty_pages_rate": info.RAM.DirtyPagesRate,
		},
		"disk": map[string]any{
			"transferred": diskTransferred,
			"total":       diskTotal,
		},
		"total_time_ms": info.TotalTimeMs,
		"downtime_ms":   info.DowntimeMs,
		"setup_time_ms": info.SetupTimeMs,
	}
}

// liveBootDiskJob returns the block job mirroring the boot disk
// (Device == liveSourceDiskNode) and whether it was found. The reporter keys
// disk-phase progress on this entry; an absent entry means the mirror has not
// started (or has finalized) and the reporter falls back to logging the raw
// count.
func liveBootDiskJob(jobs []qemu.BlockJobInfo) (qemu.BlockJobInfo, bool) {
	for _, j := range jobs {
		if j.Device == liveSourceDiskNode {
			return j, true
		}
	}
	return qemu.BlockJobInfo{}, false
}

// teardownIncomingTarget fully reaps a target-side live-migration incoming setup
// at a PRE-CUTOVER terminal outcome (failure or cancel), where the real guest is
// still safe on the source. Order is load-bearing: kill the incoming qemu FIRST
// (which drops its NBD server, exports, TLS objects, and PORT BINDINGS with the
// process), then release the pair - releasing before the kill would hand a still-
// bound port back to the allocator (the "address already in use" bug). It then
// removes the adopted VM + its destination disk dir (safe: pre-cutover the disk is
// empty / not the live copy) and reaps the migration record. Idempotent: a missing
// VM, unknown ports, and an already-terminal record are all no-ops, so it is safe
// to call twice (the CP trigger racing the resume goroutine).
func (m *Manager) teardownIncomingTarget(migrationID, vmID uuid.UUID, ram, nbd int, phase migration.Phase, reason string) {
	if v, err := m.Get(vmID); err == nil {
		m.killQEMU(v)
		m.teardownNICs(v.NICs)
		m.removeAdoptedVM(vmID)
	}
	// Release the port pair ONLY if this call WINS the non-terminal->terminal
	// transition of the record. The store applies the closure under its mutex, so
	// exactly one finalizer (this teardown or a racing failIncomingResume) both
	// stamps terminal AND releases the ports. This closes the ABA double-release:
	// a stale finalizer (cancelLive passes a stale snapshot) would otherwise
	// unconditionally free a pair a later migration has already re-reserved, giving
	// two live incoming qemus the same RAM/NBD port. Fails toward inaction: the
	// loser leaks the pair (recoverable - the pair is re-derivable and the range
	// self-heals on reap) rather than wrongly freeing a second migration's live
	// port. killQEMU above already dropped the OS-level port binding with the
	// process, so a leaked allocator entry is the only cost of losing.
	won := false
	m.migrations.Update(migrationID, func(r *migration.Record) {
		if r.Terminal() {
			return
		}
		won = true
		r.Phase = phase
		r.ErrorMessage = reason
		r.CompletedAt = time.Now().UTC()
	})
	if won {
		m.migPorts.ReleasePair(ram, nbd)
	}
}

// cancelLive aborts a pre-cutover live migration best-effort: cancel the RAM
// migration and the disk mirror on the source guest, release the reserved
// ports, and pin phase=cancelled. Idempotent; a terminal record is returned
// unchanged. A failure to obtain a QMP conn does not block the bookkeeping -
// the ports are still released and the phase still set so the slot frees.
func (m *Manager) cancelLive(id uuid.UUID, rec migration.Record) (MigrationView, bool) {
	if rec.Terminal() {
		return m.GetMigration(id)
	}

	if rec.Role == migration.RoleTarget {
		// Defense-in-depth on the TRUE invariant ("never reap a running guest"):
		// CancelMigration passes a STALE record snapshot, and the P1b stamp-move
		// only narrows the window where that snapshot's Terminal() is false while
		// the resume goroutine has already cont'd the guest. Read the IN-MEMORY
		// status (not m.Get, which probes the pidfile and would misreport under
		// fakes / supervision races) and refuse the reap when the incoming guest is
		// already StatusRunning - the cont happened, so this is a post-cutover guest
		// whose dest disk may be the only copy. Fix Risk Gate: killing it is
		// irreversible; a no-op refusal of a wrongly-timed cancel is recoverable via
		// normal VM lifecycle. This does NOT block a legitimate pre-cont cancel: an
		// adopted incoming VM sits at StatusMigratingIncoming (paused) until
		// transitionVM flips it to StatusRunning AFTER cont, so a real pre-cutover
		// target is never StatusRunning here.
		//
		// KNOWN RESIDUAL (accepted, follow-up): this reads the CACHED status, which
		// lags the qemu cont by a sub-microsecond, no-I/O window in runIncomingResume
		// (cont inside migRunLiveTarget -> transitionVM(StatusRunning)). A cancel that
		// executes this read in that exact gap sees StatusMigratingIncoming and reaps a
		// guest qemu has already resumed - theoretical VM loss if the source already
		// self-destroyed. Astronomically narrow and PRE-EXISTING (not the CP-side
		// auto-complete-vs-cancel race, which is closed at the control plane). To fully
		// eliminate, make this guard read the LIVE qemu run-state (query-status) rather
		// than the cached status.
		if st, ok := m.vmStatus(rec.VMID); ok && st == StatusRunning {
			return m.GetMigration(id)
		}
		// Full pre-cutover reap: kill the incoming qemu, free the pair, remove the
		// adopted VM, reap the record. (The previous body released the ports while
		// the incoming qemu still held them - the "address already in use" leak.)
		m.teardownIncomingTarget(id, rec.VMID, rec.Port, rec.NBDPort, migration.PhaseCancelled, "cancelled")
		return m.GetMigration(id)
	}

	// Source: best-effort QMP abort of the live mirror + RAM stream; the guest
	// stays running on this node (fail-safe). object-del migtls is handled by
	// RunLiveSource's deferred cleanup when the in-flight run unwinds.
	if v, err := m.Get(rec.VMID); err == nil {
		if conn, err := m.migDialQMP(v.QMPSocket); err == nil {
			_ = conn.MigrateCancel()
			if rec.BlockJobID != "" {
				_ = conn.BlockJobCancel(rec.BlockJobID, true)
			}
			_ = conn.Close()
		}
	}
	if rec.Port > 0 || rec.NBDPort > 0 {
		m.migPorts.ReleasePair(rec.Port, rec.NBDPort)
	}
	m.migrations.Update(id, func(r *migration.Record) {
		r.Phase = migration.PhaseCancelled
		r.ErrorMessage = "cancelled"
		r.CompletedAt = time.Now().UTC()
	})
	return m.GetMigration(id)
}

// launchIncomingQemu is the production migLaunchIncoming: it boots a paused
// -incoming qemu for the adopted target VM v, booting the node-named
// destination disks (so SetupLiveIncoming can NBD-export each one) instead of
// the base `-drive`, and returns once the QMP socket answers. The base args
// omit the OS disk drive (OmitBootDisk) to avoid expressing the boot disk
// twice; the per-disk node-named blockdevs + virtio-blk devices from
// LiveIncomingArgs supply every disk. SMOKE MUST VALIDATE the resulting cmdline
// boots and the exports + boot devices agree on the "virtio<i>" nodes.
func (m *Manager) launchIncomingQemu(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error {
	binary, err := qemu.Binary(v.Architecture)
	if err != nil {
		return fmt.Errorf("qemu binary: %v", err)
	}
	args, err := qemu.BuildArgs(qemu.VMSpec{
		Name:            v.Name,
		UUID:            v.ID,
		VCPUs:           v.VCPUs,
		MemoryMib:       v.MemoryMib,
		Architecture:    v.Architecture,
		Accelerator:     m.accelerator,
		DiskPath:        v.DiskPath,
		QMPSocket:       v.QMPSocket,
		ConsoleSocket:   v.ConsoleSocket,
		PIDFile:         v.PIDFile,
		AArch64Firmware: m.aarch64Firmware,
		NICs:            v.NICs,
		OmitBootDisk:    true,
	})
	if err != nil {
		return fmt.Errorf("build qemu args: %v", err)
	}
	// Append every migration disk as a node-named blockdev + virtio-blk
	// device, and -incoming defer. LiveIncomingArgs supplies the blockdevs,
	// the devices, and -incoming for every disk in the spec.
	args = append(args, qemu.LiveIncomingArgs(ls)...)

	spawnCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := qemu.Spawn(spawnCtx, binary, args); err != nil {
		return fmt.Errorf("spawn incoming qemu: %v", err)
	}

	// Wait until QMP answers so SetupLiveIncoming can issue commands.
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, derr := qemu.DialQMP(v.QMPSocket, 2*time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("incoming qemu qmp not reachable: %v", derr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
