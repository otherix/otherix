// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"fmt"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/state"
)

// materialiseNICs creates the host tap for each NIC and enslaves it to
// its bridge, in slice order. The bridge is assumed to exist already -
// bridge creation belongs to the network reconciler, not the VM create
// path. On the first failure it best-effort deletes every tap created so
// far this call (to avoid leaking host devices) and returns the error.
func (m *Manager) materialiseNICs(nics []netfabric.NIC) error {
	var created []netfabric.NIC
	for _, nic := range nics {
		tap := nic.TapName()
		if err := m.fabric.CreateTap(tap, nic.MTU); err != nil {
			m.teardownNICs(created)
			return fmt.Errorf("create tap %s: %v", tap, err)
		}
		created = append(created, nic)
		if err := m.fabric.AttachTap(tap, nic.Bridge); err != nil {
			m.teardownNICs(created)
			return fmt.Errorf("attach tap %s to bridge %s: %v", tap, nic.Bridge, err)
		}
	}
	return nil
}

// teardownNICs best-effort deletes the host tap of each NIC. Errors are
// logged at warn and never abort the caller - delete must make progress
// even when a tap is already gone.
func (m *Manager) teardownNICs(nics []netfabric.NIC) {
	for _, nic := range nics {
		tap := nic.TapName()
		if err := m.fabric.DeleteTap(tap); err != nil {
			m.log.Warn("delete tap", "tap", tap, "err", err)
		}
	}
}

// nicsToMeta projects the in-memory NICs to their persisted form,
// recording the resolved host tap name so teardown can act on it after a
// restart without re-deriving the convention.
func nicsToMeta(nics []netfabric.NIC) []state.NICMeta {
	if len(nics) == 0 {
		return nil
	}
	out := make([]state.NICMeta, 0, len(nics))
	for _, nic := range nics {
		out = append(out, state.NICMeta{
			ID:          nic.ID,
			Bridge:      nic.Bridge,
			MAC:         nic.MAC,
			Model:       nic.Model,
			MTU:         nic.MTU,
			DeviceOrder: nic.DeviceOrder,
			TapName:     nic.TapName(),
		})
	}
	return out
}

// metaToNICs reconstructs the in-memory NICs from persisted meta on
// startup replay. The TapName field is derived from the NIC id, so it is
// not carried back into the NIC struct.
func metaToNICs(metas []state.NICMeta) []netfabric.NIC {
	if len(metas) == 0 {
		return nil
	}
	out := make([]netfabric.NIC, 0, len(metas))
	for _, meta := range metas {
		out = append(out, netfabric.NIC{
			ID:          meta.ID,
			Bridge:      meta.Bridge,
			MAC:         meta.MAC,
			Model:       meta.Model,
			MTU:         meta.MTU,
			DeviceOrder: meta.DeviceOrder,
		})
	}
	return out
}
