// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Templates are user-owned, cluster-wide, addressed by UUID with a global
// case-insensitive name guard (uq_templates_name, partial on deleted_at). An
// owner index (index/templates/owner) backs CountUserResources and a firmware
// index (index/templates/firmware) backs DeleteFirmware's blocking count.
// Visibility is private on create and moves only through SetTemplateVisibility.
// Delete is soft and blocked by materialised images or active vms.

func templateKey(id uuid.UUID) string { return etcd.Key("templates", id.String()) }

func templatePrefix() string { return etcd.Key("templates") + "/" }

func templateNameGuard(name string) string {
	return etcd.Key("uniq", "templates", "name", strings.ToLower(name))
}

func templateOwnerIndexKey(ownerID, id uuid.UUID) string {
	return etcd.Key("index", "templates", "owner", ownerID.String(), id.String())
}

func templateFirmwareIndexKey(firmwareID, id uuid.UUID) string {
	return etcd.Key("index", "templates", "firmware", firmwareID.String(), id.String())
}

// storageImagesTemplateIndexPrefix and vmsTemplateIndexPrefix list the
// dependants that block a template delete (maintained by the storage-pools and
// vms slices).
func storageImagesTemplateIndexPrefix(id uuid.UUID) string {
	return etcd.Key("index", "storage_images", "template", id.String()) + "/"
}

func vmsTemplateIndexPrefix(id uuid.UUID) string {
	return etcd.Key("index", "vms", "template", id.String()) + "/"
}

// TemplateByID returns the non-deleted template with the given id, or
// store.ErrNotFound.
func (s *Store) TemplateByID(ctx context.Context, id uuid.UUID) (store.Template, error) {
	var t store.Template
	found, err := s.c.GetJSON(ctx, templateKey(id), &t)
	if err != nil {
		return store.Template{}, err
	}
	if !found || t.DeletedAt != nil {
		return store.Template{}, store.ErrNotFound
	}
	return t, nil
}

// TemplateByName returns the non-deleted template with the given name
// (case-insensitive) via the name guard, or store.ErrNotFound.
func (s *Store) TemplateByName(ctx context.Context, name string) (store.Template, error) {
	id, found, err := s.resolveGuard(ctx, templateNameGuard(name))
	if err != nil {
		return store.Template{}, err
	}
	if !found {
		return store.Template{}, store.ErrNotFound
	}
	return s.TemplateByID(ctx, id)
}

