// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestVMCreate_CloudInit_VMUserDataOverridesTemplate confirms the L3
// CP-side resolver picks vm.user_data over template.cloud_init_user_data
// when both are set (Area 3 lock — full-replace override). End-to-end:
// seed а template с baked YAML, POST /v1/vms with own user_data, assert
// the agent-mock observes the VM-level blob, не the template's.
func TestVMCreate_CloudInit_VMUserDataOverridesTemplate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-ci-override", 0xb0, "private")

	templateBaked := "#cloud-config\nusers:\n  - name: from-template\n"
	if _, err := v.store.Queries().UpdateTemplate(ctx, store.UpdateTemplateParams{
		ID:                     tpl.ID,
		Name:                   tpl.Name,
		Description:            tpl.Description,
		OsVariant:              tpl.OsVariant,
		FirmwareType:           tpl.FirmwareType,
		FirmwareID:             tpl.FirmwareID,
		DefaultCpuCores:        tpl.DefaultCpuCores,
		DefaultMemoryMib:       tpl.DefaultMemoryMib,
		DefaultDiskGib:         tpl.DefaultDiskGib,
		CloudInitSupported:     tpl.CloudInitSupported,
		CloudInitUserData:      &templateBaked,
		QemuGuestAgentExpected: tpl.QemuGuestAgentExpected,
		Metadata:               tpl.Metadata,
	}); err != nil {
		t.Fatalf("UpdateTemplate bake cloud_init_user_data: %v", err)
	}

	vmOverride := "#cloud-config\nusers:\n  - name: from-vm\n"
	vmName := "ci-override-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
		UserData: &vmOverride,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	row, _ := v.store.Queries().GetTask(ctx, createTaskID)
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("create task status = %q, want success: %s", row.Status, row.Error)
	}

	stored, ok := v.mock.StoredVM(vmName)
	if !ok {
		t.Fatalf("mock.StoredVM(%q) = false; want materialised", vmName)
	}
	if stored.UserData != vmOverride {
		t.Errorf("mock-agent UserData = %q, want VM-level override %q (CP-side resolver picked template instead)", stored.UserData, vmOverride)
	}
}

// TestVMCreate_CloudInit_TemplateBakedFallback verifies the resolver
// falls back к template.cloud_init_user_data when vm.user_data is
// absent. Different template blob, no VM-level user_data → mock-agent
// observes the template's blob.
func TestVMCreate_CloudInit_TemplateBakedFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-ci-fallback", 0xb1, "private")

	templateBaked := "#cloud-config\npackages:\n  - htop\n"
	if _, err := v.store.Queries().UpdateTemplate(ctx, store.UpdateTemplateParams{
		ID:                     tpl.ID,
		Name:                   tpl.Name,
		Description:            tpl.Description,
		OsVariant:              tpl.OsVariant,
		FirmwareType:           tpl.FirmwareType,
		FirmwareID:             tpl.FirmwareID,
		DefaultCpuCores:        tpl.DefaultCpuCores,
		DefaultMemoryMib:       tpl.DefaultMemoryMib,
		DefaultDiskGib:         tpl.DefaultDiskGib,
		CloudInitSupported:     tpl.CloudInitSupported,
		CloudInitUserData:      &templateBaked,
		QemuGuestAgentExpected: tpl.QemuGuestAgentExpected,
		Metadata:               tpl.Metadata,
	}); err != nil {
		t.Fatalf("UpdateTemplate bake: %v", err)
	}

	vmName := "ci-fallback-vm-" + uuid.NewString()[:8]
	_, _ = v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	stored, ok := v.mock.StoredVM(vmName)
	if !ok {
		t.Fatalf("mock.StoredVM(%q) = false; want materialised", vmName)
	}
	if stored.UserData != templateBaked {
		t.Errorf("mock-agent UserData = %q, want template fallback %q", stored.UserData, templateBaked)
	}
}

