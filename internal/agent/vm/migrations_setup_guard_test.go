// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/qemu"
)

// incomingSetupCase parameterizes the two incoming paths over the same
// assertions. blockAt installs a seam that parks inside setup - after the VM is
// adopted, before the migration record is published - so the guard can be probed
// while setup is genuinely mid-flight rather than by calling into it directly.
type incomingSetupCase struct {
	name    string
	mode    string
	blockAt func(m *Manager, entered chan<- struct{}, release <-chan struct{})
}

func incomingSetupCases() []incomingSetupCase {
	return []incomingSetupCase{
		{
			name: "offline",
			mode: "offline",
			// migSpawnNBD runs after AdoptForMigration and the destination disk.
			blockAt: func(m *Manager, entered chan<- struct{}, release <-chan struct{}) {
				m.migCreateDisk = func(context.Context, string, int64) error { return nil }
				m.migSpawnNBD = func(context.Context, []string) (int, error) {
					entered <- struct{}{}
					<-release
					return 4321, nil
				}
			},
		},
		{
			name: "live",
			mode: "live",
			// migLaunchIncoming runs after AdoptForMigration, disk replication and
			// NIC materialisation.
			blockAt: func(m *Manager, entered chan<- struct{}, release <-chan struct{}) {
				m.migCreateDisk = func(context.Context, string, int64) error { return nil }
				m.migDialQMP = func(string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
				m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error {
					entered <- struct{}{}
					<-release
					return nil
				}
			},
		},
	}
}

// startIncomingSpec builds a minimal valid IncomingSpec for the given mode.
func startIncomingSpec(m *Manager, migID, vmID uuid.UUID, name, mode string) IncomingSpec {
	return IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmID,
		VMName:         name,
		VCPUs:          1,
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           mode,
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	}
}

// TestStartIncomingRefusesTeardownDuringSetup is the invariant this change
// exists for. Between AdoptForMigration and the migration record's publication
// the VM is in m.vms - so the reconciler observes it and a control-plane
// tombstone can target it - while HasActiveForVM is still false and the observed
// status is tearable (StatusStopped for an offline target; a live target's
// StatusMigratingIncoming downgrades to StatusFailed while its -incoming qemu has
// not launched). Holding the per-name lifecycle slot for the whole of setup is
// what closes that window.
//
// The assertion is on Manager.Delete - the real destructive entry point - not on
// the reconciler's HasInFlight probe, so a change that keeps the flag set but
// stops Delete honouring it still fails here.
func TestStartIncomingRefusesTeardownDuringSetup(t *testing.T) {
	for _, tc := range incomingSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			tc.blockAt(m, entered, release)

			migID, vmID := uuid.New(), uuid.New()
			const vmName = "demo"
			done := make(chan error, 1)
			go func() {
				_, err := m.StartIncoming(context.Background(), startIncomingSpec(m, migID, vmID, vmName, tc.mode))
				done <- err
			}()

			<-entered // setup is parked, past the adopt

			if _, err := m.Get(vmID); err != nil {
				t.Fatalf("adopted vm absent mid-setup: %v", err)
			}
			if !m.HasInFlight(vmName) {
				t.Errorf("HasInFlight(%q) = false mid-setup, want true", vmName)
			}
			if _, err := m.Delete(context.Background(), vmID); !errors.Is(err, ErrInFlight) {
				t.Errorf("Delete mid-setup = %v, want ErrInFlight", err)
			}
			if _, err := m.Get(vmID); err != nil {
				t.Errorf("vm torn down mid-setup: %v", err)
			}

			close(release)
			if err := <-done; err != nil {
				t.Fatalf("StartIncoming(%s) = %v, want nil", tc.mode, err)
			}
		})
	}
}

