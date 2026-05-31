// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

func tplParams(name string, owner uuid.UUID) store.CreateTemplateParams {
	return store.CreateTemplateParams{
		ID:               uuid.New(),
		OwnerID:          owner,
		Name:             name,
		Description:      "test template",
		Architecture:     store.CpuArchAmd64,
		OsFamily:         store.OsFamilyLinux,
		OsVariant:        "noble",
		ImageUrl:         "https://example.test/img.qcow2",
		ImageFormat:      store.ImageFormatQcow2,
		FirmwareType:     store.FirmwareTypeUefi,
		DefaultCpuCores:  2,
		DefaultMemoryMib: 2048,
		DefaultDiskGib:   20,
	}
}

func uniqueTplName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestTemplateCreateGetByIDAndName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := uuid.New()
	p := tplParams(uniqueTplName("tpl"), owner)
	created, err := s.CreateTemplate(ctx, p)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if created.Visibility != "private" || created.CreatedAt.IsZero() {
		t.Errorf("CreateTemplate = %+v, want visibility=private + created_at", created)
	}
	byID, err := s.TemplateByID(ctx, p.ID)
	if err != nil || byID.Name != p.Name {
		t.Fatalf("TemplateByID = (%+v, %v)", byID, err)
	}
	byName, err := s.TemplateByName(ctx, strings.ToUpper(p.Name))
	if err != nil || byName.ID != p.ID {
		t.Fatalf("TemplateByName(upper) = (%+v, %v)", byName, err)
	}
}

func TestTemplateFirmwareFK(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	missing := uuid.New()
	p := tplParams(uniqueTplName("fk"), uuid.New())
	p.FirmwareID = &missing
	if _, err := s.CreateTemplate(ctx, p); !errors.Is(err, store.ErrTemplateFirmwareNotFound) {
		t.Fatalf("CreateTemplate(bad fw) = %v, want store.ErrTemplateFirmwareNotFound", err)
	}
	// With a real firmware it succeeds and the firmware index blocks fw delete.
	fw := fwParams(uniqueFwName("tplfw"), store.CpuArchAmd64, false)
	if _, err := s.CreateFirmware(ctx, fw); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	p2 := tplParams(uniqueTplName("fk2"), uuid.New())
	p2.FirmwareID = &fw.ID
	if _, err := s.CreateTemplate(ctx, p2); err != nil {
		t.Fatalf("CreateTemplate(good fw): %v", err)
	}
	var blocking *store.ResourceInUseError
	if err := s.DeleteFirmware(ctx, fw.ID); !errors.As(err, &blocking) || blocking.Resources["templates"] != 1 {
		t.Errorf("DeleteFirmware blocked = %v, want templates=1", err)
	}
}

func TestTemplateNameUniqueness(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueTplName("dup")
	if _, err := s.CreateTemplate(ctx, tplParams(name, uuid.New())); err != nil {
		t.Fatalf("first CreateTemplate: %v", err)
	}
	if _, err := s.CreateTemplate(ctx, tplParams(strings.ToUpper(name), uuid.New())); !errors.Is(err, store.ErrTemplateNameExists) {
		t.Errorf("dup name = %v, want store.ErrTemplateNameExists", err)
	}
}

func TestTemplateCloneAndSetVisibility(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	src := tplParams(uniqueTplName("src"), uuid.New())
	if _, err := s.CreateTemplate(ctx, src); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	newOwner := uuid.New()
	desc := "cloned"
	clone, err := s.CloneTemplate(ctx, store.CloneTemplateParams{
		NewID: uuid.New(), NewOwnerID: newOwner, NewName: uniqueTplName("clone"), NewDescription: &desc, SourceID: src.ID,
	})
	if err != nil {
		t.Fatalf("CloneTemplate: %v", err)
	}
	if clone.OwnerID != newOwner || clone.Visibility != "private" || clone.Description != "cloned" || clone.OsVariant != src.OsVariant {
		t.Errorf("CloneTemplate = %+v, want new owner/private/desc/copied os_variant", clone)
	}
	// Clone of a missing source is not found.
	if _, err := s.CloneTemplate(ctx, store.CloneTemplateParams{NewID: uuid.New(), NewOwnerID: newOwner, NewName: uniqueTplName("x"), SourceID: uuid.New()}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CloneTemplate(missing src) = %v, want store.ErrNotFound", err)
	}
	// Publish.
	pub, err := s.SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{ID: clone.ID, Visibility: "public"})
	if err != nil {
		t.Fatalf("SetTemplateVisibility: %v", err)
	}
	if pub.Visibility != "public" || !pub.UpdatedAt.After(clone.UpdatedAt) {
		t.Errorf("SetTemplateVisibility = %+v, want public + updated_at bumped", pub)
	}
}