// TestVMCreate_CloudInit_NeitherSourceLeavesAgentWithoutCidata verifies
// что когда both template AND VM omit user_data, the agent receives an
// empty UserData string — agent then skips cidata ISO generation
// (Manager.CreateSpec.UserData length-zero short-circuit).
func TestVMCreate_CloudInit_NeitherSourceLeavesAgentWithoutCidata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-ci-empty", 0xb2, "private")

	vmName := "ci-empty-vm-" + uuid.NewString()[:8]
	_, _ = v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	stored, ok := v.mock.StoredVM(vmName)
	if !ok {
		t.Fatalf("mock.StoredVM(%q) = false; want materialised", vmName)
	}
	if stored.UserData != "" {
		t.Errorf("mock-agent UserData = %q, want empty (no template baked, no vm override)", stored.UserData)
	}
}

// TestVMCreate_CloudInit_DisabledOverridesTemplate locks in the
// operator UX iteration's three-state contract: cloud_init_disabled=true
// wins over а template's baked cloud_init_user_data. End-to-end: seed
// а template с baked YAML, create а VM с cloud_init_disabled=true,
// assert the agent-mock observes empty UserData (no cidata.iso would
// be generated by а real agent).
func TestVMCreate_CloudInit_DisabledOverridesTemplate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-ci-disabled", 0xb3, "private")

	templateBaked := "#cloud-config\npackages:\n  - htop\n"
	if _, err := v.store.Queries().UpdateTemplate(ctx, store.UpdateTemplateParams{
		ID:                     tpl.ID,
		Name:                   tpl.Name,
		Description:            tpl.Description,
		OsVariant:              tpl.OsVariant,
		FirmwareType:           tpl.FirmwareType,
		FirmwareID:             tpl.FirmwareID,
		DefaultCpuCores:        tpl.DefaultCpuCores,
		DefaultMemoryMib:       tpl.DefaultMemoryMib,
		DefaultDiskGib:         tpl.DefaultDiskGib,
		CloudInitSupported:     tpl.CloudInitSupported,
		CloudInitUserData:      &templateBaked,
		QemuGuestAgentExpected: tpl.QemuGuestAgentExpected,
		Metadata:               tpl.Metadata,
	}); err != nil {
		t.Fatalf("UpdateTemplate bake: %v", err)
	}

	vmName := "ci-disabled-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:              vmName,
		Template:          tpl.Name,
		Pool:              v.pool.ID.String(),
		VCPUs:             1,
		MemoryMB:          512,
		CloudInitDisabled: true,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	row, _ := v.store.Queries().GetTask(ctx, createTaskID)
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("create task status = %q, want success: %s", row.Status, row.Error)
	}

	stored, ok := v.mock.StoredVM(vmName)
	if !ok {
		t.Fatalf("mock.StoredVM(%q) = false; want materialised", vmName)
	}
	if stored.UserData != "" {
		t.Errorf("mock-agent UserData = %q, want empty (cloud_init_disabled=true must win over template baked)", stored.UserData)
	}
}

// TestVMCreate_CloudInit_DisabledAndUserDataRejected locks in the
// CP-side mutual exclusion validation: sending both `user_data` AND
// `cloud_init_disabled=true` returns 400 validation_failed. The
// DB CHECK chk_vms_cloud_init_disabled_no_userdata is the durable
// backstop; this test asserts the friendly 400 envelope reaches the
// caller before any row insert.
func TestVMCreate_CloudInit_DisabledAndUserDataRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-ci-conflict", 0xb4, "private")

	conflictBody := "#cloud-config\nusers:\n  - name: conflicting\n"
	vmName := "ci-conflict-vm-" + uuid.NewString()[:8]
	status, body := v.createVMRaw(t, ctx, vmCreateBody{
		Name:              vmName,
		Template:          tpl.Name,
		Pool:              v.pool.ID.String(),
		VCPUs:             1,
		MemoryMB:          512,
		UserData:          &conflictBody,
		CloudInitDisabled: true,
	}, v.adminToken, "")
	if status != 400 {
		t.Fatalf("status = %d, body = %s, want 400 validation_failed", status, body)
	}
	if !bytes.Contains(body, []byte("mutually exclusive")) {
		t.Errorf("body missing 'mutually exclusive' phrase: %s", body)
	}
	// VM row must not exist — handler rejected before insert.
	if _, ok := v.mock.StoredVM(vmName); ok {
		t.Errorf("mock-agent unexpectedly observed VM %q after rejection", vmName)
	}
}