// CreateTemplate inserts a template (visibility forced to private), validating
// the firmware reference, and writes the primary + name guard + owner index
// (+ firmware index) atomically. Returns store.ErrTemplateNameExists or
// store.ErrTemplateFirmwareNotFound on the respective conflict.
func (s *Store) CreateTemplate(ctx context.Context, arg store.CreateTemplateParams) (store.Template, error) {
	if err := s.requireFirmware(ctx, arg.FirmwareID); err != nil {
		return store.Template{}, err
	}
	now := time.Now().UTC()
	t := store.Template{
		ID:                     arg.ID,
		OwnerID:                arg.OwnerID,
		Name:                   arg.Name,
		Description:            arg.Description,
		Visibility:             "private",
		Architecture:           arg.Architecture,
		OsFamily:               arg.OsFamily,
		OsVariant:              arg.OsVariant,
		ImageUrl:               arg.ImageUrl,
		ImageChecksumSha256:    arg.ImageChecksumSha256,
		ImageFormat:            arg.ImageFormat,
		ImageSizeBytes:         arg.ImageSizeBytes,
		FirmwareType:           arg.FirmwareType,
		FirmwareID:             arg.FirmwareID,
		DefaultCpuCores:        arg.DefaultCpuCores,
		DefaultMemoryMib:       arg.DefaultMemoryMib,
		DefaultDiskGib:         arg.DefaultDiskGib,
		CloudInitSupported:     arg.CloudInitSupported,
		CloudInitUserData:      arg.CloudInitUserData,
		QemuGuestAgentExpected: arg.QemuGuestAgentExpected,
		Metadata:               arg.Metadata,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	return s.putNewTemplate(ctx, t)
}

// CloneTemplate copies a live source template into a new owned, private row
// with a new id/name. Returns store.ErrNotFound when the source is missing or
// soft-deleted, or store.ErrTemplateNameExists on a name collision.
func (s *Store) CloneTemplate(ctx context.Context, arg store.CloneTemplateParams) (store.Template, error) {
	src, err := s.TemplateByID(ctx, arg.SourceID)
	if err != nil {
		return store.Template{}, err
	}
	now := time.Now().UTC()
	t := src
	t.ID = arg.NewID
	t.OwnerID = arg.NewOwnerID
	t.Name = arg.NewName
	t.Visibility = "private"
	t.DerivedVmCount = 0
	t.CreatedAt = now
	t.UpdatedAt = now
	t.DeletedAt = nil
	if arg.NewDescription != nil {
		t.Description = *arg.NewDescription
	}
	return s.putNewTemplate(ctx, t)
}

// putNewTemplate writes a fresh template row with its name guard + owner index
// (+ firmware index) atomically, mapping a guard conflict to
// store.ErrTemplateNameExists.
func (s *Store) putNewTemplate(ctx context.Context, t store.Template) (store.Template, error) {
	val, err := etcd.Marshal(t)
	if err != nil {
		return store.Template{}, err
	}
	guard := templateNameGuard(t.Name)
	ops := []clientv3.Op{
		clientv3.OpPut(guard, t.ID.String()),
		clientv3.OpPut(templateKey(t.ID), string(val)),
		clientv3.OpPut(templateOwnerIndexKey(t.OwnerID, t.ID), t.ID.String()),
	}
	if t.FirmwareID != nil {
		ops = append(ops, clientv3.OpPut(templateFirmwareIndexKey(*t.FirmwareID, t.ID), t.ID.String()))
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.Template{}, fmt.Errorf("create template txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Template{}, store.ErrTemplateNameExists
	}
	return t, nil
}

// UpdateTemplate rewrites the mutable fields, validating the firmware
// reference, moving the name guard / firmware index as needed, and bumping
// updated_at. Identity / image-source fields are immutable. Returns
// store.ErrNotFound, store.ErrTemplateNameExists, or
// store.ErrTemplateFirmwareNotFound.
func (s *Store) UpdateTemplate(ctx context.Context, arg store.UpdateTemplateParams) (store.Template, error) {
	existing, err := s.TemplateByID(ctx, arg.ID)
	if err != nil {
		return store.Template{}, err
	}
	if err := s.requireFirmware(ctx, arg.FirmwareID); err != nil {
		return store.Template{}, err
	}
	updated := existing
	updated.Name = arg.Name
	updated.Description = arg.Description
	updated.OsVariant = arg.OsVariant
	updated.FirmwareType = arg.FirmwareType
	updated.FirmwareID = arg.FirmwareID
	updated.DefaultCpuCores = arg.DefaultCpuCores
	updated.DefaultMemoryMib = arg.DefaultMemoryMib
	updated.DefaultDiskGib = arg.DefaultDiskGib
	updated.CloudInitSupported = arg.CloudInitSupported
	updated.CloudInitUserData = arg.CloudInitUserData
	updated.QemuGuestAgentExpected = arg.QemuGuestAgentExpected
	updated.Metadata = arg.Metadata
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.Template{}, err
	}

	var conds []clientv3.Cmp
	ops := []clientv3.Op{clientv3.OpPut(templateKey(arg.ID), string(val))}

	oldGuard := templateNameGuard(existing.Name)
	newGuard := templateNameGuard(arg.Name)
	if oldGuard != newGuard {
		conds = append(conds, clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0))
		ops = append(ops, clientv3.OpDelete(oldGuard), clientv3.OpPut(newGuard, arg.ID.String()))
	}
	if !sameFirmware(existing.FirmwareID, arg.FirmwareID) {
		if existing.FirmwareID != nil {
			ops = append(ops, clientv3.OpDelete(templateFirmwareIndexKey(*existing.FirmwareID, arg.ID)))
		}
		if arg.FirmwareID != nil {
			ops = append(ops, clientv3.OpPut(templateFirmwareIndexKey(*arg.FirmwareID, arg.ID), arg.ID.String()))
		}
	}

	if len(conds) == 0 {
		if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
			return store.Template{}, fmt.Errorf("update template txn: %v", err)
		}
		return updated, nil
	}
	resp, err := s.c.Raw().Txn(ctx).If(conds...).Then(ops...).Commit()
	if err != nil {
		return store.Template{}, fmt.Errorf("update template txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Template{}, store.ErrTemplateNameExists
	}
	return updated, nil
}

// SetTemplateVisibility flips a template's visibility, bumping updated_at.
// Owner does not move. Returns store.ErrNotFound when the row is missing.
func (s *Store) SetTemplateVisibility(ctx context.Context, arg store.SetTemplateVisibilityParams) (store.Template, error) {
	existing, err := s.TemplateByID(ctx, arg.ID)
	if err != nil {
		return store.Template{}, err
	}
	existing.Visibility = arg.Visibility
	existing.UpdatedAt = time.Now().UTC()
	if err := s.c.PutJSON(ctx, templateKey(arg.ID), existing); err != nil {
		return store.Template{}, err
	}
	return existing, nil
}