func TestTemplateListVisibilityClamp(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := uuid.New()
	other := uuid.New()
	// owner's private template
	mine := tplParams(uniqueTplName("mine"), owner)
	if _, err := s.CreateTemplate(ctx, mine); err != nil {
		t.Fatalf("seed mine: %v", err)
	}
	// other's template, published
	pub := tplParams(uniqueTplName("pub"), other)
	if _, err := s.CreateTemplate(ctx, pub); err != nil {
		t.Fatalf("seed pub: %v", err)
	}
	if _, err := s.SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{ID: pub.ID, Visibility: "public"}); err != nil {
		t.Fatalf("publish pub: %v", err)
	}
	// other's private template
	hidden := tplParams(uniqueTplName("hidden"), other)
	if _, err := s.CreateTemplate(ctx, hidden); err != nil {
		t.Fatalf("seed hidden: %v", err)
	}

	// Developer (owner): sees own private + public, not other's private.
	dev, err := s.ListTemplates(ctx, store.ListTemplatesParams{CallerCanSeeAny: false, CallerID: &owner, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTemplates(dev): %v", err)
	}
	got := idset(dev)
	if !got[mine.ID] || !got[pub.ID] || got[hidden.ID] {
		t.Errorf("developer view = %v, want mine+pub, not hidden", got)
	}
	// Viewer (nil caller): public only.
	viewer, err := s.ListTemplates(ctx, store.ListTemplatesParams{CallerCanSeeAny: false, CallerID: nil, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTemplates(viewer): %v", err)
	}
	gv := idset(viewer)
	if gv[mine.ID] || !gv[pub.ID] || gv[hidden.ID] {
		t.Errorf("viewer view = %v, want pub only", gv)
	}
	// Admin: everything.
	admin, err := s.ListTemplates(ctx, store.ListTemplatesParams{CallerCanSeeAny: true, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTemplates(admin): %v", err)
	}
	ga := idset(admin)
	if !ga[mine.ID] || !ga[pub.ID] || !ga[hidden.ID] {
		t.Errorf("admin view = %v, want all three", ga)
	}
}

func TestTemplateUpdateRenameAndDelete(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := tplParams(uniqueTplName("upd"), uuid.New())
	if _, err := s.CreateTemplate(ctx, p); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	newName := uniqueTplName("upd-renamed")
	updated, err := s.UpdateTemplate(ctx, store.UpdateTemplateParams{
		ID: p.ID, Name: newName, Description: "changed", OsVariant: "jammy",
		FirmwareType: store.FirmwareTypeUefi, DefaultCpuCores: 4, DefaultMemoryMib: 4096, DefaultDiskGib: 40,
	})
	if err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if updated.Name != newName || updated.OsVariant != "jammy" || updated.DefaultCpuCores != 4 {
		t.Errorf("UpdateTemplate = %+v", updated)
	}
	// Old name reusable.
	if _, err := s.CreateTemplate(ctx, tplParams(p.Name, uuid.New())); err != nil {
		t.Errorf("old name not reusable: %v", err)
	}
	// Delete blocked by an active vm reference.
	vmKey := etcd.Key("index", "vms", "template", p.ID.String(), uuid.NewString())
	if err := cli.Put(ctx, vmKey, []byte("vm")); err != nil {
		t.Fatalf("seed vm index: %v", err)
	}
	var blocking *store.ResourceInUseError
	if err := s.DeleteTemplate(ctx, p.ID); !errors.As(err, &blocking) || blocking.Resources["vms"] != 1 {
		t.Fatalf("DeleteTemplate blocked = %v, want vms=1", err)
	}
	if _, err := cli.Delete(ctx, vmKey); err != nil {
		t.Fatalf("clear vm index: %v", err)
	}
	if err := s.DeleteTemplate(ctx, p.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := s.TemplateByID(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TemplateByID after delete = %v, want store.ErrNotFound", err)
	}
}

func idset(ts []store.Template) map[uuid.UUID]bool {
	m := make(map[uuid.UUID]bool, len(ts))
	for _, t := range ts {
		m[t.ID] = true
	}
	return m
}
