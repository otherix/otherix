// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// The tests in this file exercise the template domain methods on
// *Store: the error translations (ErrTemplateNameExists /
// ErrTemplateFirmwareNotFound / ErrNotFound) and the transactional
// DeleteTemplate blocking path. The raw sqlc query behaviour is
// covered separately in templates_test.go.

func TestCreateTemplateDuplicateName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	name := uniqueTemplateName("dom-dup")

	if _, err := s.CreateTemplate(ctx, defaultTemplateParams(uuid.New(), owner, name)); err != nil {
		t.Fatalf("first CreateTemplate: %v", err)
	}
	_, err := s.CreateTemplate(ctx, defaultTemplateParams(uuid.New(), owner, name))
	if !errors.Is(err, store.ErrTemplateNameExists) {
		t.Errorf("duplicate name err = %v, want store.ErrTemplateNameExists", err)
	}
}

func TestCreateTemplateUnknownFirmware(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	params := defaultTemplateParams(uuid.New(), owner, uniqueTemplateName("dom-fw"))
	bogus := uuid.New()
	params.FirmwareID = &bogus

	_, err := s.CreateTemplate(ctx, params)
	if !errors.Is(err, store.ErrTemplateFirmwareNotFound) {
		t.Errorf("unknown firmware err = %v, want store.ErrTemplateFirmwareNotFound", err)
	}
}

func TestUpdateTemplateRenameCollision(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	taken := uniqueTemplateName("dom-taken")
	if _, err := s.CreateTemplate(ctx, defaultTemplateParams(uuid.New(), owner, taken)); err != nil {
		t.Fatalf("seed taken: %v", err)
	}
	id := uuid.New()
	row, err := s.CreateTemplate(ctx, defaultTemplateParams(id, owner, uniqueTemplateName("dom-mover")))
	if err != nil {
		t.Fatalf("seed mover: %v", err)
	}

	_, err = s.UpdateTemplate(ctx, store.UpdateTemplateParams{
		ID:                     id,
		Name:                   taken,
		Description:            row.Description,
		OsVariant:              row.OsVariant,
		FirmwareType:           row.FirmwareType,
		FirmwareID:             row.FirmwareID,
		DefaultCpuCores:        row.DefaultCpuCores,
		DefaultMemoryMib:       row.DefaultMemoryMib,
		DefaultDiskGib:         row.DefaultDiskGib,
		CloudInitSupported:     row.CloudInitSupported,
		CloudInitUserData:      row.CloudInitUserData,
		QemuGuestAgentExpected: row.QemuGuestAgentExpected,
		Metadata:               row.Metadata,
	})
	if !errors.Is(err, store.ErrTemplateNameExists) {
		t.Errorf("rename collision err = %v, want store.ErrTemplateNameExists", err)
	}
}

func TestCloneTemplateMissingSource(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	cloner := seedUser(t, ctx, s, "developer")
	_, err := s.CloneTemplate(ctx, store.CloneTemplateParams{
		NewID:      uuid.New(),
		NewOwnerID: cloner,
		NewName:    uniqueTemplateName("dom-ghost"),
		SourceID:   uuid.New(),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("clone of missing source err = %v, want store.ErrNotFound", err)
	}
}

func TestCloneTemplateNameCollision(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	srcID := uuid.New()
	if _, err := s.CreateTemplate(ctx, defaultTemplateParams(srcID, owner, uniqueTemplateName("dom-src"))); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	taken := uniqueTemplateName("dom-clone-taken")
	if _, err := s.CreateTemplate(ctx, defaultTemplateParams(uuid.New(), owner, taken)); err != nil {
		t.Fatalf("seed taken: %v", err)
	}

	_, err := s.CloneTemplate(ctx, store.CloneTemplateParams{
		NewID:      uuid.New(),
		NewOwnerID: owner,
		NewName:    taken,
		SourceID:   srcID,
	})
	if !errors.Is(err, store.ErrTemplateNameExists) {
		t.Errorf("clone name collision err = %v, want store.ErrTemplateNameExists", err)
	}
}

func TestDeleteTemplateSucceeds(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	id := uuid.New()
	if _, err := s.CreateTemplate(ctx, defaultTemplateParams(id, owner, uniqueTemplateName("dom-del"))); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	if err := s.DeleteTemplate(ctx, id); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := s.Queries().GetTemplate(ctx, id); err == nil {
		t.Errorf("GetTemplate after delete = nil error, want not-found")
	}
}

func TestDeleteTemplateBlockedByVM(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	id := uuid.New()
	if _, err := s.CreateTemplate(ctx, defaultTemplateParams(id, owner, uniqueTemplateName("dom-del-vm"))); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if err := insertActiveVM(ctx, s, owner, &id, uniqueTemplateName("dom-vm")); err != nil {
		t.Fatalf("seed active vm: %v", err)
	}

	err := s.DeleteTemplate(ctx, id)
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		t.Fatalf("DeleteTemplate err = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vms"] != 1 {
		t.Errorf("blocking vms = %d, want 1", blocking.Resources["vms"])
	}
	if _, ok := blocking.Resources["storage_images"]; ok {
		t.Errorf("blocking unexpectedly lists storage_images: %v", blocking.Resources)
	}
}

func TestNodeByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.NodeByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeByID(absent) err = %v, want store.ErrNotFound", err)
	}
}

func TestNodeByIDFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := seedNodeForPools(t, ctx, s)
	got, err := s.NodeByID(ctx, id)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.ID != id {
		t.Errorf("NodeByID().ID = %v, want %v", got.ID, id)
	}
}
