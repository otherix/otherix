// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The methods in this file are the template domain entrypoints on
// *Store: they wrap the raw sqlc-generated *Queries primitives,
// translate driver errors into store sentinels, and assemble the
// transactional delete. Handlers depend on these, not on *Queries.

// Template-domain sentinel errors. They translate the template write
// constraints into stable values handlers match with errors.Is, so the
// conflict policy is expressed in domain terms rather than constraint
// names.
var (
	// ErrTemplateNameExists is returned when a template with the same
	// name already exists (uq_templates_name). Name uniqueness is
	// global, not per-owner.
	ErrTemplateNameExists = errors.New("store: template name already in use")
	// ErrTemplateFirmwareNotFound is returned when firmware_id does not
	// reference an existing firmware (FK violation on create/update).
	ErrTemplateFirmwareNotFound = errors.New("store: template firmware does not exist")
)

// translateTemplateWrite maps the template insert/update constraint
// violations to their domain sentinels; any other error passes through
// unchanged. The FK violation can only be the firmware_id reference -
// owner_id is server-assigned from an authenticated principal.
func translateTemplateWrite(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			if pgErr.ConstraintName == "uq_templates_name" {
				return ErrTemplateNameExists
			}
		case "23503":
			return ErrTemplateFirmwareNotFound
		}
	}
	return err
}

// CreateTemplate inserts a template, translating the name-uniqueness and
// firmware FK violations to their domain sentinels.
func (s *Store) CreateTemplate(ctx context.Context, arg CreateTemplateParams) (Template, error) {
	t, err := s.queries.CreateTemplate(ctx, arg)
	if err != nil {
		return Template{}, translateTemplateWrite(err)
	}
	return t, nil
}

// UpdateTemplate updates a template, translating the name-uniqueness and
// firmware FK violations to their domain sentinels.
func (s *Store) UpdateTemplate(ctx context.Context, arg UpdateTemplateParams) (Template, error) {
	t, err := s.queries.UpdateTemplate(ctx, arg)
	if err != nil {
		return Template{}, translateTemplateWrite(err)
	}
	return t, nil
}

// CloneTemplate copies a source template into a new owned row. A missing
// (or soft-deleted) source surfaces as ErrNotFound; a name collision on
// the new row surfaces as ErrTemplateNameExists.
func (s *Store) CloneTemplate(ctx context.Context, arg CloneTemplateParams) (Template, error) {
	t, err := s.queries.CloneTemplate(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Template{}, ErrNotFound
		}
		return Template{}, translateTemplateWrite(err)
	}
	return t, nil
}

// SetTemplateVisibility flips a template's visibility and returns the
// updated row.
func (s *Store) SetTemplateVisibility(ctx context.Context, arg SetTemplateVisibilityParams) (Template, error) {
	t, err := s.queries.SetTemplateVisibility(ctx, arg)
	if err != nil {
		return Template{}, translateNoRows(err)
	}
	return t, nil
}

// ListTemplates returns templates matching the cursor-paginated,
// visibility-clamped filters in arg.
func (s *Store) ListTemplates(ctx context.Context, arg ListTemplatesParams) ([]Template, error) {
	return s.queries.ListTemplates(ctx, arg)
}

// DeleteTemplate soft-deletes a template in a single transaction after
// verifying nothing still references it. Returns *ResourceInUseError
// (keys "storage_images" and/or "vms") when materialised images or
// active vms block the delete. Soft-delete bypasses the three FKs that
// reference templates with differing ON DELETE semantics, so this
// precheck is the sole enforcement of "don't remove a template that's
// still in use".
func (s *Store) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	return s.InTx(ctx, func(q *Queries) error {
		vmCount, err := q.CountActiveVMsForTemplate(ctx, id)
		if err != nil {
			return err
		}
		imageCount, err := q.CountStorageImagesByTemplate(ctx, id)
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
			return &ResourceInUseError{Resources: blocking}
		}
		return q.SoftDeleteTemplate(ctx, id)
	})
}