// ListTemplates returns the templates visible to the caller (per the
// caller_can_see_any / caller_id clamp) matching the optional filters, ordered
// by (created_at, id) ascending, after the cursor, capped at LimitCount.
func (s *Store) ListTemplates(ctx context.Context, arg store.ListTemplatesParams) ([]store.Template, error) {
	items, err := s.c.Range(ctx, templatePrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.Template, 0, len(items))
	for _, kv := range items {
		var t store.Template
		if err := json.Unmarshal(kv.Value, &t); err != nil {
			return nil, fmt.Errorf("unmarshal template %q: %v", kv.Key, err)
		}
		if t.DeletedAt != nil || !templateMatchesList(t, arg) {
			continue
		}
		out = append(out, t)
	}
	sortByCreatedAtID(out, func(t store.Template) (time.Time, uuid.UUID) { return t.CreatedAt, t.ID })
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// DeleteTemplate soft-deletes a template after verifying nothing references it.
// Returns store.ErrNotFound when missing, or *store.ResourceInUseError (keys
// "storage_images" / "vms") when materialised images or active vms block it.
func (s *Store) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	t, err := s.TemplateByID(ctx, id)
	if err != nil {
		return err
	}
	imageCount, err := s.countPrefix(ctx, storageImagesTemplateIndexPrefix(id))
	if err != nil {
		return err
	}
	vmCount, err := s.countPrefix(ctx, vmsTemplateIndexPrefix(id))
	if err != nil {
		return err
	}
	if imageCount > 0 || vmCount > 0 {
		blocking := map[string]int64{}
		if imageCount > 0 {
			blocking["storage_images"] = imageCount
		}
		if vmCount > 0 {
			blocking["vms"] = vmCount
		}
		return &store.ResourceInUseError{Resources: blocking}
	}
	now := time.Now().UTC()
	t.DeletedAt = &now
	t.UpdatedAt = now
	val, err := etcd.Marshal(t)
	if err != nil {
		return err
	}
	ops := []clientv3.Op{
		clientv3.OpPut(templateKey(id), string(val)),
		clientv3.OpDelete(templateNameGuard(t.Name)),
		clientv3.OpDelete(templateOwnerIndexKey(t.OwnerID, id)),
	}
	if t.FirmwareID != nil {
		ops = append(ops, clientv3.OpDelete(templateFirmwareIndexKey(*t.FirmwareID, id)))
	}
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("delete template txn: %v", err)
	}
	return nil
}

// requireFirmware verifies a non-nil firmware reference exists, returning
// store.ErrTemplateFirmwareNotFound when it does not (the FK-violation parity).
func (s *Store) requireFirmware(ctx context.Context, firmwareID *uuid.UUID) error {
	if firmwareID == nil {
		return nil
	}
	if _, err := s.FirmwareByID(ctx, *firmwareID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrTemplateFirmwareNotFound
		}
		return err
	}
	return nil
}

// templateMatchesList reports whether a template is visible to the caller and
// passes every optional list filter and the cursor bound.
func templateMatchesList(t store.Template, arg store.ListTemplatesParams) bool {
	if !templateVisibleTo(t, arg) {
		return false
	}
	if arg.OwnerIDFilter != nil && t.OwnerID != *arg.OwnerIDFilter {
		return false
	}
	if arg.VisibilityFilter != nil && t.Visibility != *arg.VisibilityFilter {
		return false
	}
	if arg.ArchitectureFilter != nil && t.Architecture != *arg.ArchitectureFilter {
		return false
	}
	if arg.OsFamilyFilter != nil && t.OsFamily != *arg.OsFamilyFilter {
		return false
	}
	return afterCursor(t.CreatedAt, t.ID, arg.CursorCreatedAt, arg.CursorID)
}

// templateVisibleTo applies the visibility clamp: admin/operator see all,
// developer sees own + public, viewer sees public only.
func templateVisibleTo(t store.Template, arg store.ListTemplatesParams) bool {
	if arg.CallerCanSeeAny {
		return true
	}
	if arg.CallerID != nil && t.OwnerID == *arg.CallerID {
		return true
	}
	return t.Visibility == "public"
}

// sameFirmware reports whether two optional firmware references are equal.
func sameFirmware(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