// TestStartIncomingHandsOffToTheMigrationRecord pins the handoff. The slot is
// released when StartIncoming returns; the migration record must already be
// published and non-terminal at that instant, or the window merely moves rather
// than closing. Asserted after the return, when both facts are observable.
func TestStartIncomingHandsOffToTheMigrationRecord(t *testing.T) {
	for _, tc := range incomingSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			close(release) // do not park; run straight through
			tc.blockAt(m, entered, release)

			migID, vmID := uuid.New(), uuid.New()
			const vmName = "demo"
			if _, err := m.StartIncoming(context.Background(), startIncomingSpec(m, migID, vmID, vmName, tc.mode)); err != nil {
				t.Fatalf("StartIncoming(%s) = %v, want nil", tc.mode, err)
			}

			if m.HasInFlight(vmName) {
				t.Errorf("HasInFlight(%q) = true after a successful return; the slot leaked", vmName)
			}
			rec, ok := m.Migrations().Get(migID)
			if !ok {
				t.Fatalf("migration record absent after a successful return")
			}
			if rec.Terminal() {
				t.Errorf("record phase = %q, want non-terminal so HasActiveForVM guards from here", rec.Phase)
			}
			if !m.Migrations().HasActiveForVM(vmID) {
				t.Errorf("HasActiveForVM(%s) = false after the slot was released; the window is not closed", vmID)
			}
		})
	}
}

// TestStartIncomingReleasesSlotOnFailure covers the rollback. Every pre-success
// error arm must leave the slot free, or one failed migration makes its VM name
// permanently un-operable for the life of the agent process - the failure mode
// that ruled out publishing a provisional record from each cleanup() call site.
func TestStartIncomingReleasesSlotOnFailure(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range incomingSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			close(release)
			tc.blockAt(m, entered, release)
			// Fail the parked seam instead of completing it.
			switch tc.mode {
			case "offline":
				m.migSpawnNBD = func(context.Context, []string) (int, error) { return 0, boom }
			default:
				m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error { return boom }
			}

			const vmName = "demo"
			_, err := m.StartIncoming(context.Background(), startIncomingSpec(m, uuid.New(), uuid.New(), vmName, tc.mode))
			if err == nil {
				t.Fatalf("StartIncoming(%s) = nil, want the injected failure", tc.mode)
			}
			if m.HasInFlight(vmName) {
				t.Errorf("HasInFlight(%q) = true after a failed setup; the slot leaked", vmName)
			}
		})
	}
}

// TestStartIncomingRedeliveryDuringSetupIsRefused pins the redelivery case. A
// re-driven task arriving while the first attempt is still in setup must be
// refused as a retryable conflict without disturbing that attempt - it must not
// reserve a second port, adopt over the in-flight VM, or return endpoints the
// first attempt has not minted yet.
func TestStartIncomingRedeliveryDuringSetupIsRefused(t *testing.T) {
	for _, tc := range incomingSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			tc.blockAt(m, entered, release)

			migID, vmID := uuid.New(), uuid.New()
			const vmName = "demo"
			done := make(chan error, 1)
			go func() {
				_, err := m.StartIncoming(context.Background(), startIncomingSpec(m, migID, vmID, vmName, tc.mode))
				done <- err
			}()
			<-entered

			res, err := m.StartIncoming(context.Background(), startIncomingSpec(m, migID, vmID, vmName, tc.mode))
			if !errors.Is(err, ErrInFlight) {
				t.Errorf("redelivered StartIncoming = (%+v, %v), want ErrInFlight", res, err)
			}
			if res.ListenEndpoint != "" {
				t.Errorf("redelivery returned endpoint %q before the first attempt minted one", res.ListenEndpoint)
			}

			close(release)
			if err := <-done; err != nil {
				t.Fatalf("first StartIncoming(%s) = %v, want nil", tc.mode, err)
			}
		})
	}
}

// TestStartIncomingRejectsEmptyVMName guards the one way the slot fails open:
// inFlightAcquire("") returns a no-op release and ok=true, so an empty name would
// silently acquire nothing and leave the teardown window wide open.
func TestStartIncomingRejectsEmptyVMName(t *testing.T) {
	for _, tc := range incomingSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			close(release)
			tc.blockAt(m, entered, release)

			_, err := m.StartIncoming(context.Background(), startIncomingSpec(m, uuid.New(), uuid.New(), "", tc.mode))
			if err == nil {
				t.Fatalf("StartIncoming(%s) with an empty vm name = nil, want an error", tc.mode)
			}
		})
	}
}
