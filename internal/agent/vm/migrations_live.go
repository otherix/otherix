// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"fmt"
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
const liveSourceDiskNode = "virtio0"

// liveExportID is the block-export-add handle for the boot (index 0)
// destination disk. The target replicates every manifest disk under the
// "target-disk<i>" node-name / "exp<i>" export-id convention and the resume
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
	cleanup := func() {
		if launched && adopted != nil {
			m.killQEMU(adopted)
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
		UUID: s.VMUUID, Name: s.VMName, VCPUs: s.VCPUs, MemoryMB: s.MemoryMB,
		PoolName: s.PoolName, Architecture: s.Architecture,
		InitialStatus: StatusMigratingIncoming,
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

	// Launch the paused -incoming qemu, then dial it and arm the incoming
	// transport (NBD export + RAM channel) over QMP. The launched qemu keeps
	// running; the setup conn is closed once setup completes (it only carries
	// the monitoring socket, not the qemu process).
	if err := m.migLaunchIncoming(ctx, v, ls); err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("launch incoming qemu: %v", err)
	}
	launched = true
	conn, err := m.migDialQMP(v.QMPSocket)
	if err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("dial target qmp: %v", err)
	}
	// Closing the QMP client closes only the monitoring socket, NOT the
	// launched qemu process, so it is safe to close after setup.
	defer func() { _ = conn.Close() }()
	if err := qemu.SetupLiveIncoming(conn, ls); err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("setup live incoming: %v", err)
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
	// #nosec G118 -- the target resume intentionally outlives the request (fire-and-converge); a cancelled HTTP request must not abort an in-flight guest resume. The CP re-drives on failure.
	go m.runIncomingResume(context.Background(), task.ID, s.MigrationID, v.ID, ram, nbd)

	return IncomingResult{ListenEndpoint: ramEndpoint, NBDEndpoint: nbdEndpoint, AuthToken: token}, nil
}

// replicateIncomingDisks materializes one destination disk per manifest entry,
// in boot-first index order, and returns the per-disk LiveIncomingSpec entries
// plus their export ids ("exp<i>"). Index 0 is the boot disk at v.DiskPath
// (sized max(SizeBytes, ExpectedSize) to preserve the under-size guard); a raw
// read-only entry is the cidata ISO (recorded on v.CidataPath so a later cold
// restart reproduces the device); any other i>=1 entry lands at disk<i>.img.
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

		// Destination disk virtual size must be >= the source's (see the
		// offline StartIncoming for the full rationale). The boot disk
		// (index 0) is sized max(d.SizeBytes, ExpectedSize) regardless of the
		// manifest size, preserving the offline under-size guard; every other
		// disk (e.g. the cidata ISO at index 1) keeps its exact manifest size
		// and is never inflated by ExpectedSize.
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
			Node:     fmt.Sprintf("target-disk%d", d.Index),
			Path:     path,
			Export:   fmt.Sprintf("%s-%d", token, d.Index),
			ExportID: fmt.Sprintf("exp%d", d.Index),
			Format:   d.Format,
			ReadOnly: d.ReadOnly,
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
// transferred RAM: flip the VM to StatusRunning, attach the serial mux so the
// resumed console persists, release the migration port pair, and mark the
// record completed. On failure there is no fall-back to the source (it is
// already released): mark the VM StatusFailed and the record failed.
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
	if persistErr := m.persistVM(v.ID); persistErr != nil {
		m.log.Warn("incoming resume: persist meta failed", "vm", v.Name, "err", persistErr)
	}
	if err := m.attachMux(m.log, v); err != nil {
		m.log.Warn("incoming resume: attach mux failed", "vm", v.Name, "err", err)
	}
	m.migPorts.ReleasePair(ram, nbd)
	m.migrations.Update(migrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseCompleted
		r.CompletedAt = time.Now().UTC()
	})
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
}

// failIncomingResume marks the target VM failed (no source fall-back after the
// source has been released) and records the failure on the migration record and
// the tracking task.
func (m *Manager) failIncomingResume(taskID, migrationID, vmID uuid.UUID, msg string) {
	m.log.Error("incoming resume failed",
		"vm_id", vmID.String(), "migration_id", migrationID.String(), "err", msg)
	m.transitionVM(vmID, StatusFailed, msg)
	_ = m.persistVM(vmID)
	m.migrations.Update(migrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseFailed
		r.ErrorMessage = msg
		r.CompletedAt = time.Now().UTC()
	})
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

	// Mirror EVERY source disk to its matching target export so the device
	// topology matches the -incoming target. Index 0 is always the boot disk
	// (virtio0). Index 1, present iff the source carries a cidata disk, is the
	// cidata disk (virtio1): the source guest launches the boot drive then the
	// cidata drive in if=virtio cmdline order, so QEMU names them virtio0,
	// virtio1 by that ordering (see liveSourceDiskNode at the top of this file;
	// the same ordering extends to virtio1). The per-disk export name
	// s.AuthToken+"-<i>" byte-matches the target's <migrationID>-<i> because the
	// target set its record.AuthToken = migrationID.String() and the CP relays
	// it as the outgoing auth_token.
	disks := []qemu.LiveSourceDisk{{
		SrcNode: liveSourceDiskNode, JobID: liveMirrorJobID,
		NBDNode: "mirror-target0", Export: s.AuthToken + "-0",
	}}
	if v.CidataPath != "" {
		disks = append(disks, qemu.LiveSourceDisk{
			SrcNode: "virtio1", JobID: "mirror-disk1",
			NBDNode: "mirror-target1", Export: s.AuthToken + "-1",
		})
	}

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

	m.migrations.Update(s.MigrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseCompleted
		r.CompletedAt = time.Now().UTC()
	})
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
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
// NOTE: surfacing the FULL query-migrate breakdown through the CP
// `migration get` API (new Migration schema fields) is a deliberate follow-up,
// out of scope here - this puts the full detail in the AGENT LOG and the
// coarse bytes-progress on the existing record/API.
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

// cancelLive aborts a pre-cutover live migration best-effort: cancel the RAM
// migration and the disk mirror on the source guest, release the reserved
// ports, and pin phase=cancelled. Idempotent; a terminal record is returned
// unchanged. A failure to obtain a QMP conn does not block the bookkeeping -
// the ports are still released and the phase still set so the slot frees.
func (m *Manager) cancelLive(id uuid.UUID, rec migration.Record) (MigrationView, bool) {
	if rec.Terminal() {
		return m.GetMigration(id)
	}

	// Best-effort QMP abort. Only the source holds the live mirror + RAM
	// stream; the target side is reclaimed by the CP re-driving as failed.
	if rec.Role == migration.RoleSource {
		if v, err := m.Get(rec.VMID); err == nil {
			if conn, err := m.migDialQMP(v.QMPSocket); err == nil {
				_ = conn.MigrateCancel()
				if rec.BlockJobID != "" {
					_ = conn.BlockJobCancel(rec.BlockJobID, true)
				}
			}
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
// boots and the exports + boot devices agree on the "target-disk<i>" nodes.
func (m *Manager) launchIncomingQemu(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error {
	binary, err := qemu.Binary(v.Architecture)
	if err != nil {
		return fmt.Errorf("qemu binary: %v", err)
	}
	args, err := qemu.BuildArgs(qemu.VMSpec{
		Name:            v.Name,
		UUID:            v.ID,
		VCPUs:           v.VCPUs,
		MemoryMB:        v.MemoryMB,
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
